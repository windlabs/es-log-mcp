# es_log_mcp

基于 [github.com/mark3labs/mcp-go](https://github.com/mark3labs/mcp-go) 的 Elasticsearch **日志查询 MCP 服务**，
面向 **Elasticsearch 6.x REST API**（已在 6.2.2 上验证），默认只读。

## 特点

- **只读优先**：内置查询工具只发 `_search` / `_count` / `_field_caps` / `_cat` 等读请求；
  `es_raw_request` 默认拒绝写操作，需要显式 `ES_READ_ONLY=false` 才放开。
- **ES 6.x 兼容**：不依赖官方客户端，直接走 REST；`hits.total` 同时兼容 6.x 的数字和 7+ 的对象形式；
  `_cat` / `_field_caps` 参数按 6.x 能力裁剪；不会生成 7.x/8.x 专有语法。
- **结果收敛**：时间范围默认最近 1h、`@timestamp` 倒序、单条字段长度和整体响应大小都有上限，
  超出时丢弃尾部命中并给出提示，避免一次查询打爆上下文。
- **错误可执行**：把 ES 的 `index_not_found`、未映射字段、text 字段聚合、权限错误等翻译成
  具体下一步（用 `es_list_indices` / `es_get_fields` / 换 `.keyword` 子字段等）。
- **配置简单**：一个二进制 + 一个私有 env 文件，天然适配 opencode / Codex / Claude / cc-switch。

## 构建

```bash
cd /mnt/d/code/go/src/w.src.corp.qihoo.net/pubsec/runtools/es_log_mcp
make test      # 单元测试
make build     # 产出 bin/es-log-mcp
make install   # 安装到 ~/.local/bin/es-log-mcp
```

Go 版本 1.23+；依赖已固定 `github.com/mark3labs/mcp-go v0.47.1`（该版本仍支持 Go 1.23，
更高的 v0.55+/v1.x 要求 Go 1.25+）。

## 配置

全部通过环境变量，**也可以用 env 文件**：默认读取
`$ES_LOG_MCP_ENV`，否则 `~/.config/es-log-mcp/env`（建议 `chmod 600`，
真实环境变量优先级更高）。

```bash
# ~/.config/es-log-mcp/env
ES_URL=http://rw-bd1b0d.es.qihudb.net:9200
ES_USERNAME=gviez_w
ES_PASSWORD=******
ES_DEFAULT_INDEX=wdlog_monitor_ginframe_service_*
ES_DEFAULT_SINCE=24h
ES_READ_ONLY=true
```

| 变量 | 说明 | 默认 |
| --- | --- | --- |
| `ES_URL` | 集群地址，必填 | — |
| `ES_USERNAME` / `ES_PASSWORD` | Basic Auth | 空 |
| `ES_API_KEY` | API Key 认证（6.7+ 集群），优先使用 Basic Auth | 空 |
| `ES_INSECURE` | 跳过 TLS 证书校验 | `false` |
| `ES_TIMEOUT_SECONDS` | 单次请求超时 | `30` |
| `ES_READ_ONLY` | 只读模式 | `true` |
| `ES_DEFAULT_INDEX` | 未传 `index` 时的默认索引 | `filebeat-*` |
| `ES_DEFAULT_SINCE` | 未传 `since` 时的默认时间范围 | `1h` |
| `ES_MAX_HITS` | 单次 `es_search` 返回条数上限 | `200` |
| `ES_MAX_RESULT_BYTES` | 单次工具结果字节上限 | `100000` |
| `ES_MAX_FIELD_CHARS` | `_source` 中单个字符串字段最大长度 | `4000` |
| `ES_LOG_MCP_ENV` | 自定义 env 文件路径 | `~/.config/es-log-mcp/env` |

## MCP 客户端配置

```jsonc
// opencode（Windows 侧需经 wsl.exe 桥接，见下文说明）
"mcp": {
  "servers": {
    "es-log": {
      "type": "local",
      "command": ["wsl.exe", "-d", "ubuntu", "-e", "/home/tank/.local/bin/es-log-mcp"]
    }
  }
}
```

```jsonc
// WSL 原生 opencode / Claude 等：直接调用
"es-log": {
  "type": "local",
  "command": ["/home/tank/.local/bin/es-log-mcp"]
}
```

```toml
# Codex ~/.codex/config.toml
[mcp_servers.es-log]
command = "/home/tank/.local/bin/es-log-mcp"
startup_timeout_sec = 30
```

凭据放在 env 文件里，客户端配置中不需要写密码。

## 工具

| 工具 | 作用 |
| --- | --- |
| `es_cluster_info` | 集群名、ES 版本、健康状态、节点/分片数；确认连通性用 |
| `es_list_indices` | 按 pattern 列索引，带 docs 数、存储大小、健康状态，可按 docs/size/name 排序 |
| `es_get_fields` | 基于 `_field_caps` 列出真实字段名、类型、`searchable` / `aggregatable` |
| `es_search` | 日志检索：`query`（DSL）/ `query_string`、`since`/`until`、`sort`、`source`、`highlight_fields`、`aggregations` |
| `es_count` | 计数，可按字段 `group_by`（terms） |
| `es_aggregate` | `terms` / `date_histogram` / `stats` / `cardinality`，支持 per-bucket 指标（avg/sum/min/max/percentiles） |
| `es_get_document` | 按 `_id` 取单条日志（内部用 ids 查询，兼容任意 `_type`） |
| `es_raw_request` | 任意 REST 端点逃生口，默认只读 |

### 时间参数

`since` / `until` 支持：

- 相对写法：`15m`、`30s`、`2h`、`7d`、`1w`、`2 months`
- ES date math：`now-1h`、`now-1h/d`
- ISO 时间：`2026-11-25T00:00:00`

`since="all"` 表示不加时间过滤。

### 示例

```jsonc
// 最近 15 分钟 error 日志
{"index":"wdlog_monitor_ginframe_service_*","since":"15m",
 "query":{"term":{"level":"error"}},"size":20,
 "source":"@timestamp,level,msg,app_name"}

// 按小时看错误量趋势
{"index":"wdlog_monitor_ginframe_service_*","type":"date_histogram",
 "field":"@timestamp","interval":"1h","since":"24h",
 "query":{"term":{"level":"error"}}}
```

## 只读与安全

- `es_raw_request` 在只读模式下只允许 `GET`/`HEAD` 和只读 `POST`
  （`_search`、`_count`、`_msearch`、`_mget`、`_field_caps`、`_validate`、`_explain`、`_analyze`、`_cat`）。
- 索引名做白名单字符校验（字母、数字、`- _ . * + , :`），路径禁止 `..` 与外链。
- 建议在 ES 侧为 MCP 单独创建只读 API Key / 账号，只授权目标索引。
- 凭据只落在 `~/.config/es-log-mcp/env`（600）或客户端 env 中，不要提交到仓库。

## 测试

```bash
go test ./...        # 时间解析、排序、路径校验、只读判定、结果裁剪、6.x/7.x total 解析
```

联调（真实集群，只读）：

```bash
es_cluster_info → es_list_indices → es_get_fields → es_search / es_count / es_aggregate
```

## 目录

```
es_log_mcp/
├── main.go        # 入口：配置加载、MCP server 注册、stdio
├── config.go      # 环境变量 + env 文件
├── client.go      # ES REST 客户端、错误翻译
├── query.go       # 查询构造、时间/排序解析、路径与只读校验
├── tools.go       # 8 个 MCP 工具
├── format.go      # 结果压缩/裁剪、_source 字段截断
├── main_test.go   # 单元测试
└── Makefile
```
