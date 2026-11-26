package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// registerTools wires every tool into the MCP server.
func registerTools(s *server.MCPServer, c *ESClient) {
	s.AddTool(toolClusterInfo(), handlerClusterInfo(c))
	s.AddTool(toolListIndices(), handlerListIndices(c))
	s.AddTool(toolGetFields(), handlerGetFields(c))
	s.AddTool(toolSearch(), handlerSearch(c))
	s.AddTool(toolCount(), handlerCount(c))
	s.AddTool(toolAggregate(), handlerAggregate(c))
	s.AddTool(toolGetDocument(), handlerGetDocument(c))
	s.AddTool(toolRawRequest(), handlerRawRequest(c))
}

func fail(err error) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(err.Error()), nil
}

func failf(format string, args ...any) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(fmt.Sprintf(format, args...)), nil
}

// ---------------------------------------------------------------- cluster ---

func toolClusterInfo() mcp.Tool {
	return mcp.NewTool("es_cluster_info",
		mcp.WithDescription("Show Elasticsearch cluster name, version, health status, node count and shard counts. Use this first to confirm connectivity and the ES version (this server targets ES 6.x)."),
		mcp.WithReadOnlyHintAnnotation(true),
	)
}

func handlerClusterInfo(c *ESClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		type rootInfo struct {
			Name        string `json:"name"`
			ClusterName string `json:"cluster_name"`
			Tagline     string `json:"tagline"`
			Version     struct {
				Number string `json:"number"`
			} `json:"version"`
		}
		type health struct {
			ClusterName         string  `json:"cluster_name"`
			Status              string  `json:"status"`
			TimedOut            bool    `json:"timed_out"`
			NumberOfNodes       int     `json:"number_of_nodes"`
			NumberOfDataNodes   int     `json:"number_of_data_nodes"`
			ActivePrimaryShards int     `json:"active_primary_shards"`
			ActiveShards        int     `json:"active_shards"`
			RelocatingShards    int     `json:"relocating_shards"`
			InitializingShards  int     `json:"initializing_shards"`
			UnassignedShards    int     `json:"unassigned_shards"`
			PendingTasks        int     `json:"number_of_pending_tasks"`
			ActiveShardsPercent float64 `json:"active_shards_percent_as_number"`
		}

		var root rootInfo
		if err := c.DoJSON(ctx, "GET", "", nil, nil, &root); err != nil {
			return fail(err)
		}
		var h health
		if err := c.DoJSON(ctx, "GET", "_cluster/health", nil, nil, &h); err != nil {
			return fail(err)
		}

		result := map[string]any{
			"cluster_name": h.ClusterName,
			"es_version":   root.Version.Number,
			"status":       h.Status,
			"timed_out":    h.TimedOut,
			"nodes": map[string]any{
				"total": h.NumberOfNodes,
				"data":  h.NumberOfDataNodes,
			},
			"shards": map[string]any{
				"active_primary": h.ActivePrimaryShards,
				"active":         h.ActiveShards,
				"relocating":     h.RelocatingShards,
				"initializing":   h.InitializingShards,
				"unassigned":     h.UnassignedShards,
				"active_percent": h.ActiveShardsPercent,
				"pending_tasks":  h.PendingTasks,
			},
		}
		return mcp.NewToolResultText(prettyJSON(result)), nil
	}
}

// ----------------------------------------------------------------- cat ---

type catIndex struct {
	Index     string `json:"index"`
	Health    string `json:"health"`
	Status    string `json:"status"`
	DocsCount string `json:"docs.count"`
	StoreSize string `json:"store.size"`
	Pri       string `json:"pri"`
	Rep       string `json:"rep"`
}

