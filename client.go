package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseRead caps how much of an ES response we buffer in memory.
const maxResponseRead = 64 << 20 // 64 MiB

// ESClient is a small REST client for the Elasticsearch 6.x HTTP API.
//
// It deliberately avoids an official client library: the 6.x API is plain
// JSON over HTTP and a hand written client keeps the dependency surface (and
// the resulting binary) tiny.
type ESClient struct {
	base   string
	http   *http.Client
	cfg    Config
	authed bool
}

func NewESClient(cfg Config) (*ESClient, error) {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: cfg.Insecure}, // #nosec G402 - opt-in via ES_INSECURE
	}
	return &ESClient{
		base:   strings.TrimRight(cfg.ESURL, "/"),
		http:   &http.Client{Timeout: cfg.Timeout, Transport: tr},
		cfg:    cfg,
		authed: cfg.Username != "" || cfg.APIKey != "",
	}, nil
}

// Do performs a request against the cluster and returns the raw body.
//
// path is relative to the cluster root (e.g. "my-index/_search"), it must
// already be URL escaped. params may be nil.
func (c *ESClient) Do(ctx context.Context, method, path string, params url.Values, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, params), reader)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	switch {
	case c.cfg.Username != "":
		req.SetBasicAuth(c.cfg.Username, c.cfg.Password)
	case c.cfg.APIKey != "":
		req.Header.Set("Authorization", "ApiKey "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request %s %s failed: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseRead))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return data, resp.StatusCode, &ESError{Method: method, Path: path, Status: resp.StatusCode, StatusText: resp.Status, Body: data}
	}
	return data, resp.StatusCode, nil
}

// DoJSON performs a request and decodes the JSON response into out.
func (c *ESClient) DoJSON(ctx context.Context, method, path string, params url.Values, body, out any) error {
	data, _, err := c.Do(ctx, method, path, params, body)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response from %s %s: %w", method, path, err)
	}
	return nil
}

func (c *ESClient) endpoint(path string, params url.Values) string {
	p := strings.TrimLeft(strings.TrimSpace(path), "/")
	u := c.base
	if p != "" {
		u += "/" + p
	}
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	return u
}

// ESError wraps a non-2xx response and turns the usual ES error envelope into
// an actionable message for the model.
type ESError struct {
	Method     string
	Path       string
	Status     int
	StatusText string
	Body       []byte
}

type esErrorEnvelope struct {
	Error  json.RawMessage `json:"error"`
	Status int             `json:"status"`
}

type esErrorDetail struct {
	Type      string `json:"type"`
	Reason    string `json:"reason"`
	Index     string `json:"index"`
	RootCause []struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
		Index  string `json:"index"`
	} `json:"root_cause"`
}

func (e *ESError) Error() string {
	detail := parseESErrorDetail(e.Body)

	var b strings.Builder
	fmt.Fprintf(&b, "elasticsearch %s (%s %s)", e.StatusText, e.Method, e.Path)
	if detail.Type != "" || detail.Reason != "" {
		fmt.Fprintf(&b, ": %s: %s", detail.Type, detail.Reason)
	}
	if len(detail.RootCause) > 0 {
		cause := detail.RootCause[0]
		if cause.Type != "" || cause.Reason != "" {
			fmt.Fprintf(&b, " [root_cause %s: %s]", cause.Type, cause.Reason)
		}
	}
	if detail.Type == "" && len(detail.RootCause) == 0 {
		if snippet := strings.TrimSpace(string(e.Body)); snippet != "" {
			b.WriteString(": ")
			b.WriteString(clipSnippet(snippet, 400))
		}
	}
	if hint := e.hint(detail); hint != "" {
		b.WriteString("\nhint: ")
		b.WriteString(hint)
	}
	return b.String()
}

func (e *ESError) hint(detail esErrorDetail) string {
	parts := []string{detail.Type, detail.Reason}
	for _, cause := range detail.RootCause {
		parts = append(parts, cause.Type, cause.Reason)
	}
	lower := strings.ToLower(strings.Join(parts, " "))

	switch {
	case strings.Contains(lower, "security_exception"), strings.Contains(lower, "authentication"):
		return "check ES_USERNAME/ES_PASSWORD or ES_API_KEY, and that the account can read the target indices"
	case strings.Contains(lower, "index_not_found_exception"):
		return "call es_list_indices with a pattern (e.g. pattern=\"filebeat-*\") to see which indices exist"
	case strings.Contains(lower, "no mapping found"), strings.Contains(lower, "unmapped"):
		return "the field does not exist in these indices; call es_get_fields (pattern=\"<field>*\") to find the real field name, or change time_field"
	case strings.Contains(lower, "fielddata is disabled"), strings.Contains(lower, "fielddata=true"):
		return "text fields cannot be aggregated; use the keyword subfield (e.g. \"caller.raw\") shown by es_get_fields where aggregatable=true"
	case strings.Contains(lower, "unrecognized parameter"), strings.Contains(lower, "unknown parameter"):
		return "this endpoint does not accept that parameter on ES 6.x; drop it or check the ES 6.2 REST API reference"
	case strings.Contains(lower, "parsing_exception"), strings.Contains(lower, "query_shard_exception"), strings.Contains(lower, "illegal_argument_exception"):
		return "the query DSL was rejected by ES 6.x; call es_get_fields first and keep the query body valid for Elasticsearch 6 (no 7.x/8.x syntax such as typeless APIs in the body)"
	case strings.Contains(lower, "too_many_clauses"), strings.Contains(lower, "circuit_breaking"):
		return "narrow the query: shrink the time range (since), add index filters, or reduce wildcard terms"
	}
	return ""
}

func parseESErrorDetail(body []byte) esErrorDetail {
	var env esErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Error == nil {
		return esErrorDetail{}
	}
	var detail esErrorDetail
	if err := json.Unmarshal(env.Error, &detail); err == nil {
		if detail.Type != "" || detail.Reason != "" || len(detail.RootCause) > 0 {
			return detail
		}
	}
	var text string
	if err := json.Unmarshal(env.Error, &text); err == nil {
		detail.Reason = text
	}
	return detail
}

func clipSnippet(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(clipped)"
}
