package orch

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// glance.go powers the list view's at-a-glance work-in-progress: per-session
// git diff stats (the branch's total change vs its base), captured by a slow
// loop and surfaced per job on /api/state. No persistence — re-derived live.

// WipStat is a session's work product, from `git` in its worktree: the branch
// diff vs its base (orch branches from origin/main) so it stays meaningful
// after the agent commits + pushes. Ahead = commits not yet pushed upstream.
type WipStat struct {
	Files   int  `json:"files"`
	Added   int  `json:"added"`
	Removed int  `json:"removed"`
	Ahead   int  `json:"ahead"`
	OK      bool `json:"ok"` // a git read succeeded (distinguishes "0 changes" from "unknown")
}

var (
	glanceMu sync.RWMutex
	paneWip  = map[string]WipStat{}
)

func wipSnapshot(tmux string) (WipStat, bool) {
	glanceMu.RLock()
	defer glanceMu.RUnlock()
	w, ok := paneWip[tmux]
	return w, ok
}

func glancePrune(live map[string]bool) {
	glanceMu.Lock()
	defer glanceMu.Unlock()
	for k := range paneWip {
		if !live[k] {
			delete(paneWip, k)
		}
	}
}

var shortstatRe = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertion)?(?:[^,]*, (\d+) deletion)?`)

func parseShortstat(s string) (files, added, removed int) {
	m := shortstatRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0
	}
	files, _ = strconv.Atoi(m[1])
	added, _ = strconv.Atoi(m[2])
	removed, _ = strconv.Atoi(m[3])
	return
}

// runWipLoop refreshes each live session's git WIP stats every ~30s. One ssh
// per session (cheap git reads, --no-optional-locks so it never blocks the
// agent's own git). Skips paused jobs (worktree intact but idle).
func runWipLoop(ctx context.Context, cfg *Config, st *State) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			var snap map[int]Job
			if v := st.httpSnap.Load(); v != nil {
				snap = v.(map[int]Job)
			}
			for issue, j := range snap {
				if j.Tmux == "" || j.Paused {
					continue
				}
				vm := vmByName(cfg, j.VM)
				if vm == nil {
					continue
				}
				root := vmWorkdirRoot(cfg.Orch, *vm)
				wd := fmt.Sprintf("%s/issue-%d", root, issue)
				// Total work product = the branch diff vs its base (orch branches
				// from origin/main), so it stays meaningful AFTER commit + push —
				// not just uncommitted edits. Plus commits-ahead of upstream.
				// safe.directory='*' so a root-run git still reads the
				// session-user-owned worktree.
				cmd := fmt.Sprintf(
					`cd %s 2>/dev/null && { git -c safe.directory='*' --no-optional-locks diff --shortstat origin/main...HEAD 2>/dev/null; printf '\x1e'; git -c safe.directory='*' --no-optional-locks rev-list --count @{u}..HEAD 2>/dev/null; } || true`,
					wd)
				out, _, err := sshExec(*vm, cmd)
				if err != nil {
					continue
				}
				parts := strings.SplitN(out, "\x1e", 2)
				w := WipStat{OK: true}
				w.Files, w.Added, w.Removed = parseShortstat(parts[0])
				if len(parts) > 1 {
					w.Ahead, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
				}
				glanceMu.Lock()
				paneWip[j.Tmux] = w
				glanceMu.Unlock()
				if st.Bcast != nil {
					select {
					case st.Bcast <- struct{}{}:
					default:
					}
				}
			}
		}
	}
}