func toolListIndices() mcp.Tool {
	return mcp.NewTool("es_list_indices",
		mcp.WithDescription("List indices matching a pattern, with health, document count and store size (sorted). Use it to discover the right index pattern before querying, e.g. pattern=\"filebeat-*\" or \"*nginx*\"."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("pattern", mcp.Description("Index pattern, supports wildcards (*). Default \"*\""), mcp.DefaultString("*")),
		mcp.WithNumber("limit", mcp.Description("Maximum number of indices to return (default 50, max 500)"), mcp.DefaultNumber(50)),
		mcp.WithString("sort", mcp.Description("Sort order: docs (most documents first), size (largest first), name (alphabetical)"), mcp.DefaultString("docs"), mcp.Enum("docs", "size", "name")),
		mcp.WithBoolean("include_hidden", mcp.Description("Include system indices whose name starts with '.' (default false)"), mcp.DefaultBool(false)),
		mcp.WithNumber("min_docs", mcp.Description("Only return indices with at least this many documents (default 0)"), mcp.DefaultNumber(0)),
	)
}

func handlerListIndices(c *ESClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		pattern, err := sanitizeIndexPattern(req.GetString("pattern", "*"))
		if err != nil {
			return fail(err)
		}
		limit := req.GetInt("limit", 50)
		if limit <= 0 {
			limit = 50
		}
		if limit > 500 {
			limit = 500
		}
		includeHidden := req.GetBool("include_hidden", false)
		minDocs := int64(req.GetInt("min_docs", 0))
		sortBy := req.GetString("sort", "docs")

		params := url.Values{
			"format": {"json"},
			"bytes":  {"b"},
			"h":      {"index,health,status,docs.count,store.size,pri,rep"},
		}
		var rows []catIndex
		if err := c.DoJSON(ctx, "GET", "_cat/indices/"+pattern, params, nil, &rows); err != nil {
			var esErr *ESError
			if errors.As(err, &esErr) && strings.Contains(esErr.Error(), "index_not_found_exception") {
				return mcp.NewToolResultText(prettyJSON(map[string]any{
					"pattern": pattern,
					"total":   0,
					"indices": []any{},
					"note":    "no index matched this pattern",
				})), nil
			}
			return fail(err)
		}

		type indexRow struct {
			Index     string `json:"index"`
			Health    string `json:"health"`
			Status    string `json:"status"`
			Docs      int64  `json:"docs"`
			Size      string `json:"size"`
			SizeBytes int64  `json:"size_bytes"`
			Pri       int    `json:"pri"`
			Rep       int    `json:"rep"`
		}
		indices := make([]indexRow, 0, len(rows))
		for _, r := range rows {
			if !includeHidden && strings.HasPrefix(r.Index, ".") {
				continue
			}
			docs, _ := strconv.ParseInt(r.DocsCount, 10, 64)
			if minDocs > 0 && docs < minDocs {
				continue
			}
			sizeBytes, _ := strconv.ParseInt(r.StoreSize, 10, 64)
			pri, _ := strconv.Atoi(r.Pri)
			rep, _ := strconv.Atoi(r.Rep)
			indices = append(indices, indexRow{
				Index:     r.Index,
				Health:    r.Health,
				Status:    r.Status,
				Docs:      docs,
				Size:      humanizeBytes(sizeBytes),
				SizeBytes: sizeBytes,
				Pri:       pri,
				Rep:       rep,
			})
		}

		switch sortBy {
		case "size":
			sort.Slice(indices, func(i, j int) bool { return indices[i].SizeBytes > indices[j].SizeBytes })
		case "name":
			sort.Slice(indices, func(i, j int) bool { return indices[i].Index < indices[j].Index })
		default:
			sort.Slice(indices, func(i, j int) bool { return indices[i].Docs > indices[j].Docs })
		}

		total := len(indices)
		truncated := false
		if len(indices) > limit {
			indices = indices[:limit]
			truncated = true
		}

		result := map[string]any{
			"pattern":   pattern,
			"total":     total,
			"returned":  len(indices),
			"truncated": truncated,
			"indices":   indices,
		}
		return mcp.NewToolResultText(fitJSON(result, c.cfg.MaxResultBytes)), nil
	}
}

