package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// modelPricing is per-million-token USD for {input, cacheCreation,
// cacheRead, output}. Values match Anthropic's published rates at
// time of writing; subscription users see this only as a relative
// burn proxy, not an actual invoice. Update when prices move — the
// numbers feed the dashboard's Usage tab and the future budget
// controller's weight formula.
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
	return modelPricing[1].rate // unknown → sonnet rate (middle)
}

// jsonlMessage is the subset of the transcript line we care about: an
// assistant message with usage + timestamp. We ignore every other
// line type (file-history-snapshot, permission-mode, …) so a schema
// change upstream that adds new line kinds is forward-compatible.
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

// scanProgress tracks how far we've consumed each jsonl file so a
// re-scan only reads the tail. Persisting this across orch restarts
// would be nice but isn't critical — re-reading is idempotent and
// the upsert collapses duplicates.
type scanProgress struct {
	mu   sync.Mutex
	pos  map[string]int64 // file path → bytes consumed
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

// scanUsageHistory walks the claude projects directory and aggregates
// every assistant turn it finds into per-day buckets, keyed by
// (date, sessionId, model). On startup this populates the full
// history from disk; on each tick after that, only files modified
// since the last scan get re-read. Idempotent — re-scanning the
// same range produces the same numbers.
func scanUsageHistory(ctx context.Context, projectsRoot string, store *Store, prog *scanProgress) error {
	type bucketKey struct{ date, session, model string }
	type bucket struct {
		in, cc, cr, out int64
		cost            float64
	}
	buckets := map[bucketKey]*bucket{}

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
			return nil // unchanged since last scan
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
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20) // some lines are large
		var consumed int64 = start
		for sc.Scan() {
			line := sc.Bytes()
			consumed += int64(len(line)) + 1 // +1 for the trailing \n
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
		}
		prog.set(path, consumed)
		return nil
	})
	if err != nil {
		return err
	}
	// One upsert per bucket. ON CONFLICT REPLACE means we have to merge
	// with the existing row first — partial-day scans where new turns
	// landed after a previous bucket would otherwise overwrite.
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
		if ex != nil {
			in, cc, cr, out, cost = ex.InputTokens, ex.CacheCreation, ex.CacheRead, ex.OutputTokens, ex.CostUSD
		}
		// Bucket holds tokens accumulated since last scan position;
		// add them to whatever was already in the row.
		in += b.in
		cc += b.cc
		cr += b.cr
		out += b.out
		cost += b.cost
		if err := store.UpsertUsageDaily(k.date, k.session, k.model, in, cc, cr, out, cost); err != nil {
			log.Printf("usage_history: upsert %s/%s failed: %v", k.date, k.session, err)
		}
	}
	return ctx.Err()
}

// runUsageScanLoop bootstraps a full scan at startup and then re-runs
// every interval thereafter so newly-flushed turns flow into the
// dashboard within a poll window. Errors are logged but don't kill
// the loop — a partial scan is better than no scan.
func runUsageScanLoop(ctx context.Context, projectsRoot string, store *Store, interval time.Duration) {
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

// projectsRootFor resolves the ~/.claude/projects directory for the
// claude-user on the given VM. Mirrors the lookup in tailStatusLine
// so both feeds read from the same home dir.
func projectsRootFor(vm VMBlock) string {
	home := vm.SessionHome
	if home == "" && vm.User != "" {
		home = "/home/" + vm.User
	}
	if home == "" {
		home = "/home/orchid"
	}
	return fmt.Sprintf("%s/.claude/projects", home)
}
