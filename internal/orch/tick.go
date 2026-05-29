package orch

import (
	"embed"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

func startSession(cfg *Config, vm *VMBlock, is Issue, target TargetBlock, lifecycle, schedule string) error {
	session := sessionName(is.Number, vmAgent(*vm).name)
	branch := cfg.Orch.BranchPrefix + fmt.Sprint(is.Number)
	root := vmWorkdirRoot(cfg.Orch, *vm)
	workdir := fmt.Sprintf("%s/issue-%d", root, is.Number)
	sharedDir := fmt.Sprintf("%s/repos/%s", root, strings.ReplaceAll(target.Repo, "/", "-"))
	botLogin, botEmail := vmBotIdentity(cfg.Orch, *vm)
	if botLogin == "" {
		return fmt.Errorf("bot_login not set — connect GitHub from the dashboard before spawning sessions")
	}
	if err := tmuxStart(*vm, session, workdir, sharedDir, target.Repo, branch, "", botLogin, botEmail); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	_, _, _ = sshExec(*vm, fmt.Sprintf("tmux send-keys -t %s C-m", session))
	// 3 minutes covers slow claude TUI startup in heavy worktrees (e.g.
	// fresh deno checkout: lockfile parse + project scan can push first
	// idle past the 60s mark on a contended VM).
	const idleWaitTimeout = 3 * time.Minute
	deadline := time.Now().Add(idleWaitTimeout)
	sawIdle := false
	for time.Now().Before(deadline) {
		if idle, _, err := tmuxIdle(*vm, session); err == nil && idle {
			sawIdle = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !sawIdle {
		pane, _, _ := sshExec(*vm, fmt.Sprintf("tmux capture-pane -p -t %s | tail -15", session))
		tmuxKill(*vm, session)
		return fmt.Errorf("session never reached idle prompt within %s (claude not authenticated?); pane tail:\n%s", idleWaitTimeout, strings.TrimSpace(pane))
	}
	tmpl := cfg.BootstrapPrompt
	if vm.BootstrapPrompt != "" {
		tmpl = vm.BootstrapPrompt
	}
	if lifecycle == "cron" {
		tmpl = cfg.CronBootstrapPrompt
	}
	msg := renderBootstrap(tmpl, is, branch, target.Name, target.Repo, cfg.GitHub.InboxRepo, workdir, schedule)
	msg = expandIncludes(msg, cfg.GitHub.InboxRepo)
	if err := tmuxPaste(*vm, session, msg); err != nil {
		tmuxKill(*vm, session)
		return fmt.Errorf("bootstrap paste: %w", err)
	}
	return nil
}

// spawn registers a fresh oneshot Job and starts its session. Cron jobs use
// fireCron instead, since their Job is created on first sighting and the
// session is fired/refired on a schedule.
func spawn(cfg *Config, st *State, vm *VMBlock, is Issue, target TargetBlock) error {
	if err := startSession(cfg, vm, is, target, "oneshot", ""); err != nil {
		return err
	}
	branch := cfg.Orch.BranchPrefix + fmt.Sprint(is.Number)
	st.Jobs[is.Number] = &Job{
		VM: vm.Name, Tmux: sessionName(is.Number, vmAgent(*vm).name),
		Target: target.Name, TargetRepo: target.Repo,
		Branch: branch, Lifecycle: "oneshot",
		IssueTitle:           is.Title,
		LastCheckConclusions: map[string]string{},
	}
	log.Printf("issue #%d: spawned on %s/%s, target=%s (%s), branch=%s",
		is.Number, vm.Name, sessionName(is.Number, vmAgent(*vm).name), target.Name, target.Repo, branch)
	_, _, err := run("gh", "api", "-X", "POST",
		fmt.Sprintf("repos/%s/issues/%d/reactions", cfg.GitHub.InboxRepo, is.Number),
		"-f", "content=eyes")
	if err != nil {
		log.Printf("issue #%d: eyes reaction on inbox failed: %v", is.Number, err)
	}
	return nil
}

// spawnResume restarts a dead session that had an open PR, using --resume so
// claude recovers its conversation context, then pastes a short situation report.
func spawnResume(cfg *Config, st *State, vm *VMBlock, n int, j *Job) error {
	session := sessionName(n, vmAgent(*vm).name)
	if j.Tmux != session {
		log.Printf("issue #%d: tmux name updating %s → %s (agent change)", n, j.Tmux, session)
		j.Tmux = session
	}
	root := vmWorkdirRoot(cfg.Orch, *vm)
	workdir := fmt.Sprintf("%s/issue-%d", root, n)
	sharedDir := fmt.Sprintf("%s/repos/%s", root, strings.ReplaceAll(j.TargetRepo, "/", "-"))

	base := vm.SessionCmd
	if base == "" {
		base = "clawpatrol run -- claude --dangerously-skip-permissions"
	}
	resumeCmd := vmAgent(*vm).resumeXform(base)

	botLogin, botEmail := vmBotIdentity(cfg.Orch, *vm)
	if err := tmuxStart(*vm, session, workdir, sharedDir, j.TargetRepo, j.Branch, resumeCmd, botLogin, botEmail); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	_, _, _ = sshExec(*vm, fmt.Sprintf("tmux send-keys -t %s C-m", session))
	deadline := time.Now().Add(3 * time.Minute)
	resumeIdle := false
	for time.Now().Before(deadline) {
		if idle, _, err := tmuxIdle(*vm, session); err == nil && idle {
			resumeIdle = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	prURL := fmt.Sprintf("https://github.com/%s/pull/%d", j.TargetRepo, j.PR)
	ci := ""
	for name, status := range j.LastCheckConclusions {
		ci += fmt.Sprintf("  %s: %s\n", name, status)
	}
	if ci == "" {
		ci = "  (no CI results yet)\n"
	}
	msg := fmt.Sprintf(`Your session was interrupted and has been restarted.

PR: #%d (%s)
Branch: %s
Last known CI:
%s
Resume your work — check what is implemented, address any CI failures or review comments, push fixes if needed. If everything is already addressed and CI is green, stop and wait for the next review.`,
		j.PR, prURL, j.Branch, ci)

	if err := tmuxPaste(*vm, session, msg); err != nil {
		tmuxKill(*vm, session)
		return fmt.Errorf("resume paste: %w", err)
	}
	if !resumeIdle {
		log.Printf("issue #%d: resumed before idle prompt was observed; bootstrap poke still pasted best-effort", n)
	}
	j.Tmux = session
	log.Printf("issue #%d: resumed on %s/%s, PR #%d", n, vm.Name, session, j.PR)
	return nil
}

// fireCron starts an ephemeral session for an existing cron Job. Caller
// updates Job.NextFireAt on success. Job is assumed to already be in state.
func fireCron(cfg *Config, st *State, vm *VMBlock, is Issue, target TargetBlock, schedule string) error {
	if err := startSession(cfg, vm, is, target, "cron", schedule); err != nil {
		return err
	}
	session := sessionName(is.Number, vmAgent(*vm).name)
	if j := st.Jobs[is.Number]; j != nil {
		j.VM = vm.Name
		j.Tmux = session
	}
	log.Printf("issue #%d: cron tick fired on %s/%s (schedule=%s)",
		is.Number, vm.Name, session, schedule)
	return nil
}

func tickCron(cfg *Config, st *State, n int, j *Job, is Issue, target TargetBlock, thr ThrottleDecision) {
	cron := parseCronFrontmatter(is.Body)
	if cron == nil {
		log.Printf("issue #%d: cron schedule no longer parseable, skipping tick", n)
		return
	}
	if cron.ScheduleStr != j.Schedule || cron.TimeoutStr != j.Timeout {
		log.Printf("issue #%d: cron config changed (schedule %s → %s, timeout %s → %s)",
			n, j.Schedule, cron.ScheduleStr, j.Timeout, cron.TimeoutStr)
		j.Schedule = cron.ScheduleStr
		j.Timeout = cron.TimeoutStr
		saveStateLogged(st)
	}
	now := time.Now()
	if j.Tmux != "" {
		if vm := vmByName(cfg, j.VM); vm != nil {
			alive, err := tmuxHasSession(*vm, j.Tmux)
			if err != nil {
				log.Printf("issue #%d: tmux check failed: %v", n, err)
				return
			}
			if alive {
				if !j.FireStartedAt.IsZero() && now.Sub(j.FireStartedAt) > cron.Timeout {
					log.Printf("issue #%d: cron tick exceeded timeout %s, killing pane", n, cron.Timeout)
					tmuxKill(*vm, j.Tmux)
					j.Tmux = ""
					j.VM = ""
					j.FireStartedAt = time.Time{}
					saveStateLogged(st)
				}
				return
			}
			j.Tmux = ""
			j.VM = ""
			j.FireStartedAt = time.Time{}
			saveStateLogged(st)
		}
	}
	if now.Before(j.NextFireAt) {
		return
	}
	// Throttle gate sits AFTER the NextFireAt check and BEFORE the
	// NextFireAt advance below: a throttled fire is deferred, not recorded
	// as fired, so it fires on the first tick after release. In-flight cron
	// sessions (handled above) are untouched.
	if thr.BlocksNewWork() {
		log.Printf("issue #%d: cron tick due but weekly throttle active (%s), deferring fire", n, thr.Mode)
		return
	}
	vm := freeVM(cfg, st)
	if vm == nil {
		log.Printf("issue #%d: cron tick due but no free VM", n)
		return
	}
	if err := fireCron(cfg, st, vm, is, target, cron.ScheduleStr); err != nil {
		log.Printf("issue #%d: cron fire failed on %s: %v", n, vm.Name, err)
		return
	}
	fireDoneAt := time.Now()
	j.NextFireAt = fireDoneAt.Add(cron.Schedule)
	j.FireStartedAt = fireDoneAt
	saveStateLogged(st)
}

// Mention is one @-mention of a configured bot found in a comment on an
// issue or PR in a configured target repo.

func tick(cfg *Config, st *State) {
	st.mu.Lock()
	defer st.mu.Unlock()
	snap := make(map[int]Job, len(st.Jobs))
	for n, j := range st.Jobs {
		snap[n] = *j
	}
	st.httpSnap.Store(snap)

	// Weekly throttle: one decision per tick, shared by every gate below so
	// they never disagree within a pass. Pure function of live quota + wall
	// clock; fails open (ModeAllow) when throttle is disabled or quota is
	// unreadable, in which case BlocksNewWork()==false and every gate is a
	// no-op (identical to today). Log only on mode transitions to avoid
	// per-tick noise.
	thr := currentThrottle(cfg, time.Now())
	if thr.Mode != st.lastThrottleMode {
		if thr.Mode == ModeAllow {
			log.Printf("weekly throttle cleared (now %s)", thr.Mode)
		} else {
			log.Printf("weekly throttle active: %s (%s)", thr.Mode, thr.Reason)
		}
		st.lastThrottleMode = thr.Mode
	}

	// Proactive pacing governor: one decision per tick from live quota + the
	// persisted sample window, layered ON TOP of the throttle gate (it can only
	// further restrict, never admit work the gate blocked). Fails open
	// (EffectiveCap=MaxInt, PausedTarget=0) when disabled / quota unreadable /
	// thin data, in which case the admission loop and duty-cycle pass below are
	// byte-for-byte today's behavior. govCap is the slew anchor for the cap; it
	// is restored from the kv table on a fresh process so the cap doesn't snap
	// across a restart.
	now := time.Now()
	if st.govCap == 0 {
		if b, err := st.store.GetKV("gov_cap"); err == nil && len(b) > 0 {
			if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				st.govCap = v
			}
		}
	}
	five, seven, qok := latestQuota()
	var samples []QuotaSample
	if cfg.Orch.Throttle != nil && cfg.Orch.Throttle.GovernorEnabled {
		window := cfg.Orch.Throttle.withDefaults().rateWindowDur()
		samples, _ = st.store.LoadQuotaSamples(now.Add(-window).Unix())
	}
	active := countRunning(st)
	gov := GovernorDecide(now, five, seven, qok, samples, active, st.govCap, cfg.Orch.Throttle)
	st.SetGovernorState(gov)
	if gov.EffectiveCap != math.MaxInt {
		st.govCap = gov.EffectiveCap
		_ = st.store.PutKV("gov_cap", []byte(strconv.Itoa(gov.EffectiveCap)))
	}
	// Log only on cap/binding transitions to avoid per-tick noise.
	if gov.EffectiveCap != st.lastGovCap || gov.Binding != st.lastGovBinding {
		if gov.Enabled {
			log.Printf("governor: cap=%s active=%d paused-target=%d binding=%s burn(weekly=%.2f/h target=%.2f/h) projected-eow=%.1f%%",
				capLabel(gov.EffectiveCap), active, gov.PausedTarget, gov.Binding,
				gov.BurnWeekly, gov.TargetWeekly, gov.ProjectedEndPct)
		} else if st.lastGovCap != 0 && st.lastGovCap != math.MaxInt {
			log.Printf("governor: disabled/fail-open (uncapped)")
		}
		st.lastGovCap = gov.EffectiveCap
		st.lastGovBinding = gov.Binding
	}

	// Never-strand reconcile pass: runs BEFORE ghIssueList so a list error
	// can't skip it. Clears paused flags for dead panes and resumes everything
	// when duty-cycle is not actively managing. Resume gates on dutyOn (not just
	// gov.Enabled): if duty_cycle is flipped off while governor_enabled stays
	// true, persisted paused jobs must still be resumed or they'd stay SIGSTOP'd
	// forever.
	dutyOn := gov.Enabled && cfg.Orch.Throttle != nil && cfg.Orch.Throttle.DutyCycle
	reconcilePaused(cfg, st, dutyOn)

	type routed struct {
		is     Issue
		target TargetBlock
	}
	allOpen := map[int]Issue{}
	open := map[int]routed{}
	issues, err := ghIssueList(cfg.GitHub.InboxRepo, "")
	if err != nil {
		log.Printf("list inbox issues: %v; preserving tracked jobs until next successful poll", err)
		return
	}
	for _, is := range issues {
		allOpen[is.Number] = is
		for _, t := range cfg.Targets {
			if is.hasLabel(t.Label) {
				open[is.Number] = routed{is: is, target: t}
				break
			}
		}
	}

	// First pass: register cron jobs (governor cap does not gate cron — cron
	// fires are gated by thr.BlocksNewWork in tickCron). Collect the un-admitted
	// oneshot candidates for priority-ordered admission below.
	var candidates []int
	for n, r := range open {
		if _, exists := st.Jobs[n]; exists {
			continue
		}
		if r.is.hasLabel("cron") {
			cron := parseCronFrontmatter(r.is.Body)
			if cron == nil {
				log.Printf("issue #%d: cron label present but no valid `schedule` in toml frontmatter, skipping", n)
				continue
			}
			if cfg.CronBootstrapPrompt == "" {
				log.Printf("issue #%d: cron label present but cron_bootstrap_prompt unset in config, skipping", n)
				continue
			}
			prio := parsePriorityFrontmatter(r.is.Body)
			if prio == 0 {
				prio = defaultPriority(cfg)
			}
			st.Jobs[n] = &Job{
				Target: r.target.Name, TargetRepo: r.target.Repo,
				Branch:     cfg.Orch.BranchPrefix + fmt.Sprint(n),
				Lifecycle:  "cron",
				Schedule:   cron.ScheduleStr,
				Timeout:    cron.TimeoutStr,
				IssueTitle: r.is.Title,
				Priority:   prio,
			}
			log.Printf("issue #%d: registered cron job (target=%s, schedule=%s, timeout=%s, priority=%d)",
				n, r.target.Name, cron.ScheduleStr, cron.TimeoutStr, prio)
			saveStateLogged(st)
			continue
		}
		candidates = append(candidates, n)
	}

	// Admission. The throttle gate (BlocksNewWork) is the hard floor and still
	// applies first; the governor only further restricts via EffectiveCap.
	//
	// When the governor is disabled (EffectiveCap == MaxInt) we preserve the
	// original behavior: try to admit every candidate (map-order iteration,
	// freeVM is the only limit) — byte-for-byte today. When enabled we sort by
	// priority DESC (FIFO tiebreak via jobsByPriority's issue-number order) and
	// admit at most EffectiveCap - active this tick; deferred low-priority
	// issues are the implicit admission queue (re-evaluated next tick).
	admitSlots := math.MaxInt
	if gov.Enabled && gov.EffectiveCap != math.MaxInt {
		// Count TOTAL governable (running + paused), not just running: a paused
		// session still occupies a slot (it resumes and burns later), so the cap
		// must include it or pausing one would free a slot for fresh work.
		admitSlots = gov.EffectiveCap - countGovernable(st)
		if admitSlots < 0 {
			admitSlots = 0
		}
		// Sort candidates by priority DESC, parsing per-issue priority.
		prio := map[int]int{}
		for _, n := range candidates {
			p := parsePriorityFrontmatter(open[n].is.Body)
			if p == 0 {
				p = defaultPriority(cfg)
			}
			prio[n] = p
		}
		sort.SliceStable(candidates, func(a, b int) bool {
			if prio[candidates[a]] != prio[candidates[b]] {
				return prio[candidates[a]] > prio[candidates[b]]
			}
			return candidates[a] < candidates[b]
		})
	} else {
		// Disabled path: keep deterministic by issue number, but no cap.
		sort.Ints(candidates)
	}

	admitted := 0
	for _, n := range candidates {
		r := open[n]
		if thr.BlocksNewWork() {
			log.Printf("issue #%d: weekly throttle active (%s: %s), deferring spawn", n, thr.Mode, thr.Reason)
			continue
		}
		if admitted >= admitSlots {
			log.Printf("issue #%d: governor cap reached (cap=%s active=%d), deferring spawn",
				n, capLabel(gov.EffectiveCap), active)
			continue
		}
		vm := freeVM(cfg, st)
		if vm == nil {
			log.Printf("issue #%d: no free VM, skipping", n)
			continue
		}
		if err := spawn(cfg, st, vm, r.is, r.target); err != nil {
			log.Printf("issue #%d: spawn failed on %s: %v", n, vm.Name, err)
			continue
		}
		// Stamp priority on the freshly-created job.
		if j := st.Jobs[n]; j != nil {
			p := parsePriorityFrontmatter(r.is.Body)
			if p == 0 {
				p = defaultPriority(cfg)
			}
			j.Priority = p
		}
		admitted++
		saveStateLogged(st)
	}

	budget := killBudget{max: maxKillsPerTick}
	for n, j := range st.Jobs {
		// Adhoc jobs (from `orch run <title>`) aren't backed by a
		// GitHub issue — skip the open/allOpen lifecycle gates and the
		// PR machinery below. Tmux liveness alone decides their fate.
		if j.Lifecycle == "adhoc" {
			vm := vmByName(cfg, j.VM)
			if vm == nil {
				log.Printf("adhoc %s: vm %q gone, dropping", j.Tmux, j.VM)
				delete(st.Jobs, n)
				saveStateLogged(st)
				continue
			}
			if h := st.VMHealth(vm.Name); !h.LastOK.IsZero() && !h.Online {
				continue
			}
			alive, err := tmuxHasSession(*vm, j.Tmux)
			if err != nil {
				continue
			}
			if !alive {
				log.Printf("adhoc %s: tmux gone, dropping", j.Tmux)
				delete(st.Jobs, n)
				saveStateLogged(st)
			}
			continue
		}
		if r, routedOpen := open[n]; routedOpen {
			j.IssueTitle = r.is.Title
			// Re-sync priority live so editing the issue frontmatter retunes
			// the governor's ordering without a restart.
			if p := parsePriorityFrontmatter(r.is.Body); p != 0 || cfg.Orch.Throttle != nil {
				if p == 0 {
					p = defaultPriority(cfg)
				}
				if p != j.Priority {
					j.Priority = p
					saveStateLogged(st)
				}
			}
			wantCron := r.is.hasLabel("cron")
			isCron := j.Lifecycle == "cron"
			if wantCron != isCron {
				log.Printf("issue #%d: lifecycle drift (have=%q want=%s) — dropping for re-registration",
					n, j.Lifecycle, map[bool]string{true: "cron", false: "oneshot"}[wantCron])
				tearDown(cfg, st, n)
				saveStateLogged(st)
				continue
			}
		} else if _, stillOpen := allOpen[n]; !stillOpen {
			tearDown(cfg, st, n)
			saveStateLogged(st)
			continue
		}
		if j.Lifecycle == "cron" {
			r, ok := open[n]
			if !ok {
				log.Printf("issue #%d: cron job no longer in open list, dropping", n)
				tearDown(cfg, st, n)
				saveStateLogged(st)
				continue
			}
			tickCron(cfg, st, n, j, r.is, r.target, thr)
			continue
		}
		// Duty-cycle-paused sessions are frozen (SIGSTOP'd): skip the poke/PR
		// pipeline so we never paste into a frozen pane or mark reviews "seen"
		// while it can't respond. reconcilePaused (dead-pane cleanup + resume on
		// duty-off) and applyDutyCycle (resume / force-resume after maxPauseDur)
		// own the paused lifecycle.
		if j.Paused {
			continue
		}
		vm := vmByName(cfg, j.VM)
		if vm == nil {
			log.Printf("issue #%d: vm %q gone from config, dropping", n, j.VM)
			delete(st.Jobs, n)
			saveStateLogged(st)
			continue
		}
		// VM is currently offline per probe — keep the job in state
		// and render it greyed on the dashboard. Once it comes back
		// the tmux check below decides resume vs tear-down.
		if h := st.VMHealth(vm.Name); !h.LastOK.IsZero() && !h.Online {
			continue
		}
		alive, err := tmuxHasSession(*vm, j.Tmux)
		if err != nil {
			log.Printf("issue #%d: tmux check failed: %v", n, err)
			continue
		}
		if !alive {
			if !budget.tryUse() {
				log.Printf("issue #%d: tmux session %q gone, deferring respawn (kill budget %d/%d exhausted this tick)",
					n, j.Tmux, budget.used, budget.max)
				continue
			}
			if j.PR > 0 {
				log.Printf("issue #%d: tmux session %q gone, respawning with --resume (PR #%d)", n, j.Tmux, j.PR)
				if err := spawnResume(cfg, st, vm, n, j); err != nil {
					log.Printf("issue #%d: resume failed, tearing down: %v", n, err)
					tearDown(cfg, st, n)
				}
			} else {
				log.Printf("issue #%d: tmux session %q gone, tearing down", n, j.Tmux)
				tearDown(cfg, st, n)
			}
			saveStateLogged(st)
			continue
		}
		if j.PR == 0 {
			botLogin, _ := vmBotIdentity(cfg.Orch, *vm)
			pr, err := ghFindPRByBranch(j.TargetRepo, j.Branch, botLogin)
			if err != nil {
				log.Printf("issue #%d: find PR failed: %v", n, err)
				continue
			}
			if pr == nil {
				if r, ok := open[n]; ok {
					prNum, err := ghAutoCreatePR(cfg, n, j, r.is)
					if err != nil {
						if strings.Contains(err.Error(), "already exists") {
							log.Printf("issue #%d: branch %s already has a PR by another account, tearing down", n, j.Branch)
							tearDown(cfg, st, n)
							saveStateLogged(st)
							continue
						}
						log.Printf("issue #%d: auto-create PR: %v", n, err)
					} else if prNum > 0 {
						pr = &PRSummary{Number: prNum, State: "OPEN"}
					}
				}
				if pr == nil {
					continue
				}
			}
			j.PR = pr.Number
			log.Printf("issue #%d: found PR #%d in %s", n, j.PR, j.TargetRepo)
			prURL := fmt.Sprintf("https://github.com/%s/pull/%d", j.TargetRepo, j.PR)
			ntfyNotify(cfg.Orch.NtfyTopic,
				fmt.Sprintf("PR opened: issue #%d", n),
				fmt.Sprintf("%s\n%s", j.Branch, prURL),
				prURL)
			saveStateLogged(st)
		}
		v, err := ghPRView(j.TargetRepo, j.PR)
		viewedAt := time.Now()
		if err != nil {
			log.Printf("issue #%d: pr view failed: %v", n, err)
			continue
		}
		if v.State == "MERGED" || v.State == "CLOSED" {
			if v.State == "MERGED" && j.PR != 0 {
				prURL := fmt.Sprintf("https://github.com/%s/pull/%d", j.TargetRepo, j.PR)
				ntfyNotify(cfg.Orch.NtfyTopic,
					fmt.Sprintf("PR merged: issue #%d", n),
					fmt.Sprintf("%s/pull/%d merged ✓", j.TargetRepo, j.PR),
					prURL)
			}
			closedState := "merged"
			if v.State == "CLOSED" {
				closedState = "closed"
			}
			if err := st.store.PutClosedJob(n, closedState, j); err != nil {
				log.Printf("issue #%d: put closed_jobs: %v", n, err)
			}
			tearDown(cfg, st, n)
			saveStateLogged(st)
			continue
		}
		botLogin, _ := vmBotIdentity(cfg.Orch, *vm)
		vr, vt, vi, sr, st_, si, pushed, checks, mergeable := diffPR(j, v, botLogin)
		hasSilent := len(sr) > 0 || len(st_) > 0 || len(si) > 0
		if len(vr) == 0 && len(vt) == 0 && len(vi) == 0 && !pushed && len(checks) == 0 && mergeable == "" {
			j.LastHeadOID = v.HeadRefOid
			if v.Mergeable != "" && v.Mergeable != "UNKNOWN" {
				j.LastMergeable = v.Mergeable
			}
			if hasSilent {
				j.SeenReviewIDs = append(j.SeenReviewIDs, sr...)
				j.SeenThreadCommentIDs = append(j.SeenThreadCommentIDs, st_...)
				j.SeenIssueCommentIDs = append(j.SeenIssueCommentIDs, si...)
				saveStateLogged(st)
			}
			continue
		}
		idle, detected, err := tmuxIdle(*vm, j.Tmux)
		if err != nil {
			log.Printf("issue #%d: idle check failed: %v", n, err)
			continue
		}
		if detected != "" {
			if want := sessionName(n, detected); want != j.Tmux {
				if _, _, e := sshExec(*vm, fmt.Sprintf("tmux rename-session -t %s %s", j.Tmux, want)); e == nil {
					log.Printf("issue #%d: tmux renamed %s → %s (detected %s in pane)", n, j.Tmux, want, detected)
					j.Tmux = want
					saveStateLogged(st)
				}
			}
		}
		if !idle {
			log.Printf("issue #%d: pane busy, deferring poke", n)
			continue
		}
		const reCheckAfter = 5 * time.Second
		if time.Since(viewedAt) >= reCheckAfter {
			fresh, ferr := ghPRView(j.TargetRepo, j.PR)
			if ferr != nil {
				log.Printf("issue #%d: pre-poke pr re-check failed: %v", n, ferr)
			} else if fresh.State == "MERGED" || fresh.State == "CLOSED" {
				log.Printf("issue #%d: PR %s between view and poke — skipping poke and tearing down", n, fresh.State)
				closedState := "merged"
				if fresh.State == "CLOSED" {
					closedState = "closed"
				}
				if err := st.store.PutClosedJob(n, closedState, j); err != nil {
					log.Printf("issue #%d: put closed_jobs: %v", n, err)
				}
				tearDown(cfg, st, n)
				saveStateLogged(st)
				continue
			}
		}
		// PR-poke gate (opt-in, off by default): only the two hard Pause
		// modes can gate pokes, and only when throttle_pokes=true. We skip
		// without marking reviews/comments Seen, so the poke re-fires after
		// release. Default config => never gates => zero behavior change.
		if thr.BlocksPokes(cfg.Orch.Throttle) {
			log.Printf("issue #%d: weekly throttle active (%s), deferring poke", n, thr.Mode)
			continue
		}
		msg := summarize(v, vr, vt, vi, pushed, checks, mergeable)
		if err := tmuxPaste(*vm, j.Tmux, msg); err != nil {
			log.Printf("issue #%d: poke failed: %v", n, err)
			continue
		}
		j.SeenReviewIDs = append(j.SeenReviewIDs, vr...)
		j.SeenReviewIDs = append(j.SeenReviewIDs, sr...)
		j.SeenThreadCommentIDs = append(j.SeenThreadCommentIDs, vt...)
		j.SeenThreadCommentIDs = append(j.SeenThreadCommentIDs, st_...)
		j.SeenIssueCommentIDs = append(j.SeenIssueCommentIDs, vi...)
		j.SeenIssueCommentIDs = append(j.SeenIssueCommentIDs, si...)
		j.LastHeadOID = v.HeadRefOid
		if v.Mergeable != "" && v.Mergeable != "UNKNOWN" {
			j.LastMergeable = v.Mergeable
		}
		if j.LastCheckConclusions == nil {
			j.LastCheckConclusions = map[string]string{}
		}
		latestAt := map[string]string{}
		for _, c := range v.StatusCheckRollup {
			if c.Status == "COMPLETED" && c.CompletedAt > latestAt[c.Name] {
				latestAt[c.Name] = c.CompletedAt
				j.LastCheckConclusions[c.Name] = c.Conclusion
			}
		}
		saveStateLogged(st)
		log.Printf("issue #%d: poked PR #%d", n, j.PR)
	}

	// Duty-cycle pass: when the governor is enabled with duty_cycle on, SIGSTOP
	// the lowest-priority running sessions / SIGCONT the highest-priority paused
	// ones so the paused set matches gov.PausedTarget. Resume-before-pause,
	// bounded ops/tick. Disabled / fail-open => PausedTarget==0 and the
	// reconcile pass already resumed everything, so this is a no-op.
	if gov.Enabled && cfg.Orch.Throttle != nil && cfg.Orch.Throttle.DutyCycle {
		applyDutyCycle(cfg, st, gov, cfg.Orch.Throttle.withDefaults().maxPauseDur())
	}
}

//go:embed all:embed-dist
var wwwFS embed.FS