// --------------------------------------------------------------- fields ---

type fieldCapsResponse struct {
	Indices []string `json:"indices"`
	Fields  map[string]map[string]struct {
		Type         string `json:"type"`
		Searchable   bool   `json:"searchable"`
		Aggregatable bool   `json:"aggregatable"`
	} `json:"fields"`
}

func toolGetFields() mcp.Tool {
	return mcp.NewTool("es_get_fields",
		mcp.WithDescription("List field names, types and capabilities (searchable / aggregatable) for an index pattern, using the _field_caps API. Call this before writing a query so you use real field names instead of guessing."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("index", mcp.Description("Index or pattern, e.g. \"filebeat-*\"")),
		mcp.WithString("pattern", mcp.Description("Only return fields matching this glob or substring, e.g. \"*ip*\" or \"user\"")),
		mcp.WithBoolean("include_meta", mcp.Description("Include meta fields whose name starts with '_' (default false)"), mcp.DefaultBool(false)),
		mcp.WithNumber("limit", mcp.Description("Maximum number of fields to return (default 200, max 2000)"), mcp.DefaultNumber(200)),
	)
}

func handlerGetFields(c *ESClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		index, err := sanitizeIndexPattern(req.GetString("index", c.cfg.DefaultIndex))
		if err != nil {
			return fail(err)
		}
		fieldFilter := strings.TrimSpace(req.GetString("pattern", ""))
		includeMeta := req.GetBool("include_meta", false)
		limit := req.GetInt("limit", 200)
		if limit <= 0 {
			limit = 200
		}
		if limit > 2000 {
			limit = 2000
		}

		params := url.Values{"fields": {"*"}}
		var caps fieldCapsResponse
		if err := c.DoJSON(ctx, "GET", index+"/_field_caps", params, nil, &caps); err != nil {
			return fail(err)
		}

		type fieldRow struct {
			Field        string   `json:"field"`
			Type         string   `json:"type"`
			Searchable   bool     `json:"searchable"`
			Aggregatable bool     `json:"aggregatable"`
			AltTypes     []string `json:"alt_types,omitempty"`
		}

		rows := make([]fieldRow, 0, len(caps.Fields))
		for name, types := range caps.Fields {
			if !includeMeta && strings.HasPrefix(name, "_") {
				continue
			}
			if !matchFieldFilter(name, fieldFilter) {
				continue
			}
			row := fieldRow{Field: name}
			first := true
			for _, info := range types {
				if first {
					row.Type = info.Type
					row.Searchable = info.Searchable
					row.Aggregatable = info.Aggregatable
					first = false
					continue
				}
				row.AltTypes = append(row.AltTypes, info.Type)
			}
			sort.Strings(row.AltTypes)
			rows = append(rows, row)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Field < rows[j].Field })

		total := len(rows)
		truncated := false
		if len(rows) > limit {
			rows = rows[:limit]
			truncated = true
		}

		result := map[string]any{
			"index":     index,
			"total":     total,
			"returned":  len(rows),
			"truncated": truncated,
			"fields":    rows,
		}
		if len(caps.Indices) > 0 {
			result["indices"] = caps.Indices
		}
		return mcp.NewToolResultText(fitJSON(result, c.cfg.MaxResultBytes)), nil
	}
}

func matchFieldFilter(field, filter string) bool {
	if filter == "" {
		return true
	}
	if strings.ContainsAny(filter, "*?[") {
		if ok, err := path.Match(filter, field); err == nil && ok {
			return true
		}
	}
	return strings.Contains(strings.ToLower(field), strings.ToLower(strings.Trim(filter, "*")))
}

// --------------------------------------------------------------- search ---

