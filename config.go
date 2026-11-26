package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds everything needed to talk to one Elasticsearch cluster.
//
// All values come from environment variables, so the same binary can be
// pointed at different clusters by different MCP clients.
type Config struct {
	ESURL          string
	Username       string
	Password       string
	APIKey         string
	Insecure       bool
	Timeout        time.Duration
	ReadOnly       bool
	DefaultIndex   string
	DefaultSince   string
	MaxHits        int
	MaxResultBytes int
	MaxFieldChars  int
}

const (
	defaultMaxHits        = 200
	defaultMaxResultBytes = 100_000
	defaultMaxFieldChars  = 4000
	defaultTimeout        = 30 * time.Second
)

// loadDefaultEnvFile loads key=value pairs from a private env file when the
// MCP client does not pass credentials in the server definition. Real
// environment variables always win, so callers can override single values.
//
// Lookup order: $ES_LOG_MCP_ENV, then ~/.config/es-log-mcp/env.
func loadDefaultEnvFile() {
	path := os.Getenv("ES_LOG_MCP_ENV")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		path = filepath.Join(home, ".config", "es-log-mcp", "env")
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := loadEnvFile(path); err != nil {
		fmt.Fprintf(os.Stderr, "es-log-mcp: warning: cannot load env file %s: %v\n", path, err)
	}
}

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return sc.Err()
}

func loadConfig() (Config, error) {
	cfg := Config{
		ESURL:          strings.TrimSpace(os.Getenv("ES_URL")),
		Username:       strings.TrimSpace(os.Getenv("ES_USERNAME")),
		Password:       os.Getenv("ES_PASSWORD"),
		APIKey:         strings.TrimSpace(os.Getenv("ES_API_KEY")),
		Insecure:       envBool("ES_INSECURE", false),
		Timeout:        time.Duration(envInt("ES_TIMEOUT_SECONDS", int(defaultTimeout.Seconds()))) * time.Second,
		ReadOnly:       envBool("ES_READ_ONLY", true),
		DefaultIndex:   envString("ES_DEFAULT_INDEX", "filebeat-*"),
		DefaultSince:   envString("ES_DEFAULT_SINCE", "1h"),
		MaxHits:        envInt("ES_MAX_HITS", defaultMaxHits),
		MaxResultBytes: envInt("ES_MAX_RESULT_BYTES", defaultMaxResultBytes),
		MaxFieldChars:  envInt("ES_MAX_FIELD_CHARS", defaultMaxFieldChars),
	}

	if cfg.ESURL == "" {
		return cfg, fmt.Errorf("ES_URL is not set (env or ~/.config/es-log-mcp/env)")
	}
	u, err := url.Parse(cfg.ESURL)
	if err != nil {
		return cfg, fmt.Errorf("invalid ES_URL %q: %w", cfg.ESURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return cfg, fmt.Errorf("invalid ES_URL %q: scheme must be http or https", cfg.ESURL)
	}
	if u.Host == "" {
		return cfg, fmt.Errorf("invalid ES_URL %q: missing host", cfg.ESURL)
	}
	if cfg.Username == "" && cfg.APIKey == "" {
		fmt.Fprintln(os.Stderr, "es-log-mcp: warning: neither ES_USERNAME/ES_PASSWORD nor ES_API_KEY is set; requests will be sent unauthenticated")
	}
	if cfg.MaxHits <= 0 {
		cfg.MaxHits = defaultMaxHits
	}
	if cfg.MaxResultBytes < 2000 {
		cfg.MaxResultBytes = defaultMaxResultBytes
	}
	if cfg.MaxFieldChars < 0 {
		cfg.MaxFieldChars = defaultMaxFieldChars
	}
	if cfg.DefaultIndex == "" {
		cfg.DefaultIndex = "*"
	}
	return cfg, nil
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		fmt.Fprintf(os.Stderr, "es-log-mcp: warning: %s=%q is not an integer, using %d\n", key, v, def)
		return def
	}
	return n
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		fmt.Fprintf(os.Stderr, "es-log-mcp: warning: %s=%q is not a boolean, using %v\n", key, v, def)
		return def
	}
	return b
}
