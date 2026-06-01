package orch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type modelRate struct {
	Input, CacheCreate, CacheRead, Output float64
}

var modelPricing = []struct {
	pattern string
	rate    modelRate
}{
	{"opus", modelRate{Input: 15.0, CacheCreate: 18.75, CacheRead: 1.50, Output: 75.0}},
	{"sonnet", modelRate{Input: 3.0, CacheCreate: 3.75, CacheRead: 0.30, Output: 15.0}},
	{"haiku", modelRate{Input: 0.80, CacheCreate: 1.00, CacheRead: 0.08, Output: 4.00}},
}

// pricingFor returns the per-million-token rate for the given model
// string. Pattern matches the family name (sonnet/opus/haiku) — date
// suffixes and beta tags don't change the price tier.
func pricingFor(model string) modelRate {
	m := strings.ToLower(model)
	for _, p := range modelPricing {
		if strings.Contains(m, p.pattern) {
			return p.rate
		}
	}
	return modelPricing[1].rate
}

type jsonlMessage struct {
	Type      string    `json:"type"`
	Timestamp time.Time `json:"timestamp"`
	SessionID string    `json:"sessionId"`
	Message   struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type scanProgress struct {
	mu  sync.Mutex
	pos map[string]int64
}

func newScanProgress() *scanProgress { return &scanProgress{pos: map[string]int64{}} }
func (p *scanProgress) get(path string) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pos[path]
}
func (p *scanProgress) set(path string, n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pos[path] = n
}

func scanUsageHistory(ctx context.Context, projectsRoot string, store *Store, prog *scanProgress) error {
	type bucketKey struct{ date, session, model string }
	type bucket struct {
		in, cc, cr, out int64
		cost            float64
		issue           int
	}
	buckets := map[bucketKey]*bucket{}
	pathIssueRe := cwdIssueRe

	err := filepath.WalkDir(projectsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		fi, err := os.Stat(path)
		if err != nil {
			return nil
		}
		start := prog.get(path)
		if start >= fi.Size() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		if start > 0 {
			if _, err := f.Seek(start, 0); err != nil {
				return nil
			}
		}
		issueNum := 0
		if m := pathIssueRe.FindStringSubmatch(filepath.Base(filepath.Dir(path))); len(m) > 0 {
			if n, err := strconv.Atoi(m[1]); err == nil {
				issueNum = n
			}
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		var consumed int64 = start
		for sc.Scan() {
			line := sc.Bytes()
			consumed += int64(len(line)) + 1
			var m jsonlMessage
			if err := json.Unmarshal(line, &m); err != nil {
				continue
			}
			if m.Type != "assistant" {
				continue
			}
			if m.Message.Model == "" || m.Timestamp.IsZero() {
				continue
			}
			date := m.Timestamp.UTC().Format("2006-01-02")
			rate := pricingFor(m.Message.Model)
			u := m.Message.Usage
			cost := (float64(u.InputTokens)*rate.Input +
				float64(u.CacheCreationInputTokens)*rate.CacheCreate +
				float64(u.CacheReadInputTokens)*rate.CacheRead +
				float64(u.OutputTokens)*rate.Output) / 1_000_000.0
			k := bucketKey{date: date, session: m.SessionID, model: m.Message.Model}
			b := buckets[k]
			if b == nil {
				b = &bucket{}
				buckets[k] = b
			}
			b.in += u.InputTokens
			b.cc += u.CacheCreationInputTokens
			b.cr += u.CacheReadInputTokens
			b.out += u.OutputTokens
			b.cost += cost
			if issueNum > 0 {
				b.issue = issueNum
			}
		}
		prog.set(path, consumed)
		return nil
	})
	if err != nil {
		return err
	}
	for k, b := range buckets {
		existing, err := store.LoadUsageHistory(k.date)
		if err != nil {
			return err
		}
		var ex *UsageDailyRow
		for i := range existing {
			if existing[i].Date == k.date && existing[i].SessionID == k.session && existing[i].Model == k.model {
				ex = &existing[i]
				break
			}
		}
		var in, cc, cr, out int64
		var cost float64
		issue := b.issue
		if ex != nil {
			in, cc, cr, out, cost = ex.InputTokens, ex.CacheCreation, ex.CacheRead, ex.OutputTokens, ex.CostUSD
			if issue == 0 {
				issue = ex.Issue
			}
		}
		in += b.in
		cc += b.cc
		cr += b.cr
		out += b.out
		cost += b.cost
		if err := store.UpsertUsageDaily(k.date, k.session, k.model, issue, in, cc, cr, out, cost); err != nil {
			log.Printf("usage_history: upsert %s/%s failed: %v", k.date, k.session, err)
		}
	}
	return ctx.Err()
}

func runUsageScanLoop(ctx context.Context, projectsRoot string, store *Store, interval time.Duration) {
	if n, err := backfillIssue(projectsRoot, store); err != nil {
		log.Printf("usage_history: backfill: %v", err)
	} else {
		log.Printf("usage_history: backfilled issue on %d rows", n)
	}
	prog := newScanProgress()
	if err := scanUsageHistory(ctx, projectsRoot, store, prog); err != nil {
		log.Printf("usage_history: initial scan: %v", err)
	} else {
		log.Printf("usage_history: initial scan complete (%s)", projectsRoot)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := scanUsageHistory(ctx, projectsRoot, store, prog); err != nil {
				log.Printf("usage_history: scan: %v", err)
			}
		}
	}
}

// backfillIssue walks projectsRoot once and maps every jsonl filename
// (the claude session UUID) to its parent-dir issue number, then
// updates usage_daily rows where issue is still 0. Idempotent.
func backfillIssue(projectsRoot string, store *Store) (int, error) {
	sessionToIssue := map[string]int{}
	err := filepath.WalkDir(projectsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		base := filepath.Base(path)
		sid := strings.TrimSuffix(base, ".jsonl")
		parent := filepath.Base(filepath.Dir(path))
		m := cwdIssueRe.FindStringSubmatch(parent)
		if len(m) == 0 {
			return nil
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return nil
		}
		sessionToIssue[sid] = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return store.BackfillUsageIssue(sessionToIssue)
}

// projectsRootFor resolves the ~/.claude/projects directory for the
// claude-user on the given VM. Mirrors the lookup in tailStatusLine
// so both feeds read from the same home dir.
func projectsRootFor(vm VMBlock) string {
	return fmt.Sprintf("%s/.claude/projects", claudeHome(vm))
}