func toolSearch() mcp.Tool {
	return mcp.NewTool("es_search",
		mcp.WithDescription("Search log documents with the Elasticsearch 6.x query DSL. A time range filter (since/until on time_field, default last 1h) and a @timestamp:desc sort are applied by default. Use es_get_fields first when unsure about field names. Results are compact and size limited; narrow since/source instead of raising size."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("index", mcp.Description("Index or pattern, e.g. \"filebeat-*\"")),
		mcp.WithObject("query", mcp.Description("ES query DSL object, e.g. {\"term\":{\"level\":\"error\"}} or {\"match\":{\"message\":\"timeout\"}}")),
		mcp.WithString("query_string", mcp.Description("Convenience Lucene query string, e.g. \"level:error AND message:*timeout*\". Combined with query via bool.must.")),
		mcp.WithString("since", mcp.Description("Start of the time range: relative (\"15m\", \"24h\", \"7d\") or ES date math (\"now-1h\", \"2026-11-25T00:00:00\"). Use \"all\" to disable the range filter.")),
		mcp.WithString("until", mcp.Description("End of the time range (default \"now\")")),
		mcp.WithString("time_field", mcp.Description("Timestamp field used by since/until (default \"@timestamp\")")),
		mcp.WithNumber("size", mcp.Description("Number of hits to return (default 10)"), mcp.DefaultNumber(10)),
		mcp.WithNumber("from", mcp.Description("Offset for paging, use with size"), mcp.DefaultNumber(0)),
		mcp.WithString("sort", mcp.Description("Sort spec, e.g. \"@timestamp:desc\" or \"level:asc,@timestamp:desc\". Use \"none\" for relevance order.")),
		mcp.WithString("source", mcp.Description("Comma separated list of _source fields to return, e.g. \"@timestamp,level,message,host.name\"")),
		mcp.WithString("highlight_fields", mcp.Description("Comma separated fields to highlight, e.g. \"message\". Matches are wrapped in **.")),
		mcp.WithObject("aggregations", mcp.Description("Optional ES aggregations object; the search returns its buckets next to the hits")),
	)
}

func handlerSearch(c *ESClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		index, err := sanitizeIndexPattern(req.GetString("index", c.cfg.DefaultIndex))
		if err != nil {
			return fail(err)
		}
		query, err := c.buildQuery(req)
		if err != nil {
			return fail(err)
		}

		size := req.GetInt("size", 10)
		var notes []string
		if size < 0 {
			size = 0
		}
		if size > c.cfg.MaxHits {
			notes = append(notes, fmt.Sprintf("size clamped from %d to %d (ES_MAX_HITS)", size, c.cfg.MaxHits))
			size = c.cfg.MaxHits
		}
		from := req.GetInt("from", 0)
		if from < 0 {
			from = 0
		}
		if from+size > 10000 {
			notes = append(notes, "from+size exceeds 10000, the ES 6 default index.max_result_window; use a smaller offset or a narrower time range")
		}

		body := map[string]any{
			"query": query,
			"size":  size,
			"from":  from,
		}
		if sortSpec := parseSortSpec(req.GetString("sort", "@timestamp:desc")); sortSpec != nil {
			body["sort"] = sortSpec
		}
		if src := strings.TrimSpace(req.GetString("source", "")); src != "" {
			body["_source"] = splitList(src)
		}
		if aggs, ok := objArg(req, "aggregations"); ok {
			body["aggs"] = aggs
		}
		if hf := strings.TrimSpace(req.GetString("highlight_fields", "")); hf != "" {
			fields := map[string]any{}
			for _, f := range splitList(hf) {
				fields[f] = map[string]any{"fragment_size": 200, "number_of_fragments": 2}
			}
			body["highlight"] = map[string]any{"pre_tags": []string{"**"}, "post_tags": []string{"**"}, "fields": fields}
		}

		data, _, err := c.Do(ctx, "POST", index+"/_search", nil, body)
		if err != nil {
			return fail(err)
		}
		res, err := buildSearchResult(index, data, from, c.cfg.MaxFieldChars)
		if err != nil {
			return fail(err)
		}
		if since := req.GetString("since", c.cfg.DefaultSince); !isAllTime(since) && res.Total == int64(0) {
			notes = append(notes, fmt.Sprintf("0 hits in the selected range; verify time_field (currently %q) with es_get_fields, or widen since (use \"all\" to drop the time filter)", req.GetString("time_field", "@timestamp")))
		}
		res.Notes = append(res.Notes, notes...)
		return mcp.NewToolResultText(fitSearchResult(res, c.cfg.MaxResultBytes)), nil
	}
}

