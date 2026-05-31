package orch

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

type Store struct {
	mu sync.Mutex
	db *sql.DB
}

func openStore(path string) (*Store, error) {
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
  issue          INTEGER NOT NULL DEFAULT 0,
  input_tokens   INTEGER NOT NULL DEFAULT 0,
  cache_creation INTEGER NOT NULL DEFAULT 0,
  cache_read     INTEGER NOT NULL DEFAULT 0,
  output_tokens  INTEGER NOT NULL DEFAULT 0,
  cost_usd       REAL    NOT NULL DEFAULT 0,
  PRIMARY KEY (date, session_id, model)
);
CREATE INDEX IF NOT EXISTS usage_daily_date ON usage_daily(date);
CREATE TABLE IF NOT EXISTS closed_jobs (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  issue     INTEGER NOT NULL,
  state     TEXT NOT NULL,
  data      BLOB NOT NULL,
  closed_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS closed_jobs_closed_at ON closed_jobs(closed_at);
CREATE TABLE IF NOT EXISTS quota_samples (
  ts          INTEGER NOT NULL,
  five_pct    REAL    NOT NULL DEFAULT 0,
  five_reset  INTEGER NOT NULL DEFAULT 0,
  seven_pct   REAL    NOT NULL DEFAULT 0,
  seven_reset INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS quota_samples_ts ON quota_samples(ts);`); err != nil {
		_ = db.Close()
		return nil, err
	}
	_, _ = db.Exec(`ALTER TABLE usage_daily ADD COLUMN issue INTEGER NOT NULL DEFAULT 0`)
	// Per-agent quota: existing rows predate multi-agent and are all claude.
	_, _ = db.Exec(`ALTER TABLE quota_samples ADD COLUMN agent TEXT NOT NULL DEFAULT 'claude'`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS quota_samples_agent_ts ON quota_samples(agent, ts)`)
	// One-time dedupe: collapse closed_jobs to the newest row per issue (a past
	// flapping issue could have left hundreds). Insert path keeps it deduped.
	_, _ = db.Exec(`DELETE FROM closed_jobs WHERE id NOT IN (SELECT MAX(id) FROM closed_jobs GROUP BY issue)`)
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

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

func (s *Store) BackfillUsageIssue(sessionToIssue map[string]int) (int, error) {
	if len(sessionToIssue) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	stmt, err := tx.Prepare(`UPDATE usage_daily SET issue = ? WHERE session_id = ? AND issue = 0`)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	defer stmt.Close()
	updated := 0
	for sid, issue := range sessionToIssue {
		res, err := stmt.Exec(issue, sid)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		n, _ := res.RowsAffected()
		updated += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}

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

func (s *Store) UpsertUsageDaily(date, sessionID, model string, issue int, inputT, cacheCreate, cacheRead, outputT int64, costUSD float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO usage_daily (date, session_id, model, issue, input_tokens, cache_creation, cache_read, output_tokens, cost_usd)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(date, session_id, model) DO UPDATE SET
			issue          = excluded.issue,
			input_tokens   = excluded.input_tokens,
			cache_creation = excluded.cache_creation,
			cache_read     = excluded.cache_read,
			output_tokens  = excluded.output_tokens,
			cost_usd       = excluded.cost_usd`,
		date, sessionID, model, issue, inputT, cacheCreate, cacheRead, outputT, costUSD)
	return err
}

// UsageDailyRow is one bucket from the table — per session × model × day.
type UsageDailyRow struct {
	Date          string  `json:"date"`
	SessionID     string  `json:"session_id"`
	Model         string  `json:"model"`
	Issue         int     `json:"issue,omitempty"`
	InputTokens   int64   `json:"input_tokens"`
	CacheCreation int64   `json:"cache_creation"`
	CacheRead     int64   `json:"cache_read"`
	OutputTokens  int64   `json:"output_tokens"`
	CostUSD       float64 `json:"cost_usd"`
}

func (s *Store) LoadUsageHistory(sinceDate string) ([]UsageDailyRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT date, session_id, model, issue, input_tokens, cache_creation, cache_read, output_tokens, cost_usd
		FROM usage_daily WHERE date >= ? ORDER BY date ASC`, sinceDate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageDailyRow
	for rows.Next() {
		var r UsageDailyRow
		if err := rows.Scan(&r.Date, &r.SessionID, &r.Model, &r.Issue, &r.InputTokens, &r.CacheCreation, &r.CacheRead, &r.OutputTokens, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertQuotaSample appends one quota reading (append-only timeseries). The
// QuotaSample type is defined in governor.go — it is the governor's input. The
// quota-sampling loop calls this every ~90s; the governor's burn-rate
// estimator reads windows back out via LoadQuotaSamples.
func (s *Store) InsertQuotaSample(q QuotaSample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	agent := q.Agent
	if agent == "" {
		agent = "claude"
	}
	_, err := s.db.Exec(`INSERT INTO quota_samples (agent, ts, five_pct, five_reset, seven_pct, seven_reset)
		VALUES (?,?,?,?,?,?)`,
		agent, q.Ts, q.FivePct, q.FiveReset, q.SevenPct, q.SevenReset)
	return err
}

// LoadQuotaSamples returns one agent's samples with ts >= sinceTs, oldest
// first. Rows written before the per-agent migration default to agent='claude'.
func (s *Store) LoadQuotaSamples(agent string, sinceTs int64) ([]QuotaSample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if agent == "" {
		agent = "claude"
	}
	rows, err := s.db.Query(`SELECT agent, ts, five_pct, five_reset, seven_pct, seven_reset
		FROM quota_samples WHERE agent = ? AND ts >= ? ORDER BY ts ASC`, agent, sinceTs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QuotaSample
	for rows.Next() {
		var q QuotaSample
		if err := rows.Scan(&q.Agent, &q.Ts, &q.FivePct, &q.FiveReset, &q.SevenPct, &q.SevenReset); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// PruneQuotaSamples deletes samples older than beforeTs. Called periodically
// from the sampling loop to keep ~14 days of history.
func (s *Store) PruneQuotaSamples(beforeTs int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM quota_samples WHERE ts < ?`, beforeTs)
	return err
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

func (s *Store) migrateLegacyJSON(statePath, snapPath string) error {
	if !s.isEmpty() {
		return nil
	}
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

// ClosedJobRow is one record from the closed_jobs table. The dashboard
// renders these as ghost cards with a "merged" / "closed" badge.
type ClosedJobRow struct {
	Issue    int    `json:"issue"`
	State    string `json:"state"` // "merged" | "closed"
	ClosedAt int64  `json:"closed_at"`
	Job      *Job   `json:"job"`
}

// PutClosedJob writes one row to closed_jobs. Called from the tick loop
// just before tearDown when a PR is detected as merged or closed.
func (s *Store) PutClosedJob(issue int, state string, j *Job) error {
	if j == nil {
		return nil
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// One row per issue: replace any prior closed record so a re-spawned /
	// re-closed issue (or a flapping one) can't pile up hundreds of ghosts.
	if _, err = s.db.Exec("DELETE FROM closed_jobs WHERE issue = ?", issue); err != nil {
		return err
	}
	_, err = s.db.Exec(
		"INSERT INTO closed_jobs (issue, state, data, closed_at) VALUES (?,?,?,?)",
		issue, state, b, time.Now().Unix(),
	)
	return err
}

// RecentClosedJobs returns closed-job rows whose closed_at is no older
// than maxAgeSecs ago, newest first. Limit caps the result count.
func (s *Store) RecentClosedJobs(maxAgeSecs int64, limit int) ([]ClosedJobRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Unix() - maxAgeSecs
	rows, err := s.db.Query(
		"SELECT issue, state, data, closed_at FROM closed_jobs WHERE closed_at >= ? ORDER BY closed_at DESC LIMIT ?",
		cutoff, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClosedJobRow
	for rows.Next() {
		var r ClosedJobRow
		var data []byte
		if err := rows.Scan(&r.Issue, &r.State, &data, &r.ClosedAt); err != nil {
			return nil, err
		}
		var j Job
		if err := json.Unmarshal(data, &j); err == nil {
			r.Job = &j
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
