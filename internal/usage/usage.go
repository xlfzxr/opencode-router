// Package usage stores dashboard statistics in an atomic JSON snapshot.
// Records are queued so disk writes never delay an SSE stream.
package usage

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

const (
	maxErrorLen  = 300
	maxModelLen  = 200
	maxProxyLen  = 200
	defaultLimit = 50
)

// Entry is one completed downstream request.
type Entry struct {
	Time             time.Time
	Model            string
	Endpoint         string
	Status           string
	HTTPStatus       int
	LatencyMs        int64
	PromptTokens     int
	CompletionTokens int
	Stream           bool
	Proxy            string
	Error            string
}

type snapshot struct {
	Version int           `json:"version"`
	NextID  int64         `json:"nextId"`
	Entries []RecentEntry `json:"entries"`
}

type Store struct {
	mu            sync.RWMutex
	data          snapshot
	path          string
	retentionDays int
	dirty         bool
	lock          *os.File
	ch            chan Entry
	wg            sync.WaitGroup
	gate          sync.RWMutex // prevents Record racing with Close
	closed        bool
	dropOnce      sync.Once
	closeOnce     sync.Once
	closeErr      error
}

func Disabled() *Store { return &Store{} }

func DefaultPath() string {
	if p := os.Getenv("OCR_USAGE_PATH"); p != "" {
		return p
	}
	// Legacy override is accepted, but SQLite content is never overwritten.
	if p := os.Getenv("OCR_USAGE_DB"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "opencode-router", "usage.json")
}

// Open loads JSON and holds an advisory lock until Close. Corrupt or legacy
// SQLite files are rejected without modifying them. Retention is applied on
// startup and on each snapshot write; <=0 retains all history.
func Open(path string, enabled bool, retentionDays int) (*Store, error) {
	if !enabled {
		return Disabled(), nil
	}
	if path == "" {
		path = DefaultPath()
	}
	if path == "" {
		return nil, fmt.Errorf("usage path unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("usage file already locked: %w", err)
	}
	s := &Store{path: path, lock: lock, retentionDays: retentionDays, data: snapshot{Version: 1, NextID: 1, Entries: []RecentEntry{}}}
	fail := func(err error) (*Store, error) {
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return fail(fmt.Errorf("usage file is not valid JSON (SQLite is not migrated automatically); choose a new usage.path: %w", err))
		}
		if s.data.Version != 1 || s.data.NextID < 1 {
			return fail(fmt.Errorf("invalid or unsupported usage snapshot"))
		}
		var last int64
		for _, e := range s.data.Entries {
			if e.ID <= last || e.ID >= s.data.NextID {
				return fail(fmt.Errorf("invalid usage entry IDs"))
			}
			last = e.ID
		}
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	s.dirty = true
	if err := s.persist(); err != nil {
		return fail(err)
	}
	s.ch = make(chan Entry, 256)
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

func (s *Store) writer() {
	defer s.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case entry, ok := <-s.ch:
			if !ok {
				s.closeErr = s.persist()
				return
			}
			s.insert(entry)
		case <-ticker.C:
			if err := s.persist(); err != nil {
				log.Printf("[usage] save: %v", err)
			}
		}
	}
}

// Record is nonblocking; a full queue drops records rather than blocking SSE.
func (s *Store) Record(entry Entry) {
	if s == nil {
		return
	}
	s.gate.RLock()
	defer s.gate.RUnlock()
	if s.ch == nil || s.closed {
		return
	}
	select {
	case s.ch <- entry:
	default:
		s.dropOnce.Do(func() { log.Printf("[usage] write queue full; dropping records") })
	}
}