// ---------------------------------------------------------------- count ---

func toolCount() mcp.Tool {
	return mcp.NewTool("es_count",
		mcp.WithDescription("Count documents matching a query and time range. Optionally group the count by a field (terms aggregation) to get a quick breakdown, e.g. by level, host.name or response code."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("index", mcp.Description("Index or pattern, e.g. \"filebeat-*\"")),
		mcp.WithObject("query", mcp.Description("ES query DSL object (optional)")),
		mcp.WithString("query_string", mcp.Description("Convenience Lucene query string (optional)")),
		mcp.WithString("since", mcp.Description("Start of the time range, default the ES_DEFAULT_SINCE value (1h). Use \"all\" to disable.")),
		mcp.WithString("until", mcp.Description("End of the time range (default \"now\")")),
		mcp.WithString("time_field", mcp.Description("Timestamp field used by since/until (default \"@timestamp\")")),
		mcp.WithString("group_by", mcp.Description("Optional field to group by (terms aggregation)")),
		mcp.WithNumber("group_size", mcp.Description("Number of groups to return when group_by is set (default 20, max 100)"), mcp.DefaultNumber(20)),
	)
}

func handlerCount(c *ESClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		index, err := sanitizeIndexPattern(req.GetString("index", c.cfg.DefaultIndex))
		if err != nil {
			return fail(err)
		}
		query, err := c.buildQuery(req)
		if err != nil {
			return fail(err)
		}

		groupBy := strings.TrimSpace(req.GetString("group_by", ""))
		if groupBy == "" {
			var resp struct {
				Count int64 `json:"count"`
			}
			if err := c.DoJSON(ctx, "POST", index+"/_count", nil, map[string]any{"query": query}, &resp); err != nil {
				return fail(err)
			}
			return mcp.NewToolResultText(prettyJSON(map[string]any{
				"index": index,
				"total": resp.Count,
			})), nil
		}

		groupSize := req.GetInt("group_size", 20)
		if groupSize <= 0 {
			groupSize = 20
		}
		if groupSize > 100 {
			groupSize = 100
		}
		body := map[string]any{
			"query": query,
			"size":  0,
			"aggs": map[string]any{
				"groups": map[string]any{
					"terms": map[string]any{"field": groupBy, "size": groupSize, "missing": "(missing)"},
				},
			},
		}
		var resp struct {
			Hits struct {
				Total json.RawMessage `json:"total"`
			} `json:"hits"`
			Aggregations map[string]json.RawMessage `json:"aggregations"`
		}
		if err := c.DoJSON(ctx, "POST", index+"/_search", nil, body, &resp); err != nil {
			return fail(err)
		}
		buckets, err := decodeBuckets(resp.Aggregations["groups"])
		if err != nil {
			return fail(err)
		}
		groups := make([]map[string]any, 0, len(buckets))
		for _, b := range buckets {
			groups = append(groups, map[string]any{
				"key":   bucketKey(b),
				"count": b.DocCount,
			})
		}
		return mcp.NewToolResultText(fitJSON(map[string]any{
			"index":    index,
			"total":    parseTotal(resp.Hits.Total),
			"group_by": groupBy,
			"groups":   groups,
		}, c.cfg.MaxResultBytes)), nil
	}
}

// ------------------------------------------------------------ aggregate ---

