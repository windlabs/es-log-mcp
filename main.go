// Command es-log-mcp is a Model Context Protocol (MCP) server for querying
// Elasticsearch 6.x log clusters over the plain REST API.
//
// It is read-only by default: raw writes need ES_READ_ONLY=false, and the
// dedicated query tools only ever issue search/count/aggregation requests.
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/mark3labs/mcp-go/server"
)

const serverVersion = "0.1.0"

const serverInstructions = `es-log-mcp: 面向 Elasticsearch 6.x 日志集群的只读查询服务。

工作流程建议：
1. 先调用 es_cluster_info 确认连通性和版本。
2. 用 es_list_indices（pattern 支持 *）找到正确的索引，如 filebeat-*、*nginx*。
3. 用 es_get_fields 查真实字段名与类型（searchable/aggregatable），不要猜字段。
4. 用 es_search 取日志样本（默认时间范围最近 1h、按 @timestamp 倒序），用 es_count 统计、
   es_aggregate 做 terms / date_histogram / stats / cardinality 分析。
5. 时间参数 since/until 支持 "15m"、"24h"、"7d" 这类相对写法和 ES date math（"now-1h"）。
   since="all" 表示不加时间过滤。
6. 结果是紧凑 JSON，受 ES_MAX_RESULT_BYTES 限制：优先缩小 since 或用 source 限定字段，
   不要一味增大 size（上限 ES_MAX_HITS）。

查询语法是 ES 6.x 的 query DSL：不要使用 7.x/8.x 专有语法（typeless API、runtime fields、
ES|QL、data streams 等）。需要非常规端点时用 es_raw_request（默认只允许读）。`

func main() {
	log.SetFlags(0)
	log.SetPrefix("es-log-mcp: ")
	log.SetOutput(os.Stderr)

	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-version" {
			fmt.Println(serverVersion)
			return
		}
	}

	loadDefaultEnvFile()

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	log.Printf("starting %s against %s (read_only=%v, default_index=%s, default_since=%s)",
		serverVersion, cfg.ESURL, cfg.ReadOnly, cfg.DefaultIndex, cfg.DefaultSince)

	client, err := NewESClient(cfg)
	if err != nil {
		log.Fatalf("client error: %v", err)
	}

	s := server.NewMCPServer("es-log-mcp", serverVersion,
		server.WithToolCapabilities(true),
		server.WithRecovery(),
		server.WithInstructions(serverInstructions),
	)
	registerTools(s, client)

	if err := server.ServeStdio(s); err != nil {
		log.Fatalf("stdio server stopped: %v", err)
	}
}
