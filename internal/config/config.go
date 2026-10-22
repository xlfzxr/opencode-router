package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var raw string
	if err := node.Decode(&raw); err != nil {
		return err
	}
	if raw == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Value() time.Duration { return time.Duration(d) }

type ProxyEntry struct {
	ID  string `yaml:"id"`
	URL string `yaml:"url"`
}

type Proxy struct {
	Enabled            bool         `yaml:"enabled"`
	Strict             bool         `yaml:"strict"`
	URL                string       `yaml:"url"`
	ConnectionProxyURL string       `yaml:"connection_proxy_url"`
	NoProxy            string       `yaml:"no_proxy"`
	UseEnv             bool         `yaml:"use_env"`
	PoolMaxTransports  int          `yaml:"pool_max_transports"`
	Pool               []ProxyEntry `yaml:"pool"`
}

type Config struct {
	Server struct {
		Addr              string   `yaml:"addr"`
		ReadHeaderTimeout Duration `yaml:"read_header_timeout"`
		ShutdownTimeout   Duration `yaml:"shutdown_timeout"`
	} `yaml:"server"`

	Upstream struct {
		BaseURL           string   `yaml:"base_url"`
		HeaderTimeout     Duration `yaml:"header_timeout"`
		RequestTimeout    Duration `yaml:"request_timeout"`
		StreamIdleTimeout Duration `yaml:"stream_idle_timeout"`
		CatalogTTL        Duration `yaml:"catalog_ttl"`
	} `yaml:"upstream"`

	Session struct {
		TTL        Duration `yaml:"ttl"`
		MaxEntries int      `yaml:"max_entries"`
	} `yaml:"session"`

	Usage struct {
		Enabled       bool   `yaml:"enabled"`
		Path          string `yaml:"path"`
		RetentionDays int    `yaml:"retention_days"`
	} `yaml:"usage"`

	Proxy Proxy `yaml:"proxy"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{}
	cfg.Server.Addr = "127.0.0.1:3002"
	cfg.Server.ReadHeaderTimeout = Duration(10 * time.Second)
	cfg.Server.ShutdownTimeout = Duration(10 * time.Second)
	cfg.Upstream.BaseURL = "https://opencode.ai"
	cfg.Upstream.HeaderTimeout = Duration(30 * time.Second)
	cfg.Upstream.RequestTimeout = Duration(120 * time.Second)
	cfg.Upstream.StreamIdleTimeout = Duration(25 * time.Second)
	cfg.Upstream.CatalogTTL = Duration(10 * time.Minute)
	cfg.Session.TTL = Duration(2 * time.Hour)
	cfg.Session.MaxEntries = 1000
	cfg.Usage.Enabled = true
	cfg.Usage.RetentionDays = 60
	cfg.Proxy.UseEnv = true
	cfg.Proxy.PoolMaxTransports = 20

	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.Server.Addr == "" {
		cfg.Server.Addr = "127.0.0.1:3002"
	}
	if cfg.Session.MaxEntries <= 0 {
		cfg.Session.MaxEntries = 1000
	}
	if cfg.Proxy.PoolMaxTransports <= 0 {
		cfg.Proxy.PoolMaxTransports = 20
	}
	if cfg.Usage.RetentionDays < 0 {
		cfg.Usage.RetentionDays = 0
	}
	return cfg, nil
}