func (s *Store) insert(e Entry) {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Status == "" {
		e.Status = "ok"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.Entries = append(s.data.Entries, RecentEntry{
		ID: s.data.NextID, Time: e.Time, Model: truncate(e.Model, maxModelLen),
		Endpoint: e.Endpoint, Status: e.Status, HTTPStatus: e.HTTPStatus, LatencyMs: e.LatencyMs,
		PromptTokens: e.PromptTokens, CompletionTokens: e.CompletionTokens,
		TotalTokens: e.PromptTokens + e.CompletionTokens, Stream: e.Stream,
		Proxy: truncate(e.Proxy, maxProxyLen), Error: truncate(e.Error, maxErrorLen),
	})
	s.data.NextID++
	s.dirty = true
}

// persist writes a private temp file, fsyncs it, then atomically replaces the
// snapshot. A failed write leaves the old snapshot intact and is retried.
func (s *Store) persist() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -s.retentionDays)
		kept := make([]RecentEntry, 0, len(s.data.Entries))
		for _, e := range s.data.Entries {
			if !e.Time.Before(cutoff) {
				kept = append(kept, e)
			}
		}
		if len(kept) != len(s.data.Entries) {
			s.data.Entries = kept
			s.dirty = true
		}
	}
	if !s.dirty {
		return nil
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".usage-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(s.data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// Close drains the queue and flushes the snapshot. Safe alongside Record and
// safe to call more than once. The lock file stays in place (flock is released).
func (s *Store) Close() error {
	if s == nil || s.ch == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.gate.Lock()
		s.closed = true
		close(s.ch)
		s.gate.Unlock()
		s.wg.Wait()
		syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		if err := s.lock.Close(); s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

type Totals struct {
	Requests         int     `json:"requests"`
	OK               int     `json:"ok"`
	Errors           int     `json:"errors"`
	Streams          int     `json:"streams"`
	PromptTokens     int     `json:"promptTokens"`
	CompletionTokens int     `json:"completionTokens"`
	TotalTokens      int     `json:"totalTokens"`
	AvgLatencyMs     float64 `json:"avgLatencyMs"`
}

type ModelStats struct {
	Model            string  `json:"model"`
	Requests         int     `json:"requests"`
	OK               int     `json:"ok"`
	Errors           int     `json:"errors"`
	PromptTokens     int     `json:"promptTokens"`
	CompletionTokens int     `json:"completionTokens"`
	TotalTokens      int     `json:"totalTokens"`
	AvgLatencyMs     float64 `json:"avgLatencyMs"`
}

type SeriesPoint struct {
	Label            string `json:"label"`
	Requests         int    `json:"requests"`
	PromptTokens     int    `json:"promptTokens"`
	CompletionTokens int    `json:"completionTokens"`
}

type RecentEntry struct {
	ID               int64     `json:"id"`
	Time             time.Time `json:"time"`
	Model            string    `json:"model"`
	Endpoint         string    `json:"endpoint"`
	Status           string    `json:"status"`
	HTTPStatus       int       `json:"httpStatus"`
	LatencyMs        int64     `json:"latencyMs"`
	PromptTokens     int       `json:"promptTokens"`
	CompletionTokens int       `json:"completionTokens"`
	TotalTokens      int       `json:"totalTokens"`
	Stream           bool      `json:"stream"`
	Proxy            string    `json:"proxy"`
	Error            string    `json:"error,omitempty"`
}

type Stats struct {
	Period  string        `json:"period"`
	Bucket  string        `json:"bucket"`
	Since   time.Time     `json:"since"`
	Totals  Totals        `json:"totals"`
	ByModel []ModelStats  `json:"byModel"`
	Series  []SeriesPoint `json:"series"`
	Recent  []RecentEntry `json:"recent"`
}

// Stats preserves the dashboard response format of the former SQLite backend.
func (s *Store) Stats(period string, now time.Time) (Stats, error) {
	start, bucket, err := periodStart(period, now)
	if err != nil {
		return Stats{}, err
	}
	out := Stats{Period: period, Bucket: bucket, Since: start, ByModel: []ModelStats{}, Series: []SeriesPoint{}, Recent: []RecentEntry{}}
	if s == nil {
		return out, nil
	}
	s.mu.RLock()
	entries := append([]RecentEntry(nil), s.data.Entries...)
	s.mu.RUnlock()
	models := map[string]*ModelStats{}
	latency := map[string]int64{}
	var totalLatency int64
	for _, e := range entries {
		if e.Time.Before(start) {
			continue
		}
		t := &out.Totals
		t.Requests++
		if e.Status == "ok" {
			t.OK++
		} else {
			t.Errors++
		}
		if e.Stream {
			t.Streams++
		}
		t.PromptTokens += e.PromptTokens
		t.CompletionTokens += e.CompletionTokens
		totalLatency += e.LatencyMs
		m := models[e.Model]
		if m == nil {
			m = &ModelStats{Model: e.Model}
			models[e.Model] = m
		}
		m.Requests++
		if e.Status == "ok" {
			m.OK++
		} else {
			m.Errors++
		}
		m.PromptTokens += e.PromptTokens
		m.CompletionTokens += e.CompletionTokens
		latency[e.Model] += e.LatencyMs
	}
	out.Totals.TotalTokens = out.Totals.PromptTokens + out.Totals.CompletionTokens
	if out.Totals.Requests > 0 {
		out.Totals.AvgLatencyMs = round1(float64(totalLatency) / float64(out.Totals.Requests))
	}
	for _, m := range models {
		m.TotalTokens = m.PromptTokens + m.CompletionTokens
		m.AvgLatencyMs = round1(float64(latency[m.Model]) / float64(m.Requests))
		out.ByModel = append(out.ByModel, *m)
	}
	sort.Slice(out.ByModel, func(i, j int) bool {
		a, b := out.ByModel[i], out.ByModel[j]
		if a.TotalTokens != b.TotalTokens {
			return a.TotalTokens > b.TotalTokens
		}
		if a.Requests != b.Requests {
			return a.Requests > b.Requests
		}
		return a.Model < b.Model
	})
	if len(out.ByModel) > 50 {
		out.ByModel = out.ByModel[:50]
	}
	if period == "all" && len(entries) > 0 {
		start = entries[0].Time.In(now.Location())
		for _, e := range entries {
			if e.Time.Before(start) {
				start = e.Time.In(now.Location())
			}
		}
		if now.Sub(start) > 400*24*time.Hour {
			start = now.AddDate(0, 0, -400)
		}
	}
	points := map[string]SeriesPoint{}
	layout := "2006-01-02"
	if bucket == "hour" {
		layout = "2006-01-02 15:00"
	}
	for _, e := range entries {
		if e.Time.Before(start) {
			continue
		}
		label := e.Time.In(now.Location()).Format(layout)
		p := points[label]
		p.Label = label
		p.Requests++
		p.PromptTokens += e.PromptTokens
		p.CompletionTokens += e.CompletionTokens
		points[label] = p
	}
	if out.Totals.Requests > 0 {
		out.Series = fillSeries(points, start, now, bucket)
	}
	// Recent requests intentionally span all periods, matching the original API.
	for i := len(entries) - 1; i >= 0 && len(out.Recent) < defaultLimit; i-- {
		out.Recent = append(out.Recent, entries[i])
	}
	out.Since = start
	return out, nil
}

func periodStart(period string, now time.Time) (time.Time, string, error) {
	switch period {
	case "", "today":
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), "hour", nil
	case "24h":
		return now.Add(-24 * time.Hour), "hour", nil
	case "7d":
		return now.Add(-7 * 24 * time.Hour), "day", nil
	case "30d":
		return now.Add(-30 * 24 * time.Hour), "day", nil
	case "60d":
		return now.Add(-60 * 24 * time.Hour), "day", nil
	case "all":
		return time.Unix(0, 0), "day", nil
	default:
		return time.Time{}, "", fmt.Errorf("invalid period %q", period)
	}
}

func fillSeries(points map[string]SeriesPoint, start, now time.Time, bucket string) []SeriesPoint {
	out := []SeriesPoint{}
	if bucket == "hour" {
		t := time.Date(start.Year(), start.Month(), start.Day(), start.Hour(), 0, 0, 0, start.Location())
		// A malformed clock can otherwise loop for a very long time.
		for !t.After(now) && len(out) < 24*60 {
			label := t.Format("2006-01-02 15:00")
			p := points[label]
			p.Label = label
			out = append(out, p)
			t = t.Add(time.Hour)
		}
		return out
	}
	t := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	for !t.After(now) && len(out) < 800 {
		label := t.Format("2006-01-02")
		p := points[label]
		p.Label = label
		out = append(out, p)
		t = t.AddDate(0, 0, 1)
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
