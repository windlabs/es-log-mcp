package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// prettyJSON renders v as indented JSON. The model reads these results, so
// indentation matters more than the few extra bytes it costs.
func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error":"cannot encode result: %s"}`, err)
	}
	return string(b)
}

func clipText(s string, max int) (string, bool) {
	if max <= 0 || len(s) <= max {
		return s, false
	}
	// Make sure we do not split a UTF-8 rune in half.
	cut := max
	for cut > 0 && cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "\n...(truncated)", true
}

// humanizeBytes renders a byte count the way _cat/indices prints sizes.
func humanizeBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// trimStringValues walks a decoded JSON value and truncates overly long
// string values (stack traces, request bodies, ...). It keeps the document
// readable instead of blowing the context window on one field.
func trimStringValues(v any, maxChars, depth int) any {
	if maxChars <= 0 || depth > 12 {
		return v
	}
	switch val := v.(type) {
	case string:
		if len(val) > maxChars {
			return val[:maxChars] + fmt.Sprintf("...(truncated, %d bytes total)", len(val))
		}
		return val
	case []any:
		for i := range val {
			val[i] = trimStringValues(val[i], maxChars, depth+1)
		}
		return val
	case map[string]any:
		for k := range val {
			val[k] = trimStringValues(val[k], maxChars, depth+1)
		}
		return val
	default:
		return v
	}
}

// decodeSource decodes a raw _source document and applies string trimming.
func decodeSource(raw json.RawMessage, maxFieldChars int) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return trimStringValues(v, maxFieldChars, 0)
}

// esSearchResponse is the subset of the _search response this server uses.
// It is written to work with both ES 6.x ("hits.total": number) and ES 7+
// ("hits.total": {"value": n, "relation": "eq"}).
type esSearchResponse struct {
	Took     int  `json:"took"`
	TimedOut bool `json:"timed_out"`
	Hits     struct {
		Total json.RawMessage `json:"total"`
		Hits  []esHit         `json:"hits"`
	} `json:"hits"`
	Aggregations json.RawMessage `json:"aggregations"`
}

type esHit struct {
	Index     string              `json:"_index"`
	Type      string              `json:"_type"`
	ID        string              `json:"_id"`
	Score     *float64            `json:"_score"`
	Source    json.RawMessage     `json:"_source"`
	Fields    map[string]any      `json:"fields"`
	Highlight map[string][]string `json:"highlight"`
	Sort      []any               `json:"sort"`
}

type searchHitOut struct {
	Index     string              `json:"_index"`
	Type      string              `json:"_type,omitempty"`
	ID        string              `json:"_id"`
	Score     *float64            `json:"_score,omitempty"`
	Source    any                 `json:"_source,omitempty"`
	Fields    map[string]any      `json:"fields,omitempty"`
	Highlight map[string][]string `json:"highlight,omitempty"`
	Sort      []any               `json:"sort,omitempty"`
}

type searchResult struct {
	Index        string         `json:"index"`
	TookMS       int            `json:"took_ms"`
	TimedOut     bool           `json:"timed_out,omitempty"`
	Total        any            `json:"total"`
	From         int            `json:"from,omitempty"`
	Returned     int            `json:"returned"`
	Hits         []searchHitOut `json:"hits"`
	Aggregations any            `json:"aggregations,omitempty"`
	Truncated    bool           `json:"truncated,omitempty"`
	Notes        []string       `json:"notes,omitempty"`
}

// parseTotal handles the ES 6.x number and ES 7+ object forms of hits.total.
func parseTotal(raw json.RawMessage) any {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var obj struct {
		Value    int64  `json:"value"`
		Relation string `json:"relation"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Relation != "" && obj.Relation != "eq" {
			return fmt.Sprintf("%d (%s)", obj.Value, obj.Relation)
		}
		return obj.Value
	}
	return nil
}

// buildSearchResult converts a raw _search response into the compact shape
// returned to the model.
func buildSearchResult(index string, data []byte, from int, maxFieldChars int) (searchResult, error) {
	var resp esSearchResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return searchResult{}, fmt.Errorf("decode search response: %w", err)
	}

	out := searchResult{
		Index:    index,
		TookMS:   resp.Took,
		TimedOut: resp.TimedOut,
		Total:    parseTotal(resp.Hits.Total),
		From:     from,
		Hits:     make([]searchHitOut, 0, len(resp.Hits.Hits)),
	}
	for _, h := range resp.Hits.Hits {
		out.Hits = append(out.Hits, searchHitOut{
			Index:     h.Index,
			Type:      h.Type,
			ID:        h.ID,
			Score:     h.Score,
			Source:    decodeSource(h.Source, maxFieldChars),
			Fields:    h.Fields,
			Highlight: h.Highlight,
			Sort:      h.Sort,
		})
	}
	if len(resp.Aggregations) > 0 {
		var aggs any
		if err := json.Unmarshal(resp.Aggregations, &aggs); err != nil {
			aggs = string(resp.Aggregations)
		}
		out.Aggregations = aggs
	}
	out.Returned = len(out.Hits)
	return out, nil
}

// fitSearchResult serialises res and drops the trailing hits (then, if still
// too large, the aggregations) until the payload fits maxBytes.
func fitSearchResult(res searchResult, maxBytes int) string {
	for {
		payload := prettyJSON(res)
		if len(payload) <= maxBytes {
			return payload
		}
		if len(res.Hits) > 0 {
			res.Hits = res.Hits[:len(res.Hits)-1]
			res.Returned = len(res.Hits)
			res.Truncated = true
			res.Notes = appendNote(res.Notes,
				fmt.Sprintf("response exceeded %d bytes; the oldest hits were dropped. Narrow the time range or use source=\"field1,field2\"", maxBytes))
			continue
		}
		if res.Aggregations != nil {
			res.Aggregations = nil
			res.Truncated = true
			res.Notes = appendNote(res.Notes, "aggregations were dropped because the response exceeded the size limit")
			continue
		}
		clipped, _ := clipText(prettyJSON(res), maxBytes)
		return clipped
	}
}

func appendNote(notes []string, note string) []string {
	for _, n := range notes {
		if n == note {
			return notes
		}
	}
	return append(notes, note)
}

// fitJSON serialises v untouched when it fits, otherwise clips the JSON text.
func fitJSON(v any, maxBytes int) string {
	payload := prettyJSON(v)
	clipped, truncated := clipText(payload, maxBytes)
	if truncated {
		return clipped + "\n\n(note: result exceeded the size limit; pass paging/limit arguments to narrow it)"
	}
	return clipped
}

func joinList(values []string) string {
	return strings.Join(values, ", ")
}
