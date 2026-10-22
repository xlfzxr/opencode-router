// Package routing ports the essence of 9router's combo engine to
// opencode-router: a single public model alias ("ocr") fans out to a pool of
// real upstream models, ordered by strategy (fallback / round-robin),
// capability fit (vision/pdf/audio/video), and recent-failure cooldowns.
package routing

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrZeroContent reports a 2xx upstream response that carried no usable
// content, so the caller may fall back to the next candidate (mirrors
// 9router's isZeroContentResponse).
var ErrZeroContent = errors.New("upstream returned zero usable content")

// Strategy controls how the candidate pool is ordered across requests.
type Strategy string

const (
	// StrategyFallback keeps the configured order: the first model is the
	// primary, the rest are fallbacks. Mirrors 9router's "fallback".
	StrategyFallback Strategy = "fallback"
	// StrategyRoundRobin rotates the primary across the pool with a sticky
	// window. Mirrors 9router's "round-robin" + sticky limit.
	StrategyRoundRobin Strategy = "round-robin"
)

// DefaultCandidates is the pool used when the config defines none, ordered by
// general strength on the OpenCode free tier.
var DefaultCandidates = []string{
	"glm-5.3-flash-free",
	"deepseek-v4.1-flash-free",
	"minimax-m3.1-flash-free",
	"nemotron-3.5-lightning-free",
	"mimo-v2.6-flash-free",
	"jev-1.13-free",
	"nemotron-3.5-super-vl-free",
	"flint-40b-free",
	"beam-free",
	"big-pickle",
}

// Cooldowns mirror 9router's config/errorConfig.js.
const (
	cooldownLong         = 2 * time.Minute
	cooldownShort        = 5 * time.Second
	cooldownTransient    = 30 * time.Second
	cooldownMaxRateLimit = 30 * time.Minute
	backoffBase          = 2 * time.Second
	backoffMax           = 5 * time.Minute
)

type Config struct {
	Enabled     bool
	Alias       string // public model id; empty defaults to "ocr"
	Strategy    string // "fallback" | "round-robin"
	StickyLimit int    // requests per model before round-robin advances
	AutoSwitch  bool   // float capability-matching models to the front
	MaxAttempts int    // max models tried per request; 0 = all
	Candidates  []string
}

func (c Config) withDefaults() Config {
	if strings.TrimSpace(c.Alias) == "" {
		c.Alias = "ocr"
	}
	if Strategy(c.Strategy) != StrategyRoundRobin {
		c.Strategy = string(StrategyFallback)
	}
	if c.StickyLimit <= 0 {
		c.StickyLimit = 1
	}
	if len(c.Candidates) == 0 {
		c.Candidates = append([]string(nil), DefaultCandidates...)
	}
	return c
}

type rotationState struct {
	index       int
	consecutive int
}

// Router owns alias resolution, candidate ordering, rotation state and
// failure cooldowns. It is safe for concurrent use.
type Router struct {
	cfg Config

	mu         sync.Mutex
	rotations  map[string]*rotationState
	cooldown   map[string]time.Time
	rateLevels map[string]int
}

func New(cfg Config) *Router {
	return &Router{
		cfg:        cfg.withDefaults(),
		rotations:  map[string]*rotationState{},
		cooldown:   map[string]time.Time{},
		rateLevels: map[string]int{},
	}
}

func (r *Router) Enabled() bool    { return r != nil && r.cfg.Enabled }
func (r *Router) Alias() string    { return r.cfg.Alias }
func (r *Router) MaxAttempts() int { return r.cfg.MaxAttempts }

// IsAlias reports whether the client asked for the public virtual model
// ("ocr" or "ocr/ocr").
func (r *Router) IsAlias(model string) bool {
	m := strings.TrimSpace(model)
	a := r.cfg.Alias
	return m == a || m == a+"/"+a
}

// StripAlias reduces a provider-qualified id "ocr/<real>" to <real>. A bare
// alias is returned unchanged.
func (r *Router) StripAlias(model string) string {
	m := strings.TrimSpace(model)
	if strings.HasPrefix(m, r.cfg.Alias+"/") {
		return strings.TrimPrefix(m, r.cfg.Alias+"/")
	}
	return m
}

