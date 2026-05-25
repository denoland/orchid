package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the sqlite-backed persistence layer for every piece of orchid
// state that survives across process restarts:
//
//   - Job records (per inbox issue): the only piece of state that lives
//     entirely on disk between ticks.
//   - mention_cursor / maintainers: cached state used by the mention
//     watcher so we don't rescan from epoch on every boot.
//   - dashboard snap blob (the opaque "card layout" the canvas posts
//     to /api/snap) plus the previous good copy as snap.bak.
//
// One DB file replaces the old state.json + snap.json + snap.json.bak
// fan-out. WAL + busy_timeout keeps reads non-blocking while a writer
// holds the write lock; the orchestrator only ever has one writer goroutine
// at a time (callers must hold State.mu before calling Save), so the
// store's own mutex is just belt-and-suspenders against direct misuse.
type Store struct {
	mu sync.Mutex
	db *sql.DB
}

func openStore(path string) (*Store, error) {
	// Open with WAL for non-blocking reads and a generous busy timeout
	// so the SPA polling the dashboard never races a tick-time Save.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// The schema is two tables: one row per tracked job, and a small
	// key/value table for everything else (mention cursor, maintainer
	// cache, dashboard snap blob + its backup).
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS jobs (
  issue INTEGER PRIMARY KEY,
  data  BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS kv (
  key   TEXT PRIMARY KEY,
  value BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_daily (
  date           TEXT NOT NULL,
  session_id     TEXT NOT NULL,
  model          TEXT NOT NULL,
  input_tokens   INTEGER NOT NULL DEFAULT 0,
  cache_creation INTEGER NOT NULL DEFAULT 0,
  cache_read     INTEGER NOT NULL DEFAULT 0,
  output_tokens  INTEGER NOT NULL DEFAULT 0,
  cost_usd       REAL    NOT NULL DEFAULT 0,
  PRIMARY KEY (date, session_id, model)
);
CREATE INDEX IF NOT EXISTS usage_daily_date ON usage_daily(date);`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// LoadState reads every persisted job + mention cursor + maintainer cache
// in a single read transaction so a partially-applied Save can't show
// torn state. Returns a zero-valued result (empty Jobs map, nil cursor,
// nil maintainers) for a fresh DB.
func (s *Store) LoadState() (jobs map[int]*Job, cursor *time.Time, maint *MaintainerCache, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs = map[int]*Job{}
	rows, err := s.db.Query("SELECT issue, data FROM jobs")
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n int
		var b []byte
		if err := rows.Scan(&n, &b); err != nil {
			return nil, nil, nil, err
		}
		j := &Job{}
		if jerr := json.Unmarshal(b, j); jerr != nil {
			return nil, nil, nil, fmt.Errorf("jobs row %d: %w", n, jerr)
		}
		jobs[n] = j
	}
	if rerr := rows.Err(); rerr != nil {
		return nil, nil, nil, rerr
	}
	cv, ok, kerr := s.getKV("mention_cursor")
	if kerr != nil {
		return nil, nil, nil, kerr
	}
	if ok {
		t, perr := time.Parse(time.RFC3339Nano, string(cv))
		if perr == nil {
			cursor = &t
		}
	}
	mv, ok, kerr := s.getKV("maintainers")
	if kerr != nil {
		return nil, nil, nil, kerr
	}
	if ok {
		m := &MaintainerCache{}
		if jerr := json.Unmarshal(mv, m); jerr == nil {
			maint = m
		}
	}
	return jobs, cursor, maint, nil
}

// getKV is a small helper for single-row reads. Returns (value, true)
// on hit, (nil, false) on miss, error otherwise. Caller must hold s.mu.
func (s *Store) getKV(key string) ([]byte, bool, error) {
	var v []byte
	err := s.db.QueryRow("SELECT value FROM kv WHERE key=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return v, true, nil
}

// SaveState replaces the persisted job set + cursor + maintainer cache
// atomically. The old json file just wrote the entire state on every
// modification; the sqlite port preserves that simple semantics because
// the persisted set is always tiny (~30 jobs cap).
//
// Caller must hold State.mu.
func (s *Store) SaveState(jobs map[int]*Job, cursor *time.Time, maint *MaintainerCache) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM jobs"); err != nil {
		return err
	}
	if len(jobs) > 0 {
		stmt, err := tx.Prepare("INSERT INTO jobs(issue,data) VALUES(?,?)")
		if err != nil {
			return err
		}
		for n, j := range jobs {
			b, jerr := json.Marshal(j)
			if jerr != nil {
				stmt.Close()
				return jerr
			}
			if _, err := stmt.Exec(n, b); err != nil {
				stmt.Close()
				return err
			}
		}
		stmt.Close()
	}
	if cursor != nil {
		if err := upsertKVTx(tx, "mention_cursor", []byte(cursor.Format(time.RFC3339Nano))); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec("DELETE FROM kv WHERE key='mention_cursor'"); err != nil {
			return err
		}
	}
	if maint != nil {
		b, err := json.Marshal(maint)
		if err != nil {
			return err
		}
		if err := upsertKVTx(tx, "maintainers", b); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec("DELETE FROM kv WHERE key='maintainers'"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func upsertKVTx(tx *sql.Tx, key string, value []byte) error {
	_, err := tx.Exec(
		"INSERT INTO kv(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		key, value,
	)
	return err
}

// GetSnap returns the opaque dashboard layout blob (whatever the canvas
// last PUT to /api/snap), or nil if no snap has been saved yet.
// GetKV reads an arbitrary kv row. Returns (nil, nil) when the key is
// absent — callers that need to distinguish should check len(value).
// Public counterpart to getKV for use by other watchers (assignment
// poller, future mention extensions, …) that want a free-form
// per-orch persistence slot without inventing a new table.
func (s *Store) GetKV(key string) ([]byte, error) {
	b, _, err := s.getKV(key)
	return b, err
}

// PutKV writes an arbitrary kv row, overwriting any prior value.
// Same idempotent semantics as PutSnap's underlying ON CONFLICT DO
// UPDATE — callers don't need a sentinel for "first write vs update".
func (s *Store) PutKV(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := upsertKVTx(tx, key, value); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// UpsertUsageDaily writes one row per (date, session_id, model). The
// scanner replays jsonl files idempotently — same input always yields
// the same total — so ON CONFLICT REPLACE is safe and lets us re-scan
// without dedupe bookkeeping.
func (s *Store) UpsertUsageDaily(date, sessionID, model string, inputT, cacheCreate, cacheRead, outputT int64, costUSD float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO usage_daily (date, session_id, model, input_tokens, cache_creation, cache_read, output_tokens, cost_usd)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(date, session_id, model) DO UPDATE SET
			input_tokens   = excluded.input_tokens,
			cache_creation = excluded.cache_creation,
			cache_read     = excluded.cache_read,
			output_tokens  = excluded.output_tokens,
			cost_usd       = excluded.cost_usd`,
		date, sessionID, model, inputT, cacheCreate, cacheRead, outputT, costUSD)
	return err
}

// UsageDailyRow is one bucket from the table — per session × model × day.
type UsageDailyRow struct {
	Date          string  `json:"date"`
	SessionID     string  `json:"session_id"`
	Model         string  `json:"model"`
	InputTokens   int64   `json:"input_tokens"`
	CacheCreation int64   `json:"cache_creation"`
	CacheRead     int64   `json:"cache_read"`
	OutputTokens  int64   `json:"output_tokens"`
	CostUSD       float64 `json:"cost_usd"`
}

// LoadUsageHistory returns all rows on or after `sinceDate` (YYYY-MM-DD
// UTC). Caller aggregates client-side; rows stay normalised so the
// dashboard can render per-day totals, per-model splits, and
// per-session drill-ins from the same payload.
func (s *Store) LoadUsageHistory(sinceDate string) ([]UsageDailyRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT date, session_id, model, input_tokens, cache_creation, cache_read, output_tokens, cost_usd
		FROM usage_daily WHERE date >= ? ORDER BY date ASC`, sinceDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageDailyRow
	for rows.Next() {
		var r UsageDailyRow
		if err := rows.Scan(&r.Date, &r.SessionID, &r.Model, &r.InputTokens, &r.CacheCreation, &r.CacheRead, &r.OutputTokens, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetSnap() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok, err := s.getKV("snap")
	if err != nil || !ok {
		return nil, err
	}
	return v, nil
}

// PutSnap replaces the dashboard layout blob and rotates the previous
// value into snap.bak so a buggy client clobbering positions doesn't
// destroy the last known-good layout. Mirrors the on-disk snap.json /
// snap.json.bak pattern the old code used.
func (s *Store) PutSnap(body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prev []byte
	if err := tx.QueryRow("SELECT value FROM kv WHERE key='snap'").Scan(&prev); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if len(prev) > 0 {
		if err := upsertKVTx(tx, "snap.bak", prev); err != nil {
			return err
		}
	}
	if err := upsertKVTx(tx, "snap", body); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateLegacyJSON imports the pre-sqlite state.json file (and any
// snap.json / snap.json.bak siblings) into this store if its rows are
// empty and the legacy files exist. The legacy files are renamed with
// a .migrated suffix so we never re-import them on subsequent boots.
//
// Skips silently when there's nothing to do — a fresh deploy without
// any prior state is the common case.
func (s *Store) migrateLegacyJSON(statePath, snapPath string) error {
	if !s.isEmpty() {
		return nil
	}
	// state.json — top-level shape was the old State struct's json form.
	if b, err := os.ReadFile(statePath); err == nil {
		var legacy struct {
			Jobs          map[int]*Job     `json:"jobs"`
			MentionCursor *time.Time       `json:"mention_cursor,omitempty"`
			Maintainers   *MaintainerCache `json:"maintainers,omitempty"`
		}
		if jerr := json.Unmarshal(b, &legacy); jerr != nil {
			return fmt.Errorf("migrate state.json: %w", jerr)
		}
		if legacy.Jobs == nil {
			legacy.Jobs = map[int]*Job{}
		}
		if err := s.SaveState(legacy.Jobs, legacy.MentionCursor, legacy.Maintainers); err != nil {
			return fmt.Errorf("migrate state.json: write: %w", err)
		}
		_ = os.Rename(statePath, statePath+".migrated")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// snap.json — opaque blob; pass through verbatim.
	if b, err := os.ReadFile(snapPath); err == nil {
		s.mu.Lock()
		_, _ = s.db.Exec("INSERT INTO kv(key,value) VALUES('snap',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", b)
		s.mu.Unlock()
		_ = os.Rename(snapPath, snapPath+".migrated")
	}
	if b, err := os.ReadFile(snapPath + ".bak"); err == nil {
		s.mu.Lock()
		_, _ = s.db.Exec("INSERT INTO kv(key,value) VALUES('snap.bak',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", b)
		s.mu.Unlock()
		_ = os.Rename(snapPath+".bak", snapPath+".bak.migrated")
	}
	return nil
}

// isEmpty returns true when there are no jobs and no kv rows. Used by
// the legacy-import path so we only ever migrate into a virgin database.
func (s *Store) isEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&n); err != nil || n > 0 {
		return false
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM kv").Scan(&n); err != nil || n > 0 {
		return false
	}
	return true
}
