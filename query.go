package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/mark3labs/mcp-go/mcp"
)

// splitList splits a comma separated argument, trimming spaces and dropping
// empty entries.
func splitList(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// objArg returns a nested JSON object argument (query, aggregations, params).
func objArg(req mcp.CallToolRequest, key string) (map[string]any, bool) {
	raw, ok := req.GetArguments()[key]
	if !ok || raw == nil {
		return nil, false
	}
	obj, ok := raw.(map[string]any)
	if !ok || len(obj) == 0 {
		return nil, false
	}
	return obj, true
}

var relativeTimeRe = regexp.MustCompile(`(?i)^(\d+)\s*([a-z]+)$`)

var timeUnitAliases = map[string]string{
	"s": "s", "sec": "s", "secs": "s", "second": "s", "seconds": "s",
	"m": "m", "min": "m", "mins": "m", "minute": "m", "minutes": "m",
	"h": "h", "hr": "h", "hrs": "h", "hour": "h", "hours": "h",
	"d": "d", "day": "d", "days": "d",
	"w": "w", "week": "w", "weeks": "w",
	"mon": "M", "month": "M", "months": "M",
	"y": "y", "year": "y", "years": "y",
}

// normalizeTimeArg converts human friendly relative times ("15m", "24 hours")
// into ES date math ("now-15m") and fixes "YYYY-MM-DD HH:MM:SS" into ISO 8601.
// Anything else (date math like "now-1h/d", ISO timestamps) is passed through.
func normalizeTimeArg(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if m := relativeTimeRe.FindStringSubmatch(s); m != nil {
		if unit, ok := timeUnitAliases[strings.ToLower(m[2])]; ok {
			return "now-" + m[1] + unit
		}
	}
	if len(s) >= 19 && s[4] == '-' && s[10] == ' ' {
		return s[:10] + "T" + s[11:]
	}
	return s
}

func isAllTime(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "all", "any", "none", "-", "0":
		return true
	}
	return false
}

// parseSortSpec turns "@timestamp:desc,level:asc" into an ES sort array.
// "none" or an empty string disables sorting (the client then returns ES's
// default _score order).
func parseSortSpec(spec string) []any {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.EqualFold(spec, "none") {
		return nil
	}
	out := make([]any, 0, 2)
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		field, order := part, "asc"
		if i := strings.LastIndex(part, ":"); i > 0 {
			field = strings.TrimSpace(part[:i])
			order = strings.ToLower(strings.TrimSpace(part[i+1:]))
		}
		if order != "asc" && order != "desc" {
			order = "asc"
		}
		if field == "" {
			continue
		}
		if field == "_score" || field == "_doc" {
			out = append(out, map[string]any{field: map[string]any{"order": order}})
			continue
		}
		// unmapped_type keeps ES 6 happy when the field is missing from one of
		// the matching indices.
		out = append(out, map[string]any{field: map[string]any{"order": order, "unmapped_type": "keyword"}})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sanitizeIndexPattern validates an index pattern and keeps it safe to embed
// in a URL path. ES indices may only contain letters, digits, "-", "_", ".",
// and patterns additionally use "*", "+", ",", ":".
func sanitizeIndexPattern(index string) (string, error) {
	index = strings.TrimSpace(index)
	index = strings.TrimPrefix(index, "/")
	if index == "" {
		return "*", nil
	}
	if index == "." || index == ".." || strings.Contains(index, "..") {
		return "", fmt.Errorf("invalid index pattern %q", index)
	}
	for _, r := range index {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		switch r {
		case '-', '_', '.', '*', '+', ',', ':':
		default:
			return "", fmt.Errorf("invalid character %q in index pattern %q (allowed: letters, digits, - _ . * + , :)", r, index)
		}
	}
	return index, nil
}

// sanitizeRawPath validates a caller supplied REST path: it must be relative
// to the cluster root, must not traverse upwards, and each segment is escaped.
func sanitizeRawPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", fmt.Errorf("path is required, e.g. \"_cluster/health\" or \"filebeat-*/_search\"")
	}
	if strings.Contains(p, "://") || strings.Contains(p, "\\") {
		return "", fmt.Errorf("path must be relative to the cluster root, got %q", p)
	}
	if strings.Contains(p, "..") {
		return "", fmt.Errorf("path must not contain '..'")
	}
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		if seg == "" {
			return "", fmt.Errorf("path must not contain empty segments, got %q", p)
		}
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/"), nil
}

var readOnlyPostMarkers = []string{
	"_search", "_count", "_msearch", "_mget", "_field_caps", "_validate",
	"_explain", "_analyze", "_render/template", "_search/template", "_cat",
}

// isReadOnlyRequest reports whether method+path only reads data. In read-only
// mode (the default) everything else is rejected before it reaches the cluster.
func isReadOnlyRequest(method, path string) bool {
	switch method {
	case "GET", "HEAD":
		return true
	case "POST":
	default:
		return false
	}
	lower := strings.ToLower(path)
	for _, marker := range readOnlyPostMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// buildQuery assembles the ES query DSL from the shared arguments used by
// es_search, es_count and es_aggregate.
func (c *ESClient) buildQuery(req mcp.CallToolRequest) (map[string]any, error) {
	var must []any
	if base, ok := objArg(req, "query"); ok {
		must = append(must, base)
	}
	if qs := strings.TrimSpace(req.GetString("query_string", "")); qs != "" {
		must = append(must, map[string]any{
			"query_string": map[string]any{
				"query":            qs,
				"lenient":          true,
				"analyze_wildcard": true,
				"default_operator": "and",
			},
		})
	}

	var filter []any
	since := req.GetString("since", c.cfg.DefaultSince)
	until := req.GetString("until", "now")
	timeField := strings.TrimSpace(req.GetString("time_field", "@timestamp"))
	if !isAllTime(since) {
		if timeField == "" {
			return nil, fmt.Errorf("time_field must not be empty when since is set")
		}
		filter = append(filter, map[string]any{
			"range": map[string]any{
				timeField: map[string]any{
					"gte": normalizeTimeArg(since),
					"lte": normalizeTimeArg(until),
				},
			},
		})
	}

	switch {
	case len(must) == 0 && len(filter) == 0:
		return map[string]any{"match_all": map[string]any{}}, nil
	case len(must) == 1 && len(filter) == 0:
		return must[0].(map[string]any), nil
	case len(must) == 0 && len(filter) == 1:
		return filter[0].(map[string]any), nil
	}

	boolQuery := map[string]any{}
	if len(must) > 0 {
		boolQuery["must"] = must
	}
	if len(filter) > 0 {
		boolQuery["filter"] = filter
	}
	return map[string]any{"bool": boolQuery}, nil
}
