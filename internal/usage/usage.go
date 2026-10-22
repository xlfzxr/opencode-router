// Package usage stores per-request token and latency statistics for the local
// dashboard. Writes are queued so a slow disk never delays an SSE stream, and
// the SQLite file is optional: a disabled store degrades to a no-op.
package usage

import (
	"database/sql"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxErrorLen  = 300
	maxModelLen  = 200
	maxProxyLen  = 200
	defaultLimit = 50
)

const schema = `
CREATE TABLE IF NOT EXISTS requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	model TEXT NOT NULL DEFAULT '',
	endpoint TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'ok',
	http_status INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	prompt_tokens INTEGER NOT NULL DEFAULT 0,
	completion_tokens INTEGER NOT NULL DEFAULT 0,
	stream INTEGER NOT NULL DEFAULT 0,
	proxy TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_model ON requests(model);
`

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

type Store struct {
	db        *sql.DB
	ch        chan Entry
	wg        sync.WaitGroup
	disabled  bool
	dropOnce  sync.Once
	closeOnce sync.Once
}

// Disabled returns a store that accepts records but keeps nothing.
func Disabled() *Store { return &Store{disabled: true} }

// DefaultPath is where usage data lives unless config overrides it.
func DefaultPath() string {
	if p := os.Getenv("OCR_USAGE_DB"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "opencode-router", "usage.db")
}

// Open creates the database (and its parent directory) if needed. With
// enabled=false it returns a no-op store. retentionDays <= 0 keeps everything.
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
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create usage dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	var mode string
	_ = db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode)
	_, _ = db.Exec("PRAGMA busy_timeout=5000")
	_, _ = db.Exec("PRAGMA synchronous=NORMAL")
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init usage schema: %w", err)
	}
	if retentionDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -retentionDays).UnixMilli()
		_, _ = db.Exec(`DELETE FROM requests WHERE ts < ?`, cutoff)
	}

	s := &Store{db: db, ch: make(chan Entry, 256)}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

func (s *Store) writer() {
	defer s.wg.Done()
	for entry := range s.ch {
		s.insert(entry)
	}
}

// Record queues a request. It never blocks the caller; records are dropped only
// if the writer cannot keep up.
func (s *Store) Record(entry Entry) {
	if s == nil || s.db == nil {
		return
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}
	if entry.Status == "" {
		entry.Status = "ok"
	}
	select {
	case s.ch <- entry:
	default:
		s.dropOnce.Do(func() { log.Printf("[usage] write queue full; dropping records") })
	}
}

