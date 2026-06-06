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
	"strings"
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

// agentQuotaState is the latest rate-limit reading for ONE agent's account
// (claude or codex). Rate limits are account-global, so the most-recent
// reading from any of that agent's sessions is authoritative. The governor
// and throttle pace each agent independently against its own buckets.
type agentQuotaState struct {
	Five      RateLimit
	Seven     RateLimit
	PlanType  string   // e.g. "prolite", "pro"; codex plan_type / claude (unset)
	Credits   *float64 // codex $-credit balance when on a credit plan; nil otherwise
	UpdatedAt time.Time
}

var (
	agentQuotaMu sync.RWMutex
	agentQuota   = map[string]*agentQuotaState{}
)

// setAgentQuota records the latest rate-limit reading for an agent. Ignores
// empty readings (both resets zero) so a stale all-zero event can't blank a
// good one.
func setAgentQuota(agent string, five, seven RateLimit, plan string, credits *float64) {
	if agent == "" {
		agent = "claude"
	}
	if five.ResetsAt == 0 && seven.ResetsAt == 0 {
		return
	}
	agentQuotaMu.Lock()
	defer agentQuotaMu.Unlock()
	agentQuota[agent] = &agentQuotaState{
		Five: five, Seven: seven, PlanType: plan, Credits: credits, UpdatedAt: time.Now(),
	}
}

// seedAgentQuotaFromDB primes the in-memory agentQuota map with each account's
// most-recent persisted reading on startup, so the dashboard shows the
// last-known quota across restarts instead of going blank until a fresh live
// sample arrives.
func seedAgentQuotaFromDB(cfg *Config, store *Store) {
	for _, acct := range configuredAccounts(cfg) {
		samples, err := store.LoadQuotaSamples(acct, 0)
		if err != nil || len(samples) == 0 {
			continue
		}
		q := samples[len(samples)-1] // newest (rows come back ts ASC)
		setAgentQuota(acct,
			RateLimit{UsedPct: q.FivePct, ResetsAt: q.FiveReset},
			RateLimit{UsedPct: q.SevenPct, ResetsAt: q.SevenReset}, "", nil)
		log.Printf("usage: seeded %s quota from db (5h %.0f%% / 7d %.0f%%, %s old)",
			acct, q.FivePct, q.SevenPct, time.Since(time.Unix(q.Ts, 0)).Round(time.Minute))
	}
}

// latestAgentQuota returns the full latest reading for an agent (for display).
func latestAgentQuota(agent string) (agentQuotaState, bool) {
	agentQuotaMu.RLock()
	defer agentQuotaMu.RUnlock()
	if s := agentQuota[agent]; s != nil {
		return *s, true
	}
	return agentQuotaState{}, false
}

// Matches both the statusline payload's cwd (with real slashes) and
// the cwd-encoded project directory name (slashes turned into dashes)
// — claude uses `-home-orchid-orch-work-issue-N` for project dirs.
var cwdIssueRe = regexp.MustCompile(`orch-work[/-]issue-(\d+)`)

// ingestStatusLine parses one jsonl line and updates the in-memory
// indexes. Silent on malformed input — the tail loop should never die
// because of a partial line.
func ingestStatusLine(line []byte, account string) {
	if account == "" {
		account = "claude"
	}
	var e StatusLineEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return
	}
	if e.SessionID == "" {
		return
	}
	st := &usageState{StatusLineEvent: e, UpdatedAt: time.Now()}
	usageMu.Lock()
	usageBySession[e.SessionID] = st
	if m := cwdIssueRe.FindStringSubmatch(e.Cwd); len(m) > 0 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			usageByIssue[n] = st
		}
	}
	usageMu.Unlock()
	// Feed this account's per-account quota bucket from the statusline reading.
	setAgentQuota(account, e.RateLimits.FiveHour, e.RateLimits.SevenDay, "", nil)
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

// latestQuota returns the most-recent 5h + weekly rate-limit reading for one
// agent's account (claude or codex). ok=false when that agent has reported no
// usable reading yet — the governor/throttle then fail open for that agent.
func latestQuota(agent string) (RateLimit, RateLimit, bool) {
	s, ok := latestAgentQuota(agent)
	if !ok {
		return RateLimit{}, RateLimit{}, false
	}
	return s.Five, s.Seven, true
}

// codexRollout is the subset of a codex session rollout JSONL line we care
// about: the token_count event carries the account rate_limits, shaped like
// Claude's (a primary 5h window + a secondary weekly window, each a
// used_percent 0-100 + a unix-second resets_at). primary/secondary are mapped
// to five/seven by window_minutes so we never assume ordering.
type codexRollout struct {
	Payload struct {
		Type       string `json:"type"`
		RateLimits *struct {
			Primary   *codexWindow `json:"primary"`
			Secondary *codexWindow `json:"secondary"`
			PlanType  string       `json:"plan_type"`
			Credits   *float64     `json:"credits"`
		} `json:"rate_limits"`
	} `json:"payload"`
}

type codexWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// ingestCodexRollout parses one rollout JSONL line; on a token_count event with
// rate_limits it updates codex's per-agent quota bucket. Silent on anything
// else (the rollout stream is mostly non-quota events).
func ingestCodexRollout(line []byte, account string) {
	if account == "" {
		account = "codex"
	}
	var e codexRollout
	if err := json.Unmarshal(line, &e); err != nil {
		return
	}
	rl := e.Payload.RateLimits
	if e.Payload.Type != "token_count" || rl == nil {
		return
	}
	var five, seven RateLimit
	for _, w := range []*codexWindow{rl.Primary, rl.Secondary} {
		if w == nil || w.ResetsAt == 0 {
			continue
		}
		r := RateLimit{UsedPct: w.UsedPercent, ResetsAt: w.ResetsAt}
		if w.WindowMinutes <= 600 { // <=10h => the short (5h) bucket
			five = r
		} else { // weekly bucket
			seven = r
		}
	}
	setAgentQuota(account, five, seven, rl.PlanType, rl.Credits)
}

// notifyEvent is one line of ~/.claude/notify.jsonl, written by claude's
// Notification + UserPromptSubmit hooks. A Notification means claude is waiting
// on a human (asked a question / idle / permission); UserPromptSubmit means
// input was just given (clears the wait). Reliable — claude emits it itself,
// vs scraping the pane.
type notifyEvent struct {
	HookEventName string `json:"hook_event_name"`
	Cwd           string `json:"cwd"`
	SessionID     string `json:"session_id"`
	Message       string `json:"message"`
}

var (
	needsInputMu      sync.RWMutex
	needsInputByIssue = map[int]bool{}
)

func needsInputForIssue(n int) bool {
	needsInputMu.RLock()
	defer needsInputMu.RUnlock()
	return needsInputByIssue[n]
}

// ingestNotify processes one notify.jsonl line: a Notification sets the issue's
// needs-input flag (surfaced as a dashboard badge); UserPromptSubmit clears it.
// issue is resolved from the event cwd.
func ingestNotify(line []byte) {
	var e notifyEvent
	if err := json.Unmarshal(line, &e); err != nil {
		return
	}
	m := cwdIssueRe.FindStringSubmatch(e.Cwd)
	if len(m) == 0 {
		return
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return
	}
	switch e.HookEventName {
	case "Notification":
		needsInputMu.Lock()
		rising := !needsInputByIssue[n]
		needsInputByIssue[n] = true
		needsInputMu.Unlock()
		if rising {
			log.Printf("issue #%d: needs input (notify: %s)", n, strings.TrimSpace(e.Message))
		}
	case "UserPromptSubmit", "Stop":
		needsInputMu.Lock()
		needsInputByIssue[n] = false
		needsInputMu.Unlock()
	}
}

