package orch

import (
	"context"
	"log"
	"math"
	"sort"
	"strconv"
	"time"
)

// Daemon-side glue for the proactive pacing governor. governor.go owns the
// pure control law (no globals/locks/I/O); this file owns the parts that need
// the live job set, the persisted samples, the SIGSTOP/SIGCONT over ssh, and
// the per-job priority ordering. Everything here runs under st.mu (tick holds
// it) unless noted, and is written to FAIL OPEN: a governor bug must never
// deadlock work or strand a session SIGSTOP'd forever.

// maxDutyOpsPerTick bounds how many SIGSTOP/SIGCONT ssh round-trips the
// duty-cycle pass issues per tick, so st.mu isn't held through too many calls
// (mirrors maxKillsPerTick). The paused set converges over a few ticks.
const maxDutyOpsPerTick = 4

// runQuotaSampleLoop persists a reading of both rate-limit buckets every
// SampleInterval into quota_samples, giving the governor's burn-rate estimator
// a time-series to work from. It samples unconditionally (cheap; the estimator
// only consumes the data when the governor is enabled) and prunes to ~14 days
// every 50th insert. Fails silently when there is no statusline yet (ok=false)
// — no sample, no harm.
func runQuotaSampleLoop(ctx context.Context, store *Store, cfg *Config) {
	iv := defaultGovSampleInterval
	if cfg.Orch.Throttle != nil {
		iv = cfg.Orch.Throttle.withDefaults().sampleIntervalDur()
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	var n int
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Sample each configured agent's account independently — claude and
			// codex pace against their own buckets.
			for _, agent := range configuredAccounts(cfg) {
				five, seven, ok := latestQuota(agent)
				if !ok {
					continue
				}
				if err := store.InsertQuotaSample(QuotaSample{
					Agent:      agent,
					Ts:         time.Now().Unix(),
					FivePct:    five.UsedPct,
					FiveReset:  five.ResetsAt,
					SevenPct:   seven.UsedPct,
					SevenReset: seven.ResetsAt,
				}); err != nil {
					log.Printf("governor: insert quota sample (%s): %v", agent, err)
				}
			}
			if n++; n%50 == 0 {
				if err := store.PruneQuotaSamples(time.Now().Add(-14 * 24 * time.Hour).Unix()); err != nil {
					log.Printf("governor: prune quota samples: %v", err)
				}
			}
		}
	}
}

// defaultPriority returns the configured DefaultPriority, or 0 when the
// throttle block is absent. Used to substitute for issues whose frontmatter
// has no priority line.
func defaultPriority(cfg *Config) int {
	if cfg != nil && cfg.Orch.Throttle != nil {
		return cfg.Orch.Throttle.DefaultPriority
	}
	return 0
}