func (s *Store) insert(entry Entry) {
	_, err := s.db.Exec(`INSERT INTO requests
		(ts, model, endpoint, status, http_status, latency_ms, prompt_tokens, completion_tokens, stream, proxy, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entry.Time.UnixMilli(),
		truncate(entry.Model, maxModelLen),
		entry.Endpoint,
		entry.Status,
		entry.HTTPStatus,
		entry.LatencyMs,
		entry.PromptTokens,
		entry.CompletionTokens,
		boolInt(entry.Stream),
		truncate(entry.Proxy, maxProxyLen),
		truncate(entry.Error, maxErrorLen),
	)
	if err != nil {
		log.Printf("[usage] insert: %v", err)
	}
}

// Close drains pending writes and closes the database. Safe to call once.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		close(s.ch)
		s.wg.Wait()
		err = s.db.Close()
	})
	return err
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

// Stats aggregates one period. period is one of today, 24h, 7d, 30d, 60d, all.
func (s *Store) Stats(period string, now time.Time) (Stats, error) {
	start, bucket, err := periodStart(period, now)
	if err != nil {
		return Stats{}, err
	}
	out := Stats{Period: period, Bucket: bucket, Since: start}
	if s == nil || s.db == nil {
		out.ByModel = []ModelStats{}
		out.Series = []SeriesPoint{}
		out.Recent = []RecentEntry{}
		return out, nil
	}

	row := s.db.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN status = 'ok' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status <> 'ok' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN stream = 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(AVG(latency_ms), 0)
		FROM requests WHERE ts >= ?`, start.UnixMilli())
	var avg float64
	if err := row.Scan(&out.Totals.Requests, &out.Totals.OK, &out.Totals.Errors, &out.Totals.Streams,
		&out.Totals.PromptTokens, &out.Totals.CompletionTokens, &avg); err != nil {
		return Stats{}, err
	}
	out.Totals.TotalTokens = out.Totals.PromptTokens + out.Totals.CompletionTokens
	out.Totals.AvgLatencyMs = round1(avg)

	rows, err := s.db.Query(`SELECT model, COUNT(*),
		COALESCE(SUM(CASE WHEN status = 'ok' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0),
		COALESCE(AVG(latency_ms), 0)
		FROM requests WHERE ts >= ?
		GROUP BY model
		ORDER BY SUM(prompt_tokens + completion_tokens) DESC, COUNT(*) DESC
		LIMIT 50`, start.UnixMilli())
	if err != nil {
		return Stats{}, err
	}
	out.ByModel = []ModelStats{}
	for rows.Next() {
		var m ModelStats
		if err := rows.Scan(&m.Model, &m.Requests, &m.OK, &m.PromptTokens, &m.CompletionTokens, &avg); err != nil {
			rows.Close()
			return Stats{}, err
		}
		m.Errors = m.Requests - m.OK
		m.TotalTokens = m.PromptTokens + m.CompletionTokens
		m.AvgLatencyMs = round1(avg)
		out.ByModel = append(out.ByModel, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Stats{}, err
	}
	rows.Close()

	// "all" would otherwise start in 1970 and produce an unbounded series.
	if period == "all" {
		var minTS sql.NullInt64
		if err := s.db.QueryRow(`SELECT MIN(ts) FROM requests`).Scan(&minTS); err == nil && minTS.Valid {
			start = time.UnixMilli(minTS.Int64)
		}
		if now.Sub(start) > 400*24*time.Hour {
			start = now.AddDate(0, 0, -400)
		}
	}

	layout := "%Y-%m-%d"
	if bucket == "hour" {
		layout = "%Y-%m-%d %H:00"
	}
	seriesQuery := fmt.Sprintf(`SELECT strftime('%s', ts / 1000, 'unixepoch', 'localtime') AS label,
		COUNT(*),
		COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0)
		FROM requests WHERE ts >= ?
		GROUP BY label ORDER BY label`, layout)
	rows, err = s.db.Query(seriesQuery, start.UnixMilli())
	if err != nil {
		return Stats{}, err
	}
	points := map[string]SeriesPoint{}
	for rows.Next() {
		var p SeriesPoint
		if err := rows.Scan(&p.Label, &p.Requests, &p.PromptTokens, &p.CompletionTokens); err != nil {
			rows.Close()
			return Stats{}, err
		}
		points[p.Label] = p
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Stats{}, err
	}
	rows.Close()
	if out.Totals.Requests > 0 {
		out.Series = fillSeries(points, start, now, bucket)
	} else {
		out.Series = []SeriesPoint{}
	}

	out.Recent = []RecentEntry{}
	rows, err = s.db.Query(`SELECT id, ts, model, endpoint, status, http_status, latency_ms,
		prompt_tokens, completion_tokens, stream, proxy, error
		FROM requests ORDER BY id DESC LIMIT ?`, defaultLimit)
	if err != nil {
		return Stats{}, err
	}
	for rows.Next() {
		var e RecentEntry
		var stream int
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Model, &e.Endpoint, &e.Status, &e.HTTPStatus, &e.LatencyMs,
			&e.PromptTokens, &e.CompletionTokens, &stream, &e.Proxy, &e.Error); err != nil {
			rows.Close()
			return Stats{}, err
		}
		e.Time = time.UnixMilli(ts)
		e.Stream = stream != 0
		e.TotalTokens = e.PromptTokens + e.CompletionTokens
		out.Recent = append(out.Recent, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Stats{}, err
	}
	rows.Close()
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

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