type aggBucket struct {
	Key         any        `json:"key"`
	KeyAsString string     `json:"key_as_string"`
	DocCount    int64      `json:"doc_count"`
	Metric      *aggMetric `json:"metric"`
}

type aggMetric struct {
	Value  *float64       `json:"value"`
	Values map[string]any `json:"values"`
}

type bucketAgg struct {
	Buckets []aggBucket `json:"buckets"`
}

func decodeBuckets(raw json.RawMessage) ([]aggBucket, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("aggregation result is empty")
	}
	var agg bucketAgg
	if err := json.Unmarshal(raw, &agg); err != nil {
		return nil, fmt.Errorf("decode aggregation buckets: %w", err)
	}
	return agg.Buckets, nil
}

func bucketKey(b aggBucket) any {
	if b.KeyAsString != "" {
		return b.KeyAsString
	}
	return b.Key
}

func toolAggregate() mcp.Tool {
	return mcp.NewTool("es_aggregate",
		mcp.WithDescription("Aggregate logs: terms (top values of a field), date_histogram (counts over time), stats (min/max/avg/sum) or cardinality (distinct count). Use it for trends, top-N breakdowns and baseline analysis instead of fetching raw documents."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("index", mcp.Description("Index or pattern, e.g. \"filebeat-*\"")),
		mcp.WithString("type", mcp.Description("Aggregation type"), mcp.Required(), mcp.DefaultString("terms"), mcp.Enum("terms", "date_histogram", "stats", "cardinality")),
		mcp.WithString("field", mcp.Description("Field to aggregate on, e.g. \"level\" or \"host.name\" (for stats/cardinality this is the numeric field)"), mcp.Required()),
		mcp.WithString("interval", mcp.Description("date_histogram interval, e.g. \"1m\", \"1h\", \"1d\" (ES 6 syntax, default 1h)")),
		mcp.WithString("metric", mcp.Description("Optional metric computed per bucket (terms/date_histogram)"), mcp.Enum("count", "avg", "sum", "min", "max", "cardinality", "percentiles")),
		mcp.WithString("metric_field", mcp.Description("Field for the metric; defaults to \"field\"")),
		mcp.WithNumber("size", mcp.Description("Maximum buckets to return (default 60, max 500)"), mcp.DefaultNumber(60)),
		mcp.WithString("order", mcp.Description("Bucket order for terms: count (default) or key"), mcp.DefaultString("count"), mcp.Enum("count", "key")),
		mcp.WithObject("query", mcp.Description("ES query DSL object (optional)")),
		mcp.WithString("query_string", mcp.Description("Convenience Lucene query string (optional)")),
		mcp.WithString("since", mcp.Description("Start of the time range, default the ES_DEFAULT_SINCE value (1h). Use \"all\" to disable.")),
		mcp.WithString("until", mcp.Description("End of the time range (default \"now\")")),
		mcp.WithString("time_field", mcp.Description("Timestamp field used by since/until (default \"@timestamp\")")),
	)
}

func handlerAggregate(c *ESClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		index, err := sanitizeIndexPattern(req.GetString("index", c.cfg.DefaultIndex))
		if err != nil {
			return fail(err)
		}
		aggType := req.GetString("type", "terms")
		field := strings.TrimSpace(req.GetString("field", ""))
		if field == "" {
			return failf("field is required, e.g. field=\"level\"")
		}
		metric := strings.TrimSpace(req.GetString("metric", ""))
		metricField := strings.TrimSpace(req.GetString("metric_field", ""))
		if metricField == "" {
			metricField = field
		}
		size := req.GetInt("size", 60)
		if size <= 0 {
			size = 60
		}
		if size > 500 {
			size = 500
		}
		interval := req.GetString("interval", "1h")

		query, err := c.buildQuery(req)
		if err != nil {
			return fail(err)
		}

		var aggDef map[string]any
		switch aggType {
		case "terms":
			order := map[string]any{"_count": "desc"}
			if req.GetString("order", "count") == "key" {
				order = map[string]any{"_key": "asc"}
			}
			aggDef = map[string]any{"terms": map[string]any{"field": field, "size": size, "order": order, "missing": "(missing)"}}
		case "date_histogram":
			aggDef = map[string]any{"date_histogram": map[string]any{"field": field, "interval": interval, "min_doc_count": 0}}
		case "stats":
			aggDef = map[string]any{"stats": map[string]any{"field": metricField}}
		case "cardinality":
			aggDef = map[string]any{"cardinality": map[string]any{"field": metricField}}
		default:
			return failf("unsupported type %q (use terms, date_histogram, stats or cardinality)", aggType)
		}
		if metric != "" && metric != "count" && (aggType == "terms" || aggType == "date_histogram") {
			var metricDef map[string]any
			switch metric {
			case "percentiles":
				metricDef = map[string]any{"percentiles": map[string]any{"field": metricField, "percents": []int{50, 90, 95, 99}}}
			default:
				metricDef = map[string]any{metric: map[string]any{"field": metricField}}
			}
			aggDef["aggs"] = map[string]any{"metric": metricDef}
		}

		body := map[string]any{
			"query": query,
			"size":  0,
			"aggs":  map[string]any{"g": aggDef},
		}
		var resp struct {
			Hits struct {
				Total json.RawMessage `json:"total"`
			} `json:"hits"`
			Aggregations map[string]json.RawMessage `json:"aggregations"`
		}
		if err := c.DoJSON(ctx, "POST", index+"/_search", nil, body, &resp); err != nil {
			return fail(err)
		}

		raw := resp.Aggregations["g"]
		result := map[string]any{
			"index":      index,
			"type":       aggType,
			"field":      field,
			"total_hits": parseTotal(resp.Hits.Total),
		}
		if aggType == "date_histogram" {
			result["interval"] = interval
		}

		switch aggType {
		case "stats":
			var stats struct {
				Count int64    `json:"count"`
				Min   *float64 `json:"min"`
				Max   *float64 `json:"max"`
				Avg   *float64 `json:"avg"`
				Sum   *float64 `json:"sum"`
			}
			if err := json.Unmarshal(raw, &stats); err != nil {
				return fail(fmt.Errorf("decode stats aggregation: %w", err))
			}
			result["stats"] = map[string]any{
				"count": stats.Count,
				"min":   stats.Min,
				"max":   stats.Max,
				"avg":   stats.Avg,
				"sum":   stats.Sum,
			}
		case "cardinality":
			var card struct {
				Value int64 `json:"value"`
			}
			if err := json.Unmarshal(raw, &card); err != nil {
				return fail(fmt.Errorf("decode cardinality aggregation: %w", err))
			}
			result["distinct"] = card.Value
		default:
			buckets, err := decodeBuckets(raw)
			if err != nil {
				return fail(err)
			}
			rows := make([]map[string]any, 0, len(buckets))
			for _, b := range buckets {
				row := map[string]any{"key": bucketKey(b), "count": b.DocCount}
				if b.Metric != nil {
					if b.Metric.Values != nil {
						row["metric"] = b.Metric.Values
					} else if b.Metric.Value != nil {
						row["metric"] = *b.Metric.Value
					}
				}
				rows = append(rows, row)
			}
			if len(rows) > size {
				rows = rows[:size]
				result["truncated"] = true
			}
			result["returned"] = len(rows)
			result["buckets"] = rows
			if metric != "" {
				result["metric"] = metric
				result["metric_field"] = metricField
			}
		}
		return mcp.NewToolResultText(fitJSON(result, c.cfg.MaxResultBytes)), nil
	}
}

