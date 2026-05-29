package orch

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// claudeHome returns the home directory containing ~/.claude for the
// user that runs claude on the given VM. Per-VM session_home wins;
// otherwise local VMs use the orch daemon's own $HOME (correct on Mac
// + Linux dev installs) and remote VMs assume /home/<user> (workers
// are Linux).
func claudeHome(vm VMBlock) string {
	if vm.SessionHome != "" {
		return vm.SessionHome
	}
	if isLocal(vm) {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return h
		}
	}
	if vm.User != "" {
		return "/home/" + vm.User
	}
	return "/home/orchid"
}

// RateLimit mirrors Claude Code's statusline rate_limits payload — a
// used_percentage 0-100 plus a unix-second reset timestamp. The same
// shape covers both the 5-hour session bucket and the 7-day cap.
type RateLimit struct {
	UsedPct  float64 `json:"used_percentage"`
	ResetsAt int64   `json:"resets_at"`
}

type StatusLineEvent struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Model     struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Cost struct {
		TotalCostUSD      float64 `json:"total_cost_usd"`
		TotalDurationMs   int64   `json:"total_duration_ms"`
		TotalLinesAdded   int     `json:"total_lines_added"`
		TotalLinesRemoved int     `json:"total_lines_removed"`
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

// Matches both the statusline payload's cwd (with real slashes) and
// the cwd-encoded project directory name (slashes turned into dashes)
// — claude uses `-home-orchid-orch-work-issue-N` for project dirs.
var cwdIssueRe = regexp.MustCompile(`orch-work[/-]issue-(\d+)`)

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

func usageForIssue(n int) *usageState {
	usageMu.RLock()
	defer usageMu.RUnlock()
	return usageByIssue[n]
}

// contextTokensForIssue returns the latest known conversation-context size
// (total input tokens) for issue n's session, from its statusline. 0 when no
// statusline has been seen. The token-saving logic uses this to cycle a session
// whose context has grown large (every turn re-reads it as cache_read).
func contextTokensForIssue(n int) int {
	usageMu.RLock()
	defer usageMu.RUnlock()
	if s := usageByIssue[n]; s != nil {
		return s.ContextWindow.TotalInputTokens
	}
	return 0
}

func latestQuota() (RateLimit, RateLimit, bool) {
	usageMu.RLock()
	defer usageMu.RUnlock()
	var latest *usageState
	for _, s := range usageBySession {
		if s.RateLimits.FiveHour.ResetsAt == 0 && s.RateLimits.SevenDay.ResetsAt == 0 {
			continue
		}
		if latest == nil || s.UpdatedAt.After(latest.UpdatedAt) {
			latest = s
		}
	}
	if latest == nil {
		return RateLimit{}, RateLimit{}, false
	}
	return latest.RateLimits.FiveHour, latest.RateLimits.SevenDay, true
}

func tailStatusLine(ctx context.Context, vm VMBlock, bcast chan<- struct{}) {
	path := claudeHome(vm) + "/.claude/statusline.jsonl"
	log.Printf("usage: tailing %s on %s", path, vm.Name)
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