// Plan returns the ordered list of candidate models to attempt for a request.
// available is the live upstream catalog (may be nil to skip filtering).
func (r *Router) Plan(body map[string]any, available []string) []string {
	if !r.Enabled() {
		return nil
	}

	candidates := append([]string(nil), r.cfg.Candidates...)
	if len(available) > 0 {
		if filtered := intersect(candidates, available); len(filtered) > 0 {
			candidates = filtered
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Round-robin rotation (9router: getRotatedModels).
	if Strategy(r.cfg.Strategy) == StrategyRoundRobin && len(candidates) > 1 {
		candidates = r.rotate(candidates)
	}

	// Capability auto-switch (9router: reorderByCapabilities). Never drops a model.
	if r.cfg.AutoSwitch {
		if required := detectRequiredCapabilities(body); len(required) > 0 {
			candidates = reorderByCapabilities(candidates, required)
		}
	}

	// Push recently-failed models to the back, preserving relative order.
	now := time.Now()
	warm := make([]string, 0, len(candidates))
	cooled := make([]string, 0, len(candidates))
	for _, m := range candidates {
		if until, ok := r.cooldown[m]; ok {
			if until.After(now) {
				cooled = append(cooled, m)
				continue
			}
			delete(r.cooldown, m)
		}
		warm = append(warm, m)
	}
	return append(warm, cooled...)
}

// rotate mirrors 9router's getRotatedModels: return models starting at the
// current index and advance the index once the sticky window is exhausted.
func (r *Router) rotate(models []string) []string {
	key := r.cfg.Alias
	state, ok := r.rotations[key]
	if !ok {
		state = &rotationState{}
		r.rotations[key] = state
	}
	idx := state.index % len(models)
	rotated := append(append([]string(nil), models[idx:]...), models[:idx]...)

	next := state.consecutive + 1
	if next >= r.cfg.StickyLimit {
		state.index = (idx + 1) % len(models)
		state.consecutive = 0
	} else {
		state.index = idx
		state.consecutive = next
	}
	return rotated
}

// MarkFailed records a failure for model and applies the matching cooldown.
// Mirrors 9router's checkFallbackError cooldowns; a non-matching request-scoped
// error records nothing.
func (r *Router) MarkFailed(model string, status int, errText string, retryAfter time.Duration) {
	if r == nil || model == "" {
		return
	}
	lower := strings.ToLower(errText)
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	var until time.Time
	switch {
	case strings.Contains(lower, "no credentials") || strings.Contains(lower, "improperly formed request"):
		until = now.Add(cooldownLong)
	case strings.Contains(lower, "request not allowed"):
		until = now.Add(cooldownShort)
	case status == 429 || retryAfter > 0 || hasBackoffText(lower):
		d := r.nextBackoffLocked(model)
		if retryAfter > d {
			d = retryAfter
		}
		if d > cooldownMaxRateLimit {
			d = cooldownMaxRateLimit
		}
		until = now.Add(d)
	case status == 401 || status == 402 || status == 403 || status == 404:
		until = now.Add(cooldownLong)
	case status >= 500 || status == 0:
		until = now.Add(cooldownTransient)
	default:
		return
	}
	if prev, ok := r.cooldown[model]; !ok || until.After(prev) {
		r.cooldown[model] = until
	}
}

func (r *Router) nextBackoffLocked(model string) time.Duration {
	level := r.rateLevels[model] + 1
	r.rateLevels[model] = level
	d := backoffBase
	for i := 1; i < level && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

// MarkOK clears a model's cooldown and rate-limit backoff after a success.
func (r *Router) MarkOK(model string) {
	if r == nil || model == "" {
		return
	}
	r.mu.Lock()
	delete(r.cooldown, model)
	delete(r.rateLevels, model)
	r.mu.Unlock()
}

func hasBackoffText(lower string) bool {
	for _, t := range []string{
		"rate limit", "too many requests", "quota exceeded", "capacity",
		"overloaded", "resource exhausted", "resource_exhausted",
	} {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}

// ShouldFallback ports 9router's checkFallbackError decision table: text rules
// first, then status rules, then the 4xx request-scoped exclusion.
func ShouldFallback(status int, errText string) bool {
	lower := strings.ToLower(errText)
	for _, t := range []string{
		"no credentials", "request not allowed", "improperly formed request",
		"rate limit", "too many requests", "quota exceeded", "capacity",
		"overloaded", "resource exhausted", "resource_exhausted",
	} {
		if strings.Contains(lower, t) {
			return true
		}
	}
	switch {
	case status == 401 || status == 402 || status == 403 || status == 404 || status == 429:
		return true
	case status >= 400 && status < 500:
		// Request-scoped client error (context overflow, malformed body...):
		// another model would fail the same way.
		return false
	default:
		// 5xx, or 0 for transport-level failures.
		return true
	}
}

func intersect(candidates, available []string) []string {
	set := make(map[string]struct{}, len(available))
	for _, id := range available {
		set[strings.ToLower(strings.TrimSpace(id))] = struct{}{}
	}
	out := make([]string, 0, len(candidates))
	for _, m := range candidates {
		if _, ok := set[strings.ToLower(strings.TrimSpace(m))]; ok {
			out = append(out, m)
		}
	}
	return out
}