// ------------------------------------------------------------- document ---

func toolGetDocument() mcp.Tool {
	return mcp.NewTool("es_get_document",
		mcp.WithDescription("Fetch a single log document by its _id (implemented as an ids query so it works with any ES 6.x doc type). Returns the full _source."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("index", mcp.Description("Index or pattern that contains the document"), mcp.Required()),
		mcp.WithString("id", mcp.Description("Document _id"), mcp.Required()),
		mcp.WithString("source", mcp.Description("Optional comma separated list of _source fields to return")),
	)
}

func handlerGetDocument(c *ESClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		index, err := sanitizeIndexPattern(req.GetString("index", c.cfg.DefaultIndex))
		if err != nil {
			return fail(err)
		}
		id := strings.TrimSpace(req.GetString("id", ""))
		if id == "" {
			return failf("id is required")
		}
		body := map[string]any{
			"query": map[string]any{"ids": map[string]any{"values": []string{id}}},
			"size":  1,
		}
		if src := strings.TrimSpace(req.GetString("source", "")); src != "" {
			body["_source"] = splitList(src)
		}
		data, _, err := c.Do(ctx, "POST", index+"/_search", nil, body)
		if err != nil {
			return fail(err)
		}
		res, err := buildSearchResult(index, data, 0, c.cfg.MaxFieldChars)
		if err != nil {
			return fail(err)
		}
		if res.Total == int64(0) {
			res.Notes = append(res.Notes, "no document with this _id in the index pattern")
		}
		return mcp.NewToolResultText(fitSearchResult(res, c.cfg.MaxResultBytes)), nil
	}
}