// tailNotify follows ~/.claude/notify.jsonl on a claude VM (the Notification /
// UserPromptSubmit hook feed), mirroring tailStatusLine. Started per claude VM.
func tailNotify(ctx context.Context, vm VMBlock, bcast chan<- struct{}) {
	path := claudeHome(vm) + "/.claude/notify.jsonl"
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
			ingestNotify(sc.Bytes())
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

func tailStatusLine(ctx context.Context, vm VMBlock, bcast chan<- struct{}) {
	account := vmAccount(vm)
	path := claudeHome(vm) + "/.claude/statusline.jsonl"
	log.Printf("usage: tailing %s on %s (account=%s)", path, vm.Name, account)
	for ctx.Err() == nil {
		// -n 50 (not 0): on (re)start, read the recent backlog so the latest
		// rate_limits reading is ingested immediately — otherwise claude's quota
		// stays blank (absent from the dashboard) until an idle/paused session
		// happens to emit a fresh statusline line, which can be a long wait after
		// a restart. Re-ingesting a few recent lines is idempotent (latest wins).
		var cmd *exec.Cmd
		if isLocal(vm) {
			cmd = exec.CommandContext(ctx, "tail", "-F", "-n", "50", path)
		} else {
			cmd = exec.CommandContext(ctx, "ssh", append(sshArgs(vm), "tail -F -n 50 "+path)...)
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
			ingestStatusLine(sc.Bytes(), account)
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

// parseUnifiedRateHeaders extracts the live 5h + weekly utilization from a raw
// HTTP response-header dump (curl -D -) carrying Anthropic's unified
// rate-limit headers (Anthropic-Ratelimit-Unified-{5h,7d}-{Utilization,Reset}).
// Utilization is a 0..1 fraction; we scale to a 0..100 percent to match the
// statusline RateLimit shape. ok is false if neither window parsed.
func parseUnifiedRateHeaders(raw string) (five, seven RateLimit, ok bool) {
	get := func(name string) string {
		for _, ln := range strings.Split(raw, "\n") {
			if i := strings.IndexByte(ln, ':'); i > 0 && strings.EqualFold(strings.TrimSpace(ln[:i]), name) {
				return strings.TrimSpace(ln[i+1:])
			}
		}
		return ""
	}
	pf := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	pi := func(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }
	five = RateLimit{
		UsedPct:  pf(get("Anthropic-Ratelimit-Unified-5h-Utilization")) * 100,
		ResetsAt: pi(get("Anthropic-Ratelimit-Unified-5h-Reset")),
	}
	seven = RateLimit{
		UsedPct:  pf(get("Anthropic-Ratelimit-Unified-7d-Utilization")) * 100,
		ResetsAt: pi(get("Anthropic-Ratelimit-Unified-7d-Reset")),
	}
	ok = five.ResetsAt != 0 || seven.ResetsAt != 0
	return
}

// pollClaudeUnifiedQuota periodically reads the account's LIVE 5h/weekly
// utilization from Anthropic's unified rate-limit response headers and feeds
// the claude quota bucket. Claude Code 2.1.x stopped writing fresh rate_limits
// to the statusline (the values freeze at the last header it happened to
// surface — e.g. a weekly that already reset days ago), but the headers
// themselves ride every API response. So we make a tiny throwaway call and read
// them ourselves. The box's claude is clawpatrol-wrapped (its local oauth token
// is inert; the gateway injects the real cred), so the probe is DERIVED from the
// VM's session_cmd: we swap the `clawpatrol run claude …` invocation for
// `clawpatrol run -- curl …`, reusing the runuser/env/clawpatrol setup verbatim
// so the call authenticates through the very same gateway claude uses.
func pollClaudeUnifiedQuota(ctx context.Context, vm VMBlock, bcast chan<- struct{}) {
	account := vmAccount(vm)
	// sshExec runs this through a shell (`bash -c` locally, the remote shell over
	// ssh), so the JSON body MUST be single-quoted: unquoted, bash brace-expands
	// the `{...,...}` and shreds the request. The body itself uses only double
	// quotes, so single-quoting is safe.
	const body = `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	probe := "clawpatrol run -- curl -s -D - -o /dev/null -X POST https://api.anthropic.com/v1/messages " +
		"-H anthropic-version:2023-06-01 -H content-type:application/json -d '" + body + "'"
	remote := strings.Replace(vm.SessionCmd, "clawpatrol run claude --dangerously-skip-permissions", probe, 1)
	if !strings.Contains(remote, "curl") {
		log.Printf("usage: claude unified-quota poller: %s session_cmd has no clawpatrol claude invocation; skipping", vm.Name)
		return
	}
	log.Printf("usage: polling claude unified rate-limit headers via %s (account=%s)", vm.Name, account)
	for ctx.Err() == nil {
		if out, _, err := sshExec(vm, remote); err == nil {
			if five, seven, okp := parseUnifiedRateHeaders(out); okp {
				setAgentQuota(account, five, seven, "", nil)
				if bcast != nil {
					select {
					case bcast <- struct{}{}:
					default:
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}

// codexRolloutTailScript follows the NEWEST codex session rollout JSONL under
// the given codex-home dir and re-discovers on rotation: when a newer
// rollout-*.jsonl appears (a new codex session started) it kills the current
// tail and follows the new file. All rotation logic runs host-side so the orch
// reader is the same dumb line loop as tailStatusLine. homeExpr is a shell
// expression for the codex home directory (CODEX_HOME, or "$HOME/.codex"); its
// sessions/ subdir holds the rollouts.
func codexRolloutTailScript(homeExpr string) string {
	return `H=` + homeExpr + `; ` +
		`while :; do ` +
		`f=$(ls -t "$H"/sessions/*/*/*/rollout-*.jsonl 2>/dev/null | head -1); ` +
		`if [ -z "$f" ]; then sleep 5; continue; fi; ` +
		`tail -F -n 200 "$f" & tpid=$!; ` +
		`while :; do sleep 15; ` +
		`nf=$(ls -t "$H"/sessions/*/*/*/rollout-*.jsonl 2>/dev/null | head -1); ` +
		`if [ "$nf" != "$f" ]; then kill $tpid 2>/dev/null; break; fi; done; ` +
		`wait $tpid 2>/dev/null; done`
}

// tailCodexUsage follows the codex rate_limits stream for a codex VM and feeds
// that VM's account quota bucket, mirroring tailStatusLine. Reads the VM's
// CODEX_HOME (so a second codex account on the same host meters separately);
// defaults to ~/.codex. Started per codex VM in cli.go.
func tailCodexUsage(ctx context.Context, vm VMBlock, bcast chan<- struct{}) {
	account := vmAccount(vm)
	// homeExpr is a double-quoted shell expression so $HOME inside a configured
	// codex_home (e.g. "$HOME/.codex-mini") expands on the remote host.
	homeExpr := `"$HOME/.codex"`
	if vm.CodexHome != "" {
		homeExpr = `"` + vm.CodexHome + `"`
	}
	script := codexRolloutTailScript(homeExpr)
	log.Printf("usage: tailing codex rollouts on %s (account=%s, home=%s)", vm.Name, account, homeExpr)
	for ctx.Err() == nil {
		var cmd *exec.Cmd
		if isLocal(vm) {
			cmd = exec.CommandContext(ctx, "sh", "-c", script)
		} else {
			cmd = exec.CommandContext(ctx, "ssh", append(sshArgs(vm), script)...)
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
			ingestCodexRollout(sc.Bytes(), account)
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
