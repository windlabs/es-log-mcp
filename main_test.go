package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeTimeArg(t *testing.T) {
	cases := map[string]string{
		"15m":                 "now-15m",
		"30s":                 "now-30s",
		"2h":                  "now-2h",
		"7d":                  "now-7d",
		"1w":                  "now-1w",
		"2 months":            "now-2M",
		"1 year":              "now-1y",
		"now-1h":              "now-1h",
		"2026-11-25T00:00:00": "2026-11-25T00:00:00",
		"2026-11-25 10:00:00": "2026-11-25T10:00:00",
		"all":                 "all",
	}
	for in, want := range cases {
		if got := normalizeTimeArg(in); got != want {
			t.Errorf("normalizeTimeArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSortSpec(t *testing.T) {
	if got := parseSortSpec("none"); got != nil {
		t.Errorf("parseSortSpec(none) = %v, want nil", got)
	}
	if got := parseSortSpec(""); got != nil {
		t.Errorf("parseSortSpec(empty) = %v, want nil", got)
	}

	got := parseSortSpec("@timestamp:desc,level:asc")
	if len(got) != 2 {
		t.Fatalf("unexpected sort length: %v", got)
	}
	first := got[0].(map[string]any)["@timestamp"].(map[string]any)
	if first["order"] != "desc" {
		t.Errorf("first sort order = %v, want desc", first["order"])
	}
	second := got[1].(map[string]any)["level"].(map[string]any)
	if second["order"] != "asc" {
		t.Errorf("second sort order = %v, want asc", second["order"])
	}
	if got := parseSortSpec("_score:desc"); !sortEquals(got, "_score") {
		t.Errorf("_score sort not passed through: %v", got)
	}
}

func sortEquals(spec []any, field string) bool {
	if len(spec) != 1 {
		return false
	}
	_, ok := spec[0].(map[string]any)[field]
	return ok
}

func TestSanitizeIndexPattern(t *testing.T) {
	good := []string{"filebeat-*", "a,b", "*", "_all", "logs.2026.11.26", "x+y:z"}
	for _, in := range good {
		if _, err := sanitizeIndexPattern(in); err != nil {
			t.Errorf("sanitizeIndexPattern(%q) returned error: %v", in, err)
		}
	}
	bad := []string{"../../etc", "a/b", "a b", "a\nb", "a{b}", ".."}
	for _, in := range bad {
		if _, err := sanitizeIndexPattern(in); err == nil {
			t.Errorf("sanitizeIndexPattern(%q) should fail", in)
		}
	}
}

func TestSanitizeRawPath(t *testing.T) {
	if got, err := sanitizeRawPath("/_cluster/health"); err != nil || got != "_cluster/health" {
		t.Errorf("sanitizeRawPath = %q, %v", got, err)
	}
	if _, err := sanitizeRawPath("http://evil/_search"); err == nil {
		t.Error("absolute URL should be rejected")
	}
	if _, err := sanitizeRawPath("a/../b"); err == nil {
		t.Error("path traversal should be rejected")
	}
	if _, err := sanitizeRawPath(""); err == nil {
		t.Error("empty path should be rejected")
	}
}

func TestIsReadOnlyRequest(t *testing.T) {
	readOnly := [][2]string{
		{"GET", "_cluster/health"},
		{"HEAD", "filebeat-*"},
		{"POST", "filebeat-*/_search"},
		{"POST", "filebeat-*/_count"},
		{"POST", "_msearch"},
		{"POST", "idx/_field_caps"},
		{"POST", "idx/_validate/query"},
	}
	for _, tc := range readOnly {
		if !isReadOnlyRequest(tc[0], tc[1]) {
			t.Errorf("isReadOnlyRequest(%q, %q) = false, want true", tc[0], tc[1])
		}
	}
	writes := [][2]string{
		{"PUT", "idx/_doc/1"},
		{"DELETE", "idx"},
		{"POST", "_bulk"},
		{"POST", "idx/_delete_by_query"},
		{"POST", "idx/_update/1"},
	}
	for _, tc := range writes {
		if isReadOnlyRequest(tc[0], tc[1]) {
			t.Errorf("isReadOnlyRequest(%q, %q) = true, want false", tc[0], tc[1])
		}
	}
}

func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	content := "# comment\nexport ES_URL=http://example:9200\nES_USERNAME='user'\nES_PASSWORD=\"pass word\"\nBADLINE\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"ES_URL", "ES_USERNAME", "ES_PASSWORD"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatalf("loadEnvFile: %v", err)
	}
	if got := os.Getenv("ES_URL"); got != "http://example:9200" {
		t.Errorf("ES_URL = %q", got)
	}
	if got := os.Getenv("ES_USERNAME"); got != "user" {
		t.Errorf("ES_USERNAME = %q", got)
	}
	if got := os.Getenv("ES_PASSWORD"); got != "pass word" {
		t.Errorf("ES_PASSWORD = %q", got)
	}

	// Existing environment wins over the file.
	t.Setenv("ES_URL", "http://override:9200")
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("ES_URL"); got != "http://override:9200" {
		t.Errorf("ES_URL was overridden by the env file: %q", got)
	}
}

func TestFitSearchResultDropsHits(t *testing.T) {
	res := searchResult{
		Index: "idx",
		Total: int64(3),
		Hits: []searchHitOut{
			{Index: "idx", ID: "1", Source: map[string]any{"message": "aaaa"}},
			{Index: "idx", ID: "2", Source: map[string]any{"message": "bbbb"}},
			{Index: "idx", ID: "3", Source: map[string]any{"message": "cccc"}},
		},
		Returned: 3,
	}
	out := fitSearchResult(res, 200)
	if !containsAll(out, `"truncated": true`, `"returned": 0`) {
		t.Errorf("expected hits to be dropped, got: %s", out)
	}

	full := fitSearchResult(res, 100000)
	if !containsAll(full, `"message": "aaaa"`, `"message": "cccc"`) || containsAll(full, `"truncated": true`) {
		t.Errorf("expected full result, got: %s", full)
	}
}

func TestBuildSearchResultES6AndES7Totals(t *testing.T) {
	es6 := []byte(`{"took":3,"hits":{"total":2,"hits":[{"_index":"i","_type":"doc","_id":"1","_score":null,"_source":{"a":1}}]}}`)
	res, err := buildSearchResult("i", es6, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != int64(2) || res.Returned != 1 || res.Hits[0].Type != "doc" {
		t.Errorf("unexpected ES6 parse: %+v", res)
	}

	es7 := []byte(`{"took":3,"hits":{"total":{"value":10,"relation":"gte"},"hits":[]}}`)
	res, err = buildSearchResult("i", es7, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != "10 (gte)" {
		t.Errorf("unexpected ES7 total: %v", res.Total)
	}
}

func TestESErrorHintUsesRootCause(t *testing.T) {
	body := []byte(`{"error":{"root_cause":[{"type":"illegal_argument_exception","reason":"Fielddata is disabled on text fields by default. Set fielddata=true on [caller]"}],"type":"search_phase_execution_exception","reason":"all shards failed"},"status":400}`)
	err := &ESError{Method: "POST", Path: "idx/_search", Status: 400, StatusText: "400 Bad Request", Body: body}
	msg := err.Error()
	if !strings.Contains(msg, "keyword subfield") {
		t.Errorf("expected text-field aggregation hint, got: %s", msg)
	}

	notFound := &ESError{Method: "POST", Path: "nope/_search", Status: 404, StatusText: "404 Not Found", Body: []byte(`{"error":{"type":"index_not_found_exception","reason":"no such index [nope]"},"status":404}`)}
	if !strings.Contains(notFound.Error(), "es_list_indices") {
		t.Errorf("expected index hint, got: %s", notFound.Error())
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