// configuredAccounts returns the distinct billing/metering accounts across all
// configured VMs (claude/codex/codex-mini/…), defaulting an unset account to
// the VM's agent name. Sorted for stable iteration. This is the set the
// governor + quota sampler pace independently.
func configuredAccounts(cfg *Config) []string {
	seen := map[string]bool{}
	var out []string
	for i := range cfg.VMs {
		a := vmAccount(cfg.VMs[i])
		if a == "" {
			a = "claude"
		}
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// jobAccount resolves the metering account a job counts against, via its VM's
// account (which defaults to the agent name). Falls back to "claude" when the
// VM is gone or unset.
func jobAccount(cfg *Config, j *Job) string {
	if j == nil {
		return "claude"
	}
	if vm := vmByName(cfg, j.VM); vm != nil {
		if a := vmAccount(*vm); a != "" {
			return a
		}
	}
	return "claude"
}

// countRunning counts the jobs the governor's cap/duty math treats as "active
// running sessions" for one agent: oneshot/PR jobs on that agent that are alive
// (have a tmux session) and NOT paused. Cron in-flight fires and adhoc jobs are
// excluded — they are short, timeout-bounded, and never duty-cycle victims
// (§2). Call under st.mu.
func countRunning(cfg *Config, st *State, agent string) int {
	n := 0
	for _, j := range st.Jobs {
		if !governable(j) || jobAccount(cfg, j) != agent {
			continue
		}
		if j.Paused {
			continue
		}
		n++
	}
	return n
}

// countGovernable counts governable jobs (running OR paused) for one agent. The
// admission cap bounds TOTAL admitted work — a paused session resumes and burns
// again — so admission must count paused jobs too, else duty-cycle-pausing one
// would free an admission slot and pull in fresh work, defeating the pause.
// Call under st.mu.
func countGovernable(cfg *Config, st *State, agent string) int {
	n := 0
	for _, j := range st.Jobs {
		if governable(j) && jobAccount(cfg, j) == agent {
			n++
		}
	}
	return n
}

// governable reports whether a job participates in the governor's
// cap/duty-cycle accounting. Excludes cron and adhoc lifecycles and jobs with
// no live tmux session.
func governable(j *Job) bool {
	if j == nil || j.Tmux == "" {
		return false
	}
	switch j.Lifecycle {
	case "cron", "adhoc":
		return false
	}
	return true
}

// jobsByPriority returns the issue numbers of jobs matching filter, ordered by
// priority. ascending=false => Priority DESC (highest first); ascending=true =>
// Priority ASC (lowest first). Ties break deterministically by issue number
// ASC so the ordering never thrashes. Call under st.mu.
//
// The richer LIFO/oldest-paused tiebreaks in §2 are applied by the callers
// (pause victims protect recently-started work, resume favours oldest-paused)
// using the returned slice plus the Job fields; the issue-number fallback here
// keeps a stable base order.
func jobsByPriority(st *State, filter func(n int, j *Job) bool, ascending bool) []int {
	var ns []int
	for n, j := range st.Jobs {
		if filter(n, j) {
			ns = append(ns, n)
		}
	}
	sort.SliceStable(ns, func(a, b int) bool {
		ja, jb := st.Jobs[ns[a]], st.Jobs[ns[b]]
		if ja.Priority != jb.Priority {
			if ascending {
				return ja.Priority < jb.Priority
			}
			return ja.Priority > jb.Priority
		}
		return ns[a] < ns[b] // issue number ASC (FIFO)
	})
	return ns
}

// reconcilePaused is the never-strand safety pass. It runs every tick BEFORE the
// issue-list fetch so a list error can't skip it. A Paused job is one the
// governor intentionally KILLED for duty-cycle (process gone, RAM freed, worktree
// kept) — so "paused" means the tmux session is deliberately absent and will be
// brought back via the scheduler's normal dead-session-with-PR → spawnResume
// path once unpaused. This pass guarantees a paused job is never stranded down:
// if duty-cycle is no longer actively managing (governor off/fail-open, or
// duty_cycle turned off while governor_enabled stays true) or the VM is gone,
// clear the flag so the main loop respawns it (--resume) on its next pass.
// Call under st.mu.
func reconcilePaused(cfg *Config, st *State, dutyActiveByAgent map[string]bool) {
	changed := false
	for n, j := range st.Jobs {
		if !j.Paused {
			continue
		}
		if !dutyActiveByAgent[jobAccount(cfg, j)] || vmByName(cfg, j.VM) == nil {
			j.Paused = false
			j.PausedAt = time.Time{}
			changed = true
			log.Printf("issue #%d: duty-cycle inactive, unpausing %q (main loop will respawn --resume)", n, j.Tmux)
		}
	}
	if changed {
		saveStateLogged(st)
	}
}

// applyDutyCycle drives the paused set toward gov.PausedTarget using kill +
// --resume (no process freeze): pause = kill the session (frees RAM, stops burn,
// worktree kept); resume = clear the flag and let the main loop's
// dead-session-with-PR → spawnResume path bring it back. Resume-before-pause to
// avoid churn. Bounded by maxDutyOpsPerTick per tick. Call under st.mu.
//
// Because --resume is not free (claude restart + context reload + a
// situation-report turn), this should pause in LONG windows: a job is only
// resumed once the binding bucket is no longer over pace (its windowed burn has
// decayed back under target) or it has been paused longer than maxPause
// (never-strand). Gating resume on !OverPace, not the paused count, is what lets
// a single hot session pace below 50% duty.
func applyDutyCycle(cfg *Config, st *State, agent string, gov GovernorDecision, maxPause time.Duration) {
	ops := 0
	changed := false
	now := time.Now()

	// --- Resume pass (highest priority first, oldest-paused tiebreak) ---
	paused := jobsByPriority(st, func(n int, j *Job) bool {
		return j.Paused && governable(j) && jobAccount(cfg, j) == agent
	}, false) // DESC
	sort.SliceStable(paused, func(a, b int) bool {
		ja, jb := st.Jobs[paused[a]], st.Jobs[paused[b]]
		if ja.Priority != jb.Priority {
			return ja.Priority > jb.Priority
		}
		return ja.PausedAt.Before(jb.PausedAt)
	})
	pausedCount := len(paused)
	for _, n := range paused {
		if ops >= maxDutyOpsPerTick {
			break
		}
		j := st.Jobs[n]
		forceResume := !j.PausedAt.IsZero() && now.Sub(j.PausedAt) > maxPause
		if forceResume || !gov.OverPace {
			// Just clear the flag: the main loop sees tmux gone + PR>0 and
			// respawns with --resume on its next pass. No SSH op here.
			j.Paused = false
			j.PausedAt = time.Time{}
			pausedCount--
			ops++
			changed = true
			log.Printf("issue #%d: duty-cycle unpausing, will respawn --resume (paused %d/%d, binding=%s)",
				n, pausedCount, gov.PausedTarget, gov.Binding)
		}
	}

	// --- Pause pass (lowest priority first, most-recently-started tiebreak) ---
	// Only PR-backed sessions: --resume recovery (spawnResume) requires a PR, and
	// killing a no-PR session would lose its early work.
	if pausedCount < gov.PausedTarget {
		victims := jobsByPriority(st, func(n int, j *Job) bool {
			return governable(j) && !j.Paused && j.PR > 0 && jobAccount(cfg, j) == agent
		}, true) // ASC
		sort.SliceStable(victims, func(a, b int) bool {
			ja, jb := st.Jobs[victims[a]], st.Jobs[victims[b]]
			if ja.Priority != jb.Priority {
				return ja.Priority < jb.Priority
			}
			return ja.FireStartedAt.After(jb.FireStartedAt)
		})
		for _, n := range victims {
			if ops >= maxDutyOpsPerTick || pausedCount >= gov.PausedTarget {
				break
			}
			j := st.Jobs[n]
			vm := vmByName(cfg, j.VM)
			if vm == nil {
				continue
			}
			// Kill the session (process gone, RAM freed) but KEEP the worktree so
			// --resume can recover the conversation. The main loop's respawn
			// branch (tmux gone + PR>0 → spawnResume) brings it back when unpaused.
			tmuxKill(*vm, j.Tmux)
			j.Paused = true
			j.PausedAt = now
			pausedCount++
			ops++
			changed = true
			log.Printf("issue #%d: duty-cycle paused — killed %q (PR #%d), will --resume later (priority=%d, paused %d/%d, binding=%s)",
				n, j.Tmux, j.PR, j.Priority, pausedCount, gov.PausedTarget, gov.Binding)
		}
	}

	if changed {
		saveStateLogged(st)
	}
}

// capLabel renders the EffectiveCap for logs, mapping math.MaxInt to "uncapped".
func capLabel(c int) string {
	if c == math.MaxInt {
		return "uncapped"
	}
	return strconv.Itoa(c)
}