// ---------------------------------------------------------- raw request ---

func toolRawRequest() mcp.Tool {
	return mcp.NewTool("es_raw_request",
		mcp.WithDescription("Escape hatch: call any Elasticsearch 6.x REST endpoint directly. In the default read-only mode only GET/HEAD and read-only POST endpoints (_search, _count, _msearch, _mget, _field_caps, _validate, _explain, _analyze, _cat) are allowed; set ES_READ_ONLY=false to lift this."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithString("method", mcp.Description("HTTP method"), mcp.DefaultString("GET"), mcp.Enum("GET", "POST", "PUT", "DELETE", "HEAD")),
		mcp.WithString("path", mcp.Description("Path relative to the cluster root, e.g. \"_cluster/settings\" or \"filebeat-*/_search\""), mcp.Required()),
		mcp.WithObject("params", mcp.Description("Optional URL query parameters as an object")),
		mcp.WithObject("body", mcp.Description("Optional JSON request body")),
	)
}

func handlerRawRequest(c *ESClient) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		method := strings.ToUpper(strings.TrimSpace(req.GetString("method", "GET")))
		p, err := sanitizeRawPath(req.GetString("path", ""))
		if err != nil {
			return fail(err)
		}
		if c.cfg.ReadOnly && !isReadOnlyRequest(method, p) {
			return failf("read-only mode: %s %s is not allowed. Only GET/HEAD and read-only POST endpoints are permitted; start the server with ES_READ_ONLY=false to allow writes", method, p)
		}

		var params url.Values
		if obj, ok := objArg(req, "params"); ok {
			params = url.Values{}
			for k, v := range obj {
				switch val := v.(type) {
				case []any:
					for _, item := range val {
						params.Add(k, fmt.Sprint(item))
					}
				default:
					params.Set(k, fmt.Sprint(val))
				}
			}
		}
		bodyObj, _ := objArg(req, "body")

		data, status, err := c.Do(ctx, method, p, params, bodyObj)
		if err != nil {
			return fail(err)
		}

		result := map[string]any{
			"status": status,
			"method": method,
			"path":   p,
		}
		var decoded any
		if len(data) > 0 && json.Unmarshal(data, &decoded) == nil {
			result["body"] = decoded
		} else if len(data) > 0 {
			result["body"], _ = clipText(string(data), c.cfg.MaxResultBytes)
		} else {
			result["body"] = nil
		}
		return mcp.NewToolResultText(fitJSON(result, c.cfg.MaxResultBytes)), nil
	}
}
