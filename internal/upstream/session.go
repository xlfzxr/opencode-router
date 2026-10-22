package upstream

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	sessionPrefix = "ses_"
	sessionMask   = (1 << 48) - 1
	timeMask      = (1 << 36) - 1
	base62        = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

var sessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

type sessionEntry struct {
	id   string
	last time.Time
}

type SessionStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]*sessionEntry
}

func NewSessionStore(ttl time.Duration, max int) *SessionStore {
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	if max <= 0 {
		max = 1000
	}
	return &SessionStore{ttl: ttl, max: max, entries: make(map[string]*sessionEntry)}
}

// Resolve applies 9router's session priority: a valid native session header
// wins, then hinted sessions are translated deterministically, and finally one
// stable canonical session per downstream identity is reused. Minting a fresh
// session per request burns the free-tier quota and surfaces as 429.
func (s *SessionStore) Resolve(body map[string]any, header http.Header, providerSessionID, clientTool string) string {
	if clientTool == "" {
		clientTool = header.Get("X-Opencode-Client")
	}
	if clientTool == "" {
		clientTool = "generic"
	}

	incoming := strings.TrimSpace(header.Get("X-Opencode-Session"))
	if sessionRe.MatchString(incoming) {
		return incoming
	}
	if strings.TrimSpace(providerSessionID) != "" {
		return TranslateSessionID(providerSessionID, clientTool)
	}
	if incoming != "" && len(incoming) <= 256 {
		return TranslateSessionID(incoming, clientTool)
	}
	if hint := bodySessionHint(body); hint != "" {
		return TranslateSessionID(hint, clientTool)
	}
	return s.Stable(identityKey(header))
}

// Stable returns the canonical session for key, evicting the oldest entry when
// the store is full and expired entries lazily.
func (s *SessionStore) Stable(key string) string {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	if entry, ok := s.entries[key]; ok {
		entry.last = now
		return entry.id
	}
	s.evict(now)
	entry := &sessionEntry{id: GenerateSessionID(now), last: now}
	s.entries[key] = entry
	return entry.id
}

func (s *SessionStore) evict(now time.Time) {
	if len(s.entries) < s.max {
		return
	}
	var oldestKey string
	var oldest time.Time
	for key, entry := range s.entries {
		if now.Sub(entry.last) > s.ttl {
			delete(s.entries, key)
			continue
		}
		if oldestKey == "" || entry.last.Before(oldest) {
			oldestKey, oldest = key, entry.last
		}
	}
	if len(s.entries) >= s.max && oldestKey != "" {
		delete(s.entries, oldestKey)
	}
}

var genMu sync.Mutex
var lastStamp int64
var counter uint64

// GenerateSessionID produces the OpenCode v2 CLI shape:
// ses_ + 12 hex of ~(unixMilli*0x1000+counter), low 48 bits + 14 base62 chars.
func GenerateSessionID(now time.Time) string {
	stamp := now.UnixMilli()
	genMu.Lock()
	if stamp != lastStamp {
		lastStamp = stamp
		counter = 0
	}
	counter++
	c := counter
	genMu.Unlock()

	prefix := (^(uint64(stamp)*0x1000 + c)) & sessionMask
	return fmt.Sprintf("%s%012x%s", sessionPrefix, prefix, randomBase62(14))
}

// TranslateSessionID maps any downstream conversation id onto a deterministic
// OpenCode-compatible session (counter pinned to 1, like the CLI sends).
func TranslateSessionID(id, clientTool string) string {
	id = strings.TrimSpace(id)
	if sessionRe.MatchString(id) {
		return id
	}
	if clientTool == "" {
		clientTool = "generic"
	}
	sum := sha256.Sum256([]byte("opencode\x00" + clientTool + "\x00" + id))

	var padded [8]byte
	copy(padded[3:], sum[:5])
	timeVal := binary.BigEndian.Uint64(padded[:]) & timeMask
	prefix := (^(timeVal*0x1000 + 1)) & sessionMask

	var suffix strings.Builder
	for i := 6; i < 20; i++ {
		suffix.WriteByte(base62[int(sum[i])%62])
	}
	return fmt.Sprintf("%s%012x%s", sessionPrefix, prefix, suffix.String())
}

func randomBase62(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		for i := range buf {
			buf[i] = byte(time.Now().UnixNano() >> (i * 5))
		}
	}
	for i, b := range buf {
		buf[i] = base62[int(b)%62]
	}
	return string(buf)
}

func identityKey(header http.Header) string {
	auth := strings.TrimSpace(header.Get("Authorization"))
	if auth == "" {
		auth = strings.TrimSpace(header.Get("X-Api-Key"))
	}
	if auth == "" {
		return "opencode:default"
	}
	sum := sha256.Sum256([]byte(auth))
	return "opencode:auth:" + hex.EncodeToString(sum[:16])
}

// bodySessionHint mirrors 9router's bodyHasSessionHints plus a deterministic
// digest so the same conversation keeps the same session.
func bodySessionHint(body map[string]any) string {
	if body == nil {
		return ""
	}
	for _, key := range []string{"session_id", "conversation_id", "prompt_cache_key"} {
		if v, ok := body[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if metadata, ok := body["metadata"].(map[string]any); ok {
		if v, ok := metadata["user_id"].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	if request, ok := body["request"].(map[string]any); ok {
		if v, ok := request["sessionId"]; ok && fmt.Sprint(v) != "" {
			return fmt.Sprint(v)
		}
	}
	var assistant strings.Builder
	for _, msg := range messagesOf(body) {
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			assistant.WriteString(content)
		case []any:
			for _, part := range content {
				if m, ok := part.(map[string]any); ok {
					if text, ok := m["text"].(string); ok {
						assistant.WriteString(text)
					}
				}
			}
		}
		if assistant.Len() >= 50 {
			sum := sha256.Sum256([]byte(assistant.String()))
			return "history:" + hex.EncodeToString(sum[:16])
		}
	}
	return ""
}

func messagesOf(body map[string]any) []map[string]any {
	raw, ok := body["messages"].([]any)
	if !ok {
		if raw, ok = body["input"].([]any); !ok {
			return nil
		}
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
