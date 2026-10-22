package egress

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opencode-router/internal/config"
)

const (
	SourceDirect   = "direct"
	SourceOverride = "override"
	SourceURL      = "url"
	SourcePool     = "pool"
	SourceEnv      = "env"
)

type Selection struct {
	URL    string
	Source string
}

func (s Selection) IsDirect() bool { return s.URL == "" }

type Manager struct {
	cfg           config.Proxy
	headerTimeout time.Duration
	rr            atomic.Uint64

	mu    sync.Mutex
	cache map[string]*http.Transport
	order []string
	max   int
}

func New(cfg config.Proxy, headerTimeout time.Duration) *Manager {
	max := cfg.PoolMaxTransports
	if max <= 0 {
		max = 20
	}
	return &Manager{
		cfg:           cfg,
		headerTimeout: headerTimeout,
		cache:         make(map[string]*http.Transport),
		max:           max,
	}
}

func (m *Manager) Strict() bool { return m.cfg.Strict }

// Select mirrors 9router's proxy resolution order:
// per-request override > connection proxy > provider proxy > pool > env > direct.
func (m *Manager) Select(target, override, circuit string) (Selection, error) {
	if override != "" {
		return m.fromOverride(target, override, circuit)
	}
	if p := strings.TrimSpace(m.cfg.ConnectionProxyURL); p != "" {
		return m.fromProxyURL(target, p, SourceOverride, circuit)
	}
	if p := strings.TrimSpace(m.cfg.URL); p != "" {
		return m.fromProxyURL(target, p, SourceURL, circuit)
	}
	if m.cfg.Enabled && len(m.cfg.Pool) > 0 {
		idx := int(m.rr.Add(1)-1) % len(m.cfg.Pool)
		return m.fromProxyURL(target, m.cfg.Pool[idx].URL, SourcePool+":"+m.cfg.Pool[idx].ID, circuit)
	}
	if m.cfg.UseEnv {
		if p := envProxy(target, m.cfg.NoProxy); p != "" {
			return m.fromProxyURL(target, p, SourceEnv, circuit)
		}
	}
	return Selection{Source: SourceDirect}, nil
}

func (m *Manager) fromOverride(target, override, circuit string) (Selection, error) {
	for _, entry := range m.cfg.Pool {
		if strings.EqualFold(entry.ID, override) {
			return m.fromProxyURL(target, entry.URL, SourceOverride+":"+entry.ID, circuit)
		}
	}
	if looksLikeURL(override) {
		return m.fromProxyURL(target, override, SourceOverride, circuit)
	}
	err := fmt.Errorf("unknown proxy %q (not a pool id or URL)", override)
	if m.cfg.Strict {
		return Selection{}, err
	}
	return Selection{Source: SourceDirect}, nil
}

func (m *Manager) fromProxyURL(target, raw, source, circuit string) (Selection, error) {
	normalized := normalizeProxyURL(raw)
	if normalized == "" {
		if m.cfg.Strict {
			return Selection{}, fmt.Errorf("proxy %q is empty", raw)
		}
		return Selection{Source: SourceDirect}, nil
	}
	u, err := url.Parse(normalized)
	if err != nil || u.Host == "" {
		if m.cfg.Strict {
			return Selection{}, fmt.Errorf("invalid proxy %q: %w", raw, err)
		}
		return Selection{Source: SourceDirect}, nil
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		if m.cfg.Strict {
			return Selection{}, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
		return Selection{Source: SourceDirect}, nil
	}
	if circuit != "" {
		u.User = url.User(circuit)
	}
	_ = target
	return Selection{URL: u.String(), Source: source}, nil
}

// Transport returns a cached transport for the selection. Direct selections use
// a shared, tuned transport (never the http.DefaultTransport pool size of 2).
func (m *Manager) Transport(sel Selection) (*http.Transport, error) {
	key := sel.URL
	if key == "" {
		key = "direct"
	}
	m.mu.Lock()
	if t, ok := m.cache[key]; ok {
		m.mu.Unlock()
		return t, nil
	}
	m.mu.Unlock()

	var proxyURL *url.URL
	if sel.URL != "" {
		u, err := url.Parse(sel.URL)
		if err != nil {
			return nil, err
		}
		proxyURL = u
	}

	t := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: m.headerTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true, // SSE must never be buffered by gzip
	}
	if proxyURL != nil {
		t.Proxy = http.ProxyURL(proxyURL)
	}

	m.mu.Lock()
	if existing, ok := m.cache[key]; ok {
		m.mu.Unlock()
		return existing, nil
	}
	if len(m.order) >= m.max {
		oldest := m.order[0]
		m.order = m.order[1:]
		if old, ok := m.cache[oldest]; ok {
			old.CloseIdleConnections()
			delete(m.cache, oldest)
		}
	}
	m.cache[key] = t
	m.order = append(m.order, key)
	m.mu.Unlock()
	return t, nil
}

func (m *Manager) CloseIdle() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range m.cache {
		t.CloseIdleConnections()
	}
}

func looksLikeURL(v string) bool {
	return strings.Contains(v, "://") || strings.Contains(v, ":")
}

func normalizeProxyURL(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if !strings.Contains(v, "://") {
		return "http://" + v
	}
	return v
}

func envProxy(target, noProxy string) string {
	if matchNoProxy(target, noProxy) {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(target), "https:") {
		for _, key := range []string{"HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
			if v := os.Getenv(key); v != "" {
				return v
			}
		}
		return ""
	}
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}
	return ""
}

func matchNoProxy(target, noProxy string) bool {
	list := strings.TrimSpace(noProxy)
	if list == "" {
		list = strings.TrimSpace(os.Getenv("NO_PROXY"))
		if list == "" {
			list = strings.TrimSpace(os.Getenv("no_proxy"))
		}
	}
	if list == "" {
		return false
	}
	host := ""
	if u, err := url.Parse(target); err == nil {
		host = strings.ToLower(u.Hostname())
	}
	if host == "" {
		return false
	}
	for _, raw := range strings.Split(list, ",") {
		pattern := strings.ToLower(strings.TrimSpace(raw))
		if pattern == "" {
			continue
		}
		if pattern == "*" {
			return true
		}
		if strings.HasPrefix(pattern, ".") {
			if strings.HasSuffix(host, pattern) || host == pattern[1:] {
				return true
			}
			continue
		}
		if host == pattern || strings.HasSuffix(host, "."+pattern) {
			return true
		}
	}
	return false
}
