package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// RateLimit mirrors Claude Code's statusline rate_limits payload — a
// used_percentage 0-100 plus a unix-second reset timestamp. The same
// shape covers both the 5-hour session bucket and the 7-day cap.
type RateLimit struct {
	UsedPct  float64 `json:"used_percentage"`
	ResetsAt int64   `json:"resets_at"`
}

// StatusLineEvent is the subset of fields we care about from each
// statusline.jsonl line. Claude Code emits one of these per render
// tick (~once a second while a session is interactive). The rest of
// the payload (effort, thinking, etc.) is ignored.
type StatusLineEvent struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Cost struct {
		TotalCostUSD     float64 `json:"total_cost_usd"`
		TotalDurationMs  int64   `json:"total_duration_ms"`
		TotalLinesAdded  int     `json:"total_lines_added"`
		TotalLinesRemoved int    `json:"total_lines_removed"`
	} `json:"cost"`
	ContextWindow struct {
		TotalInputTokens  int      `json:"total_input_tokens"`
		TotalOutputTokens int      `json:"total_output_tokens"`
		CtxSize           int      `json:"context_window_size"`
		UsedPct           *float64 `json:"used_percentage"`
	} `json:"context_window"`
	RateLimits struct {
		FiveHour RateLimit `json:"five_hour"`
		SevenDay RateLimit `json:"seven_day"`
	} `json:"rate_limits"`
}

// usageState holds the latest event for a session plus the local
// time we observed it (used to age out stale entries and to pick the
// most-recent quota reading when multiple sessions report).
type usageState struct {
	StatusLineEvent
	UpdatedAt time.Time
}

var (
	usageMu        sync.RWMutex
	usageBySession = map[string]*usageState{}
	usageByIssue   = map[int]*usageState{}
)

var cwdIssueRe = regexp.MustCompile(`orch-work/issue-(\d+)`)

// ingestStatusLine parses one jsonl line and updates the in-memory
// indexes. Silent on malformed input — the tail loop should never die
// because of a partial line.
func ingestStatusLine(line []byte) {
	var e StatusLineEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return
	}
	if e.SessionID == "" {
		return
	}
	st := &usageState{StatusLineEvent: e, UpdatedAt: time.Now()}
	usageMu.Lock()
	defer usageMu.Unlock()
	usageBySession[e.SessionID] = st
	if m := cwdIssueRe.FindStringSubmatch(e.Cwd); len(m) > 0 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			usageByIssue[n] = st
		}
	}
}

// usageForIssue returns the latest statusline reading for the claude
// session running the given inbox issue, or nil if we haven't seen
// one. cwd → issue is matched via the standard orch-work/issue-N
// worktree layout.
func usageForIssue(n int) *usageState {
	usageMu.RLock()
	defer usageMu.RUnlock()
	return usageByIssue[n]
}

// latestQuota returns the most recent five-hour + seven-day reading
// across every session we've seen. The values are global across the
// account, so any session's last sample is authoritative.
func latestQuota() (RateLimit, RateLimit, bool) {
	usageMu.RLock()
	defer usageMu.RUnlock()
	var latest *usageState
	for _, s := range usageBySession {
		if latest == nil || s.UpdatedAt.After(latest.UpdatedAt) {
			latest = s
		}
	}
	if latest == nil {
		return RateLimit{}, RateLimit{}, false
	}
	return latest.RateLimits.FiveHour, latest.RateLimits.SevenDay, true
}

// tailStatusLine runs `tail -F` against the claude statusline.jsonl
// on the given VM and feeds each line into ingestStatusLine. Local
// VMs read directly; remote VMs go through ssh. Restarts on EOF /
// error so a transient pipe break (claude restart, log rotate, ssh
// disconnect) doesn't permanently lose the data feed.
func tailStatusLine(ctx context.Context, vm VMBlock, bcast chan<- struct{}) {
	path := fmt.Sprintf("/home/%s/.claude/statusline.jsonl", vm.User)
	for ctx.Err() == nil {
		var cmd *exec.Cmd
		if isLocal(vm) {
			cmd = exec.CommandContext(ctx, "tail", "-F", "-n", "0", path)
		} else {
			cmd = exec.CommandContext(ctx, "ssh", append(sshArgs(vm), "tail -F -n 0 "+path)...)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		if err := cmd.Start(); err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			ingestStatusLine(sc.Bytes())
			// Nudge the relay state pusher so the dashboard sees the
			// new usage/quota numbers within ~2s instead of waiting
			// for the next tick.
			if bcast != nil {
				select {
				case bcast <- struct{}{}:
				default:
				}
			}
		}
		_ = cmd.Wait()
		time.Sleep(5 * time.Second)
	}
}
