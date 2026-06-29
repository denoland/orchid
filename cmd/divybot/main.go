// Command divybot is the new orchid: a single-file GitHub-issue → PR swarm
// coordinator over a herdr fabric. It replaces the old multi-file orchid (SSH +
// tmux-shim + capture-pane + paste-buffer pokes + dashboard + clawpatrol) with a
// thin scheduler that drives each host's herdr server natively over SSH.
//
// Pipeline:  poll issues → governor-admit → push creds → herdr spawn (bare
// claude/codex) → inject goal → supervise via herdr agent_status → poll PRs and
// relay reviews/CI/conflicts via herdr send → teardown.
//
// Perception is herdr's native agent_status (idle|working|blocked|done|unknown),
// one structured call per host per tick — not a 5Hz scrape. Auth is centrally
// owned: the coordinator holds the canonical claude oauth + codex auth + gh token
// and pushes them to every host on spawn and on an interval (killing the
// per-host stale-credential 401/JWT class). No clawpatrol, no dashboard, no shim.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ============================ config ============================

type Config struct {
	Inbox        string   `json:"inbox"`         // e.g. "denoland/divybot"
	BotLogin     string   `json:"bot_login"`     // PR author login (for review attribution)
	BotEmail     string   `json:"bot_email"`     // git committer email
	PollInterval string   `json:"poll_interval"` // e.g. "30s"
	BranchPrefix string   `json:"branch_prefix"` // e.g. "orch/divybot-"
	StateFile    string   `json:"state_file"`    // e.g. "/root/divybot/state.json"
	NtfyTopic    string   `json:"ntfy_topic"`    // ntfy.sh topic for escalation (optional)
	Hosts        []Host   `json:"hosts"`
	Targets      []Target `json:"targets"`
	Governor     Gov      `json:"governor"`
	Memory       Mem      `json:"memory"`
}

// Mem configures the git-backed shared memory. Reuses the inbox repo
// (denoland/divybot) on main, subtree memory/. Each host keeps a clone; claude's
// autoMemoryDirectory points at <clone>/memory so workers read the union and
// write locally. Robustness: the coordinator syncs hosts SERIALLY (one committer
// at a time → no push race), memory/.gitattributes sets `* merge=union` (concurrent
// writes MERGE, never override), and the repo subtree is memory-only so auth is
// never in the blast radius.
type Mem struct {
	Enabled  bool   `json:"enabled"`
	Repo     string `json:"repo"`     // default: inbox
	Branch   string `json:"branch"`   // default: main
	Dir      string `json:"dir"`      // subtree, default: memory
	Interval string `json:"interval"` // default: 5m
}

// Host is one reachable box on the tailnet running a herdr server. No "join": a
// host is usable iff it's on the tailnet and its herdr answers. Capabilities
// gate routing (a deno build needs a beefy box, not a phone).
type Host struct {
	Name         string   `json:"name"`
	SSH          string   `json:"ssh"`          // ssh target, e.g. "orchid@100.112.121.20" or "localhost"
	Key          string   `json:"key"`          // ssh key path; "" = default/agent
	Home         string   `json:"home"`         // agent user $HOME (creds + socket)
	WorkdirRoot  string   `json:"workdir_root"` // where issue worktrees live
	Capabilities []string `json:"capabilities"` // e.g. ["build-deno","windows"]
	Capacity     int      `json:"capacity"`     // max concurrent agents
}

// Target maps an inbox-issue label to a work repo + which agent runs it.
type Target struct {
	Label string `json:"label"` // inbox label, e.g. "deno"
	Repo  string `json:"repo"`  // e.g. "denoland/deno"
	Agent string `json:"agent"` // "claude" | "codex" (default claude)
	// Agents is an ordered agent-overflow preference. When set it overrides Agent:
	// admission spawns the issue on the FIRST listed agent whose account still has
	// governor budget, so work spills to an idle account when the preferred one is
	// throttled (e.g. ["claude","codex"] => prefer claude, fall to codex when
	// claude is capped). Empty => just [Agent]. Each account paces independently.
	Agents  []string `json:"agents"`
	NeedCap string   `json:"need_cap"` // required host capability, e.g. "build-deno"
	// AutoMerge enables divybot to squash-merge a worker's PR once it is non-draft,
	// MERGEABLE, and all CI checks pass — closing the green-PR → merge → fan-out
	// loop without a human. Opt-in per repo; only set it where divybot has merge
	// rights AND auto-merging bot PRs without human review is acceptable.
	AutoMerge bool `json:"automerge"`
}

// Gov holds the quota-pacing knobs (the governor — paces against the Max
// subscription / codex plan so the swarm spends the budget evenly instead of
// blowing the window early). It reads each account's REAL rate-limit meter
// (claude's statusline rate_limits / codex's rollout token_count) and runs a
// burn-rate adaptive cap PER ACCOUNT, so claude and codex pace independently.
type Gov struct {
	Enabled       bool    `json:"enabled"`
	WeeklyCeiling float64 `json:"weekly_ceiling_pct"` // hard-pause new work at/above this used% (default 92)
	Slack         float64 `json:"slack_pct"`          // floor the cap to MinActive within this of the ceiling (default 8)
	MaxActive     int     `json:"max_active"`         // ceiling for the adaptive per-account cap (default 16)
	MinActive     int     `json:"min_active"`         // never fully stall while under budget (default 1)
	// Burn-rate estimator windows + sampling cadence (mirrors the old orchid
	// governor). RateWindow is the weekly-bucket burn lookback, FiveRateWindow
	// the 5h-bucket lookback, SampleInterval how often the live meter is read.
	RateWindow     string `json:"rate_window"`      // default 3h
	FiveRateWindow string `json:"five_rate_window"` // default 45m
	SampleInterval string `json:"sample_interval"`  // default 90s
}

func (g Gov) rateWindowDur() time.Duration     { return durOr(g.RateWindow, 3*time.Hour) }
func (g Gov) fiveRateWindowDur() time.Duration { return durOr(g.FiveRateWindow, 45*time.Minute) }
func (g Gov) sampleIntervalDur() time.Duration { return durOr(g.SampleInterval, 90*time.Second) }

func (c *Config) withDefaults() {
	if c.PollInterval == "" {
		c.PollInterval = "30s"
	}
	if c.BranchPrefix == "" {
		c.BranchPrefix = "orch/divybot-"
	}
	if c.StateFile == "" {
		c.StateFile = "state.json"
	}
	if c.Governor.WeeklyCeiling == 0 {
		c.Governor.WeeklyCeiling = 92
	}
	if c.Governor.Slack == 0 {
		c.Governor.Slack = 8
	}
	if c.Governor.MaxActive == 0 {
		c.Governor.MaxActive = 16
	}
	if c.Governor.MinActive == 0 {
		c.Governor.MinActive = 1
	}
	for i := range c.Hosts {
		if c.Hosts[i].Capacity == 0 {
			c.Hosts[i].Capacity = 4
		}
	}
	if c.Memory.Repo == "" {
		c.Memory.Repo = c.Inbox
	}
	if c.Memory.Branch == "" {
		c.Memory.Branch = "main"
	}
	if c.Memory.Dir == "" {
		c.Memory.Dir = "memory"
	}
	if c.Memory.Interval == "" {
		c.Memory.Interval = "5m"
	}
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	c.withDefaults()
	return &c, nil
}

// ============================ state ============================

// tracker records what PR activity a job has already relayed, so we only forward
// NEW reviews/comments/CI to the worker each tick.
type tracker struct {
	Reviews       []string          `json:"reviews"`        // seen review IDs
	Comments      []string          `json:"comments"`       // seen review-thread comment IDs
	IssueComments []string          `json:"issue_comments"` // seen PR conversation comment IDs
	Checks        map[string]string `json:"checks"`         // check name → last conclusion
	HeadOID       string            `json:"head_oid"`       // last seen PR head commit
	Conflicted    bool              `json:"conflicted"`     // already relayed the current merge conflict
}

type Job struct {
	Issue     int       `json:"issue"`
	Host      string    `json:"host"`
	Label     string    `json:"label"`     // display name (claude-<n>)
	Pane      string    `json:"pane"`      // herdr send/read target (pane id)
	Workspace string    `json:"workspace"` // herdr teardown handle
	Target    string    `json:"target"`
	Repo      string    `json:"repo"`
	Branch    string    `json:"branch"`
	Agent     string    `json:"agent"`
	Title     string    `json:"title"`
	Goal      string    `json:"goal"`
	PR        int       `json:"pr"`
	SpawnedAt time.Time `json:"spawned_at"`
	LastPoke  time.Time `json:"last_poke"`
	// FanoutNudgedAt is when we nudged this job's worker (on its PR merge) to fan
	// out remaining work into sibling inbox issues. Teardown is deferred until the
	// worker files a sibling stub or fanoutGraceWindow elapses — a single tick is
	// far too short for the worker to wake, decide, and run gh issue create.
	FanoutNudgedAt time.Time `json:"fanout_nudged_at,omitempty"`
	Track          tracker   `json:"track"`
}

type State struct {
	mu   sync.Mutex
	Jobs map[int]*Job `json:"jobs"`
	// Continued counts how many CONTINUATION stubs we've re-filed per upstream ref
	// ("owner/repo#N"), so a never-closing upstream can't churn forever. Persisted.
	Continued map[string]int `json:"continued"`
	// QuotaSamples is the governor's per-account burn-rate time series (account =>
	// readings), persisted so the burn estimate survives a restart instead of
	// going blind for a full RateWindow. PrevCap is each account's last adaptive
	// cap, the slew anchor across ticks/restarts.
	QuotaSamples map[string][]QuotaSample `json:"quota_samples"`
	PrevCap      map[string]int           `json:"prev_cap"`
	path         string
}

func loadState(path string) *State {
	s := &State{Jobs: map[int]*Job{}, Continued: map[string]int{}, QuotaSamples: map[string][]QuotaSample{}, PrevCap: map[string]int{}, path: path}
	if b, err := os.ReadFile(path); err == nil {
		var raw struct {
			Jobs         map[int]*Job             `json:"jobs"`
			Continued    map[string]int           `json:"continued"`
			QuotaSamples map[string][]QuotaSample `json:"quota_samples"`
			PrevCap      map[string]int           `json:"prev_cap"`
		}
		if json.Unmarshal(b, &raw) == nil {
			if raw.Jobs != nil {
				s.Jobs = raw.Jobs
			}
			if raw.Continued != nil {
				s.Continued = raw.Continued
			}
			if raw.QuotaSamples != nil {
				s.QuotaSamples = raw.QuotaSamples
			}
			if raw.PrevCap != nil {
				s.PrevCap = raw.PrevCap
			}
		}
	}
	return s
}

func (s *State) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(struct {
		Jobs         map[int]*Job             `json:"jobs"`
		Continued    map[string]int           `json:"continued"`
		QuotaSamples map[string][]QuotaSample `json:"quota_samples"`
		PrevCap      map[string]int           `json:"prev_cap"`
	}{s.Jobs, s.Continued, s.QuotaSamples, s.PrevCap}, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// ============================ exec helpers ============================

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, p[2:])
	}
	return p
}

// ============================ herdr client ============================

func (h Host) isLocal() bool {
	return h.SSH == "" || h.SSH == "localhost" || h.SSH == "127.0.0.1"
}

func (h Host) has(cap string) bool {
	for _, c := range h.Capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

func (h Host) agentHome() string {
	if h.Home != "" {
		return h.Home
	}
	if h.isLocal() {
		return "/root"
	}
	if i := strings.IndexByte(h.SSH, '@'); i > 0 {
		return "/home/" + h.SSH[:i]
	}
	return "/root"
}

func (h Host) sshBase() []string {
	a := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "ServerAliveInterval=30", "-o", "ServerAliveCountMax=3"}
	if h.Key != "" {
		a = append(a, "-i", expandHome(h.Key))
	}
	return a
}

// runRemote runs a shell command on the host (local = bash -c, remote = ssh).
func (h Host) runRemote(ctx context.Context, script string) (string, error) {
	if h.isLocal() {
		return run(ctx, "bash", "-c", script)
	}
	return run(ctx, "ssh", append(h.sshBase(), h.SSH, script)...)
}

// herdr runs `herdr <args>` on the host with HOME + PATH set for the socket.
func (h Host) herdr(ctx context.Context, args ...string) (string, error) {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shq(a)
	}
	home := h.agentHome()
	script := fmt.Sprintf(`export HOME=%s; export PATH="$HOME/.local/bin:/usr/local/bin:$PATH"; herdr %s`,
		shq(home), strings.Join(q, " "))
	return h.runRemote(ctx, script)
}

type herdrEnv struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code, Message string
	} `json:"error"`
}

func herdrUnwrap(out string) (json.RawMessage, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		ln := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(ln, "{") {
			continue
		}
		var e herdrEnv
		if json.Unmarshal([]byte(ln), &e) != nil {
			continue
		}
		if e.Error != nil {
			return nil, fmt.Errorf("herdr %s: %s", e.Error.Code, e.Error.Message)
		}
		return e.Result, nil
	}
	return nil, fmt.Errorf("no herdr json: %.160q", out)
}

type AgentInfo struct {
	Agent       string `json:"agent"`
	AgentStatus string `json:"agent_status"`
	Cwd         string `json:"cwd"`
	PaneID      string `json:"pane_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	Name        string `json:"name"`
}

func (h Host) agentList(ctx context.Context) ([]AgentInfo, error) {
	out, err := h.herdr(ctx, "agent", "list")
	if err != nil {
		return nil, fmt.Errorf("%v: %.160q", err, out)
	}
	raw, err := herdrUnwrap(out)
	if err != nil {
		return nil, err
	}
	var r struct {
		Agents []AgentInfo `json:"agents"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return r.Agents, nil
}

// spawnAgent creates a DEDICATED single-pane workspace and launches a BARE agent
// in its root pane (no clawpatrol). Each agent gets its own workspace — earlier,
// `agent start` without an isolated workspace piled every agent into one (10+
// unrelated sessions crammed together). We run the agent IN the root pane via
// `pane run` (env exported inline + exec) so the workspace stays exactly 1 pane,
// rather than `agent start` adding a second pane beside an idle shell.
func (h Host) spawnAgent(ctx context.Context, label, cwd string, env map[string]string, agentCmd string) (pane, ws string, err error) {
	wout, werr := h.herdr(ctx, "workspace", "create", "--label", label, "--cwd", cwd, "--no-focus")
	if werr != nil {
		return "", "", fmt.Errorf("workspace create: %v: %.120q", werr, wout)
	}
	wraw, e := herdrUnwrap(wout)
	if e != nil {
		return "", "", e
	}
	// workspace_id/root pane are NOT at result top-level — they're under
	// .workspace / .root_pane.
	var wr struct {
		Workspace struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspace"`
		RootPane struct {
			PaneID      string `json:"pane_id"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"root_pane"`
	}
	_ = json.Unmarshal(wraw, &wr)
	ws = wr.Workspace.WorkspaceID
	if ws == "" {
		ws = wr.RootPane.WorkspaceID
	}
	pane = wr.RootPane.PaneID
	if ws == "" || pane == "" {
		return "", "", fmt.Errorf("workspace create returned no id/pane: %.160q", wout)
	}
	// Build the launch command: PATH guard, export env, then exec the agent.
	var b strings.Builder
	b.WriteString(`export PATH="$HOME/.local/bin:/usr/local/bin:$PATH"; `)
	for k, v := range env {
		if v != "" {
			fmt.Fprintf(&b, "export %s=%s; ", k, shq(v))
		}
	}
	b.WriteString("exec ")
	b.WriteString(agentCmd)
	if out, err := h.herdr(ctx, "pane", "run", pane, b.String()); err != nil {
		return "", "", fmt.Errorf("pane run: %v: %.160q", err, out)
	}
	return pane, ws, nil
}

// send injects literal text then submits with Enter.
func (h Host) send(ctx context.Context, target, text string) error {
	if out, err := h.herdr(ctx, "agent", "send", target, text); err != nil {
		return fmt.Errorf("send: %v: %.120q", err, out)
	}
	if out, err := h.herdr(ctx, "pane", "send-keys", target, "Enter"); err != nil {
		return fmt.Errorf("enter: %v: %.120q", err, out)
	}
	return nil
}

// agentBareIdle reports whether the pane shows an idle, empty agent prompt
// (the "bypass permissions" footer is present only when claude is sitting
// at an empty input box). Used to detect (a) that the TUI has finished
// booting and (b) after a send, that the input was NOT accepted — once a
// task is submitted claude goes busy and the footer disappears.
func agentBareIdle(pane string) bool {
	return strings.Contains(pane, "bypass permissions")
}

// injectGoal delivers the goal to a freshly-spawned agent reliably. It
// waits for the prompt to render, sends, then confirms the agent left the
// bare prompt (i.e. accepted the task). A dropped keystroke during boot or
// an Enter that raced ahead of a long paste both leave the worker idle;
// this retries (nudging Enter, then clearing and resending) until the goal
// registers or the context expires.
func (h Host) injectGoal(ctx context.Context, target, goal string) error {
	// Wait for the TUI to render its input prompt (up to ~the ctx budget).
	for i := 0; i < 30; i++ {
		if agentBareIdle(h.read(ctx, target, 6)) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	for attempt := 0; attempt < 4; attempt++ {
		if err := h.send(ctx, target, goal); err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(4 * time.Second):
		}
		if !agentBareIdle(h.read(ctx, target, 12)) {
			return nil // busy → task accepted
		}
		// Still at a bare prompt: the text may have landed but the Enter
		// raced ahead of it. Nudge Enter once before a full resend.
		_, _ = h.herdr(ctx, "pane", "send-keys", target, "Enter")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
		if !agentBareIdle(h.read(ctx, target, 12)) {
			return nil
		}
		// Neither took: clear whatever's in the box and retry from scratch.
		_, _ = h.herdr(ctx, "pane", "send-keys", target, "Escape")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
	return fmt.Errorf("goal did not register after retries")
}

func (h Host) read(ctx context.Context, target string, lines int) string {
	out, _ := h.herdr(ctx, "agent", "read", target, "--source", "recent", "--lines", strconv.Itoa(lines), "--format", "text")
	if raw, err := herdrUnwrap(out); err == nil {
		var r struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &r) == nil && r.Text != "" {
			return r.Text
		}
	}
	return out
}

func (h Host) closeWorkspace(ctx context.Context, ws string) error {
	out, err := h.herdr(ctx, "workspace", "close", ws)
	if err != nil {
		return fmt.Errorf("%v: %.120q", err, out)
	}
	return nil
}

// ============================ auth sync ============================

type AuthStore struct {
	mu           sync.Mutex
	ClaudeCreds  string
	ClaudeConfig string
	CodexAuth    string
	GHToken      string
}

func defaultAuth() *AuthStore {
	home, _ := os.UserHomeDir()
	a := &AuthStore{
		ClaudeCreds:  filepath.Join(home, ".claude", ".credentials.json"),
		ClaudeConfig: filepath.Join(home, ".claude.json"),
		CodexAuth:    filepath.Join(home, ".codex", "auth.json"),
	}
	a.GHToken = resolveGHToken()
	return a
}

func resolveGHToken() string {
	if t := strings.TrimSpace(os.Getenv("GH_TOKEN")); t != "" {
		return t
	}
	if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func (a *AuthStore) token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.GHToken
}

// scpTo copies a local file to dest on a host (creating parent dirs).
func (h Host) scpTo(ctx context.Context, local, dest string) error {
	if _, err := os.Stat(local); err != nil {
		return nil // source absent → skip
	}
	dir := filepath.Dir(dest)
	if h.isLocal() {
		_ = os.MkdirAll(dir, 0o700)
		_, err := run(ctx, "cp", local, dest)
		return err
	}
	if out, err := run(ctx, "ssh", append(h.sshBase(), h.SSH, "mkdir -p "+shq(dir))...); err != nil {
		return fmt.Errorf("mkdir: %v: %.80q", err, out)
	}
	scp := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=accept-new"}
	if h.Key != "" {
		scp = append(scp, "-i", expandHome(h.Key))
	}
	scp = append(scp, local, h.SSH+":"+dest)
	if out, err := run(ctx, "scp", scp...); err != nil {
		return fmt.Errorf("scp: %v: %.80q", err, out)
	}
	return nil
}

// scpFrom copies a file FROM a host to a local path (used to adopt the freshest
// self-refreshed creds back as canonical).
func (h Host) scpFrom(ctx context.Context, remote, local string) error {
	_ = os.MkdirAll(filepath.Dir(local), 0o700)
	if h.isLocal() {
		_, err := run(ctx, "cp", remote, local)
		return err
	}
	scp := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=accept-new"}
	if h.Key != "" {
		scp = append(scp, "-i", expandHome(h.Key))
	}
	scp = append(scp, h.SSH+":"+remote, local)
	if out, err := run(ctx, "scp", scp...); err != nil {
		return fmt.Errorf("scp: %v: %.80q", err, out)
	}
	return nil
}

// credExpiry parses the oauth access-token expiry (ms epoch) from a claude
// .credentials.json blob; 0 if absent/unparseable.
func credExpiry(b []byte) int64 {
	var d struct {
		ClaudeAiOauth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(b, &d) == nil {
		return d.ClaudeAiOauth.ExpiresAt
	}
	return 0
}

func credExpiryFile(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return credExpiry(b)
}

func (h Host) credExpiryRemote(ctx context.Context) int64 {
	out, err := h.runRemote(ctx, "cat "+shq(h.agentHome()+"/.claude/.credentials.json")+" 2>/dev/null")
	if err != nil {
		return 0
	}
	return credExpiry([]byte(out))
}

// syncToHost pushes all canonical creds to one host's agent home.
func (a *AuthStore) syncToHost(ctx context.Context, h Host) error {
	a.mu.Lock()
	creds, cfg, codex, tok := a.ClaudeCreds, a.ClaudeConfig, a.CodexAuth, a.GHToken
	a.mu.Unlock()
	home := h.agentHome()
	var errs []string
	// Refresh-safe: push the canonical claude creds ONLY if the host has none or
	// the host's are OLDER. claude rotates the oauth refresh token on every refresh;
	// blindly overwriting a host's self-refreshed creds with a staler canonical
	// reverts the rotation and eventually kills the whole refresh chain (the bug
	// that 401'd the swarm). Codex/gh have no rotation, so push them unconditionally.
	canonExp := credExpiryFile(creds)
	hostExp := h.credExpiryRemote(ctx)
	if hostExp == 0 || canonExp > hostExp {
		if err := h.scpTo(ctx, creds, filepath.Join(home, ".claude", ".credentials.json")); err != nil {
			errs = append(errs, "claude:"+err.Error())
		}
	}
	_ = h.scpTo(ctx, cfg, filepath.Join(home, ".claude.json"))
	_ = h.scpTo(ctx, codex, filepath.Join(home, ".codex", "auth.json"))
	if tok != "" {
		gh := fmt.Sprintf("github.com:\n    oauth_token: %s\n    git_protocol: https\n", tok)
		dest := filepath.Join(home, ".config", "gh", "hosts.yml")
		script := fmt.Sprintf("mkdir -p %s && cat > %s <<'GHEOF'\n%sGHEOF\nchmod 600 %s",
			shq(filepath.Dir(dest)), shq(dest), gh, shq(dest))
		if out, err := h.runRemote(ctx, script); err != nil {
			errs = append(errs, fmt.Sprintf("gh:%v:%.60q", err, out))
		}
	}
	// Worker Claude Code settings: kill the "Co-Authored-By: Claude" trailer +
	// "Generated with Claude Code" footer (includeCoAuthoredBy=false) and skip the
	// dangerous-mode prompt. Bot is the sole author. Merge-preserve any existing keys
	// (e.g. autoMemoryDirectory written by ensureMemory). Runs every authsync.
	settings := `SJ="$HOME/.claude/settings.json"; mkdir -p "$HOME/.claude"; [ -f "$SJ" ] || echo '{}' > "$SJ"
if command -v jq >/dev/null 2>&1; then t=$(mktemp); jq '.includeCoAuthoredBy=false | .skipDangerousModePermissionPrompt=true' "$SJ" > "$t" 2>/dev/null && mv "$t" "$SJ" || rm -f "$t"; fi`
	_, _ = h.runRemote(ctx, settings)
	if len(errs) > 0 {
		return fmt.Errorf("authsync %s: %s", h.Name, strings.Join(errs, "; "))
	}
	return nil
}

// refreshLocal triggers a claude oauth refresh on the coordinator (cheap noop
// call) so the canonical creds file is fresh before we push it.
func (a *AuthStore) refreshLocal(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "claude", "-p", "ok")
	cmd.Stdin = strings.NewReader("")
	_ = cmd.Run()
	if t := resolveGHToken(); t != "" {
		a.mu.Lock()
		a.GHToken = t
		a.mu.Unlock()
	}
}

// ============================ shared memory ============================

// memRepoDir is the persistent memory clone path on a host.
func (h Host) memRepoDir() string {
	return h.agentHome() + "/.orch/memory-repo"
}

// ensureMemory makes a host ready for shared memory (idempotent, run at spawn):
// clone the memory repo if absent, install the union merge driver scoped to the
// memory subtree, and point claude's autoMemoryDirectory at <clone>/<dir>. Auth
// for the clone/push uses the gh credential helper (hosts.yml the coordinator
// already wrote), so no token ever lands on a command line.
func (h Host) ensureMemory(ctx context.Context, m Mem, bot, email string) error {
	home := h.agentHome()
	repoURL := "https://github.com/" + m.Repo + ".git"
	clone := h.memRepoDir()
	memdir := clone + "/" + m.Dir
	script := fmt.Sprintf(`set -e
export HOME=%s
export PATH="$HOME/.local/bin:/usr/local/bin:$PATH"
command -v gh >/dev/null 2>&1 && gh auth setup-git >/dev/null 2>&1 || true
R=%s
if [ ! -d "$R/.git" ]; then rm -rf "$R"; mkdir -p "$(dirname "$R")"; git clone --depth=1 -b %s %s "$R" >/dev/null 2>&1 || git clone --depth=1 %s "$R" >/dev/null 2>&1 || true; fi
[ -d "$R/.git" ] || exit 0
git -C "$R" config user.name %s >/dev/null 2>&1 || true
git -C "$R" config user.email %s >/dev/null 2>&1 || true
mkdir -p %s
# union merge ONLY within the memory subtree → concurrent writes merge, never override
printf '%%s\n' '* merge=union' > %s/.gitattributes
# point claude auto-memory at the shared clone (settings.json, separate from creds)
SJ="$HOME/.claude/settings.json"; mkdir -p "$HOME/.claude"; [ -f "$SJ" ] || echo '{}' > "$SJ"
if command -v jq >/dev/null 2>&1; then t=$(mktemp); jq --arg d %s '.autoMemoryEnabled=true | .autoMemoryDirectory=$d' "$SJ" > "$t" 2>/dev/null && mv "$t" "$SJ" || rm -f "$t"; fi
# native skills: refresh the clone and mirror its skills/ subtree into the host's
# personal skill dir so claude auto-discovers them. Kept OUT of the target worktree
# (~/.claude/skills, not <repo>/.claude) → they never leak into a PR.
git -C "$R" pull --rebase --autostash -q origin %s >/dev/null 2>&1 || true
if [ -d "$R/skills" ]; then mkdir -p "$HOME/.claude/skills"; cp -a "$R/skills/." "$HOME/.claude/skills/" 2>/dev/null || true; fi
# codex has no native skill discovery — point its global AGENTS.md at the mirrored
# skill files (idempotent via marker; runs only if ~/.codex exists).
if [ -d "$HOME/.codex" ] && ! grep -qs 'divybot-skills' "$HOME/.codex/AGENTS.md" 2>/dev/null; then printf '\n<!-- divybot-skills -->\nReusable skills live in ~/.claude/skills/<name>/SKILL.md. Before starting, scan those SKILL.md files; if one matches the task (e.g. windows-deno-desktop-testing for deno desktop / native-window issues), follow it.\n' >> "$HOME/.codex/AGENTS.md"; fi
`,
		shq(home), shq(clone), shq(m.Branch), shq(repoURL), shq(repoURL),
		shq(bot), shq(email), shq(memdir), shq(memdir), shq(memdir), shq(m.Branch))
	out, err := h.runRemote(ctx, script)
	if err != nil {
		return fmt.Errorf("ensureMemory %s: %v: %.120q", h.Name, err, out)
	}
	return nil
}

// syncMemory commits the host's local memory writes and integrates the remote.
// The coordinator calls this SERIALLY across hosts (one committer at a time), so
// pushes never race; the union merge driver makes any same-file overlap merge
// rather than clobber. Scoped to the memory subtree — code and auth untouched.
func (h Host) syncMemory(ctx context.Context, m Mem, bot, email string) error {
	home := h.agentHome()
	clone := h.memRepoDir()
	script := fmt.Sprintf(`set -e
export HOME=%s
export PATH="$HOME/.local/bin:/usr/local/bin:$PATH"
R=%s; DIR=%s
[ -d "$R/.git" ] || exit 0
cd "$R"
git config user.name %s >/dev/null 2>&1 || true
git config user.email %s >/dev/null 2>&1 || true
git add "$DIR" >/dev/null 2>&1 || true
if ! git diff --cached --quiet 2>/dev/null; then git commit -q -m "memory sync $(hostname) $(date -u +%%FT%%TZ)" >/dev/null 2>&1 || true; fi
git pull --rebase --autostash origin %s >/dev/null 2>&1 || { git rebase --abort >/dev/null 2>&1 || true; }
git push origin HEAD:%s >/dev/null 2>&1 || true
`,
		shq(home), shq(clone), shq(m.Dir), shq(bot), shq(email), shq(m.Branch), shq(m.Branch))
	out, err := h.runRemote(ctx, script)
	if err != nil {
		return fmt.Errorf("syncMemory %s: %v: %.120q", h.Name, err, out)
	}
	return nil
}

// ============================ gh helpers ============================

type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"-"`
}

func ghJSON(ctx context.Context, out any, args ...string) error {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return err
	}
	return json.Unmarshal(buf.Bytes(), out)
}

func ghIssues(ctx context.Context, inbox, label string) ([]Issue, error) {
	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := ghJSON(ctx, &raw, "issue", "list", "--repo", inbox, "--label", label,
		"--state", "open", "--limit", "100", "--json", "number,title,body,labels"); err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(raw))
	for _, r := range raw {
		is := Issue{Number: r.Number, Title: r.Title, Body: r.Body}
		for _, l := range r.Labels {
			is.Labels = append(is.Labels, l.Name)
		}
		out = append(out, is)
	}
	return out, nil
}

// assignedIssue is one row from `gh api /search/issues?q=assignee:<bot>`.
// We dedupe against NodeID — GitHub recycles issue numbers per repo, but
// node ids are globally unique and stable across renames.
type assignedIssue struct {
	NodeID string
	Repo   string
	Number int
	Title  string
	Body   string
	URL    string
	Author string
}

// searchAssignments returns open issues in `repo` currently assigned to
// `bot`. Caller dedupes via node id so a re-poll never re-creates the
// inbox issue. No --paginate: assigned-issue counts per repo stay well
// under one page (per_page=50), and --paginate concatenates per-page
// JSON objects which would break the single-object Unmarshal below.
func searchAssignments(ctx context.Context, repo, bot string) ([]assignedIssue, error) {
	var resp struct {
		Items []struct {
			NodeID  string                 `json:"node_id"`
			Number  int                    `json:"number"`
			Title   string                 `json:"title"`
			Body    string                 `json:"body"`
			HTMLURL string                 `json:"html_url"`
			User    struct{ Login string } `json:"user"`
		} `json:"items"`
	}
	if err := ghJSON(ctx, &resp, "api", "-H", "Accept: application/vnd.github+json",
		fmt.Sprintf("/search/issues?q=repo:%s+assignee:%s+is:issue+is:open&per_page=50", repo, bot)); err != nil {
		return nil, err
	}
	res := make([]assignedIssue, 0, len(resp.Items))
	for _, it := range resp.Items {
		res = append(res, assignedIssue{
			NodeID: it.NodeID, Repo: repo, Number: it.Number,
			Title: it.Title, Body: it.Body, URL: it.HTMLURL, Author: it.User.Login,
		})
	}
	return res, nil
}

// inboxMirrored returns the set of upstream refs ("owner/repo#N") that
// already have an inbox issue, parsed from the "[owner/repo#N] ..."
// title convention across BOTH open and closed inbox issues. The inbox
// itself is the feeder's dedupe store — stateless, so a restart or a
// closed-then-reassigned issue never double-files, and no seed set has
// to be carried in state.json.
var inboxTitleRef = regexp.MustCompile(`^\[([^\]]+#\d+)\]`)

func inboxMirrored(ctx context.Context, inbox string) (map[string]bool, error) {
	var raw []struct {
		Title string `json:"title"`
	}
	if err := ghJSON(ctx, &raw, "issue", "list", "--repo", inbox,
		"--state", "all", "--limit", "1000", "--json", "title"); err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(raw))
	for _, r := range raw {
		if m := inboxTitleRef.FindStringSubmatch(r.Title); m != nil {
			have[m[1]] = true
		}
	}
	return have, nil
}

// assignmentTick scans every target repo for issues assigned to the bot
// and opens a matching inbox issue (labeled with the target's label) so
// the swarm picks them up on the same cycle. This is the feeder that
// turns "assign an upstream issue to @divybot" into work; without it the
// inbox only fills by hand. Dedupe is against the existing inbox titles
// (inboxMirrored) — if listing those fails we abort the tick rather than
// risk a duplicate storm.
func (c *Coord) assignmentTick(ctx context.Context) {
	bot := c.cfg.BotLogin
	if bot == "" {
		return
	}
	have, err := inboxMirrored(ctx, c.cfg.Inbox)
	if err != nil {
		log.Printf("assignments: inbox list failed, skipping feed: %v", err)
		return
	}
	for _, t := range c.cfg.Targets {
		if t.Repo == "" || t.Repo == c.cfg.Inbox || t.Label == "" {
			continue
		}
		issues, err := searchAssignments(ctx, t.Repo, bot)
		if err != nil {
			log.Printf("assignments: search %s × %s failed: %v", t.Repo, bot, err)
			continue
		}
		for _, it := range issues {
			ref := fmt.Sprintf("%s#%d", it.Repo, it.Number)
			if have[ref] {
				continue
			}
			title := fmt.Sprintf("[%s] %s", ref, it.Title)
			body := fmt.Sprintf(
				"Triggered by assignment of [%s](%s) to @%s.\n\nOpened by @%s.\n\n---\n\n%s",
				ref, it.URL, bot, it.Author, truncate(it.Body, 2000))
			out, err := run(ctx, "gh", "issue", "create",
				"--repo", c.cfg.Inbox, "--label", t.Label, "--title", title, "--body", body)
			if err != nil {
				log.Printf("assignments: gh issue create failed for %s: %v: %s",
					ref, err, strings.TrimSpace(out))
				continue
			}
			have[ref] = true // guard against the same ref twice in one pass
			log.Printf("assignments: opened inbox issue for %s (assignee=%s) → %s",
				ref, bot, strings.TrimSpace(out))
		}
	}
}

type PRView struct {
	Number    int
	HeadOID   string
	Mergeable string
	Reviews   []struct {
		ID     string
		Author string
		Body   string
		State  string
	}
	Comments []struct { // review-thread + conversation comments
		ID     string
		Author string
		Body   string
	}
	Checks []struct {
		Name       string
		Conclusion string
	}
	IsDraft bool
}

func ghPRView(ctx context.Context, repo string, n int) (*PRView, error) {
	var raw struct {
		Number     int    `json:"number"`
		Mergeable  string `json:"mergeable"`
		IsDraft    bool   `json:"isDraft"`
		HeadRefOid string `json:"headRefOid"`
		Reviews    []struct {
			ID     string                 `json:"id"`
			Author struct{ Login string } `json:"author"`
			Body   string                 `json:"body"`
			State  string                 `json:"state"`
		} `json:"reviews"`
		Comments []struct {
			ID     string                 `json:"id"`
			Author struct{ Login string } `json:"author"`
			Body   string                 `json:"body"`
		} `json:"comments"`
		StatusCheckRollup []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
		} `json:"statusCheckRollup"`
	}
	if err := ghJSON(ctx, &raw, "pr", "view", strconv.Itoa(n), "--repo", repo,
		"--json", "number,mergeable,isDraft,headRefOid,reviews,comments,statusCheckRollup"); err != nil {
		return nil, err
	}
	v := &PRView{Number: raw.Number, Mergeable: raw.Mergeable, HeadOID: raw.HeadRefOid, IsDraft: raw.IsDraft}
	for _, r := range raw.Reviews {
		v.Reviews = append(v.Reviews, struct{ ID, Author, Body, State string }{r.ID, r.Author.Login, r.Body, r.State})
	}
	for _, c := range raw.Comments {
		v.Comments = append(v.Comments, struct{ ID, Author, Body string }{c.ID, c.Author.Login, c.Body})
	}
	for _, ck := range raw.StatusCheckRollup {
		concl := ck.Conclusion
		if concl == "" {
			concl = ck.State
		}
		v.Checks = append(v.Checks, struct{ Name, Conclusion string }{ck.Name, concl})
	}
	return v, nil
}

func ghPRByBranch(ctx context.Context, repo, branch, author string) int {
	var raw []struct {
		Number int                    `json:"number"`
		Author struct{ Login string } `json:"author"`
	}
	if err := ghJSON(ctx, &raw, "pr", "list", "--repo", repo, "--head", branch,
		"--state", "open", "--json", "number,author"); err != nil {
		return 0
	}
	for _, p := range raw {
		if author == "" || p.Author.Login == author {
			return p.Number
		}
	}
	return 0
}

// ============================ PR relay (diff) ============================

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ciFailing reports whether a check-run conclusion (or legacy status state) is an
// ACTIONABLE failure worth nudging the worker about. Everything else — success,
// neutral, skipped, stale, cancelled/superseded, pending, "" — is not.
func ciFailing(conclusion string) bool {
	switch conclusion {
	case "FAILURE", "ERROR", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED":
		return true
	}
	return false
}

// diff returns new reviews/comments/CI/conflict since the last relay and updates
// the tracker. Mirrors orchid's diffPR semantics in slim form: only forward
// items we haven't seen, skip the bot's own posts, surface CI conclusion changes.
func diff(t *tracker, v *PRView, bot string) (lines []string, changed bool) {
	if t.Checks == nil {
		t.Checks = map[string]string{}
	}
	for _, r := range v.Reviews {
		if r.Author == bot || contains(t.Reviews, r.ID) {
			continue
		}
		t.Reviews = append(t.Reviews, r.ID)
		body := strings.TrimSpace(r.Body)
		if body == "" {
			body = r.State
		}
		lines = append(lines, fmt.Sprintf("review (%s by %s): %s", r.State, r.Author, body))
		changed = true
	}
	for _, c := range v.Comments {
		if c.Author == bot || contains(t.Comments, c.ID) {
			continue
		}
		t.Comments = append(t.Comments, c.ID)
		if strings.TrimSpace(c.Body) == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("comment by %s: %s", c.Author, strings.TrimSpace(c.Body)))
		changed = true
	}
	for _, ck := range v.Checks {
		// Only ACTIONABLE failures nudge the worker. SUCCESS/NEUTRAL/SKIPPED/STALE/
		// CANCELLED/PENDING and "" are not failures — record + move on. SKIPPED in
		// particular is the by-design skip-set: a CI rerun flips it every poll, which
		// used to re-poke the pane each tick and bury the worker in "address each item"
		// noise for checks that never needed action.
		if !ciFailing(ck.Conclusion) {
			t.Checks[ck.Name] = ck.Conclusion
			continue
		}
		if t.Checks[ck.Name] == ck.Conclusion {
			continue // already relayed this failure — single nudge, not every tick
		}
		t.Checks[ck.Name] = ck.Conclusion
		lines = append(lines, fmt.Sprintf("CI %s: %s", ck.Name, ck.Conclusion))
		changed = true
	}
	// Forward a merge conflict on first detection — INCLUDING when the head OID is
	// unchanged because the BASE branch advanced underneath the PR (the common case
	// the old head-OID-only gate silently dropped). Re-notify if the head later
	// moves but it's still conflicting; clear once GitHub reports it mergeable.
	// UNKNOWN (mergeability still computing) leaves the flag untouched.
	switch v.Mergeable {
	case "CONFLICTING":
		if !t.Conflicted || t.HeadOID != v.HeadOID {
			lines = append(lines, "merge-conflict: rebase onto the latest base branch, resolve conflicts, force-push.")
			changed = true
		}
		t.Conflicted = true
	case "MERGEABLE":
		t.Conflicted = false
	}
	t.HeadOID = v.HeadOID
	return lines, changed
}

// ============================ governor ============================

// RateLimit mirrors the rate-limit meter shape shared by claude (statusline
// rate_limits) and codex (rollout token_count): a used_percentage 0-100 plus a
// unix-second reset. The same shape covers both the 5h and weekly windows.
type RateLimit struct {
	UsedPct  float64
	ResetsAt int64
}

// quota is one account's freshest live meter reading (both windows).
type quota struct {
	five  RateLimit
	seven RateLimit
	ok    bool
	at    time.Time
}

// QuotaSample is one persisted reading of both buckets at a wall instant — the
// burn-rate estimator's only time-series input. *Pct are used% 0-100; Ts and
// the *Reset fields are unix seconds.
type QuotaSample struct {
	Account    string  `json:"account"`
	Ts         int64   `json:"ts"`
	FivePct    float64 `json:"five_pct"`
	FiveReset  int64   `json:"five_reset"`
	SevenPct   float64 `json:"seven_pct"`
	SevenReset int64   `json:"seven_reset"`
}

// Governor estimator/controller numerics (fixed — describe the control law, not
// policy). Ported from the old orchid governor.
const (
	govMinSamples     = 3                // min kept samples to estimate a slope
	govMinSpan        = 10 * time.Minute // and min wall span
	govEpsilon        = 5 * time.Minute  // near-reset clamp on remaining time
	govMinRate        = 0.05             // %/h divide-guard
	govCapDeadband    = 0.15             // |normErr| within this => hold cap (braking hysteresis)
	govSlewPerTick    = 1                // cap moves at most this many slots per decide
	govEngageFloorPct = 10.0             // below this used%, the soft governor relaxes (signal too noisy)
	govSampleRetain   = 6 * time.Hour    // trim persisted samples older than this
)

func govHours(d time.Duration) float64 { return float64(d) / float64(time.Hour) }

// accountKey normalizes a job/target agent to its pacing account.
func accountKey(agent string) string {
	if agent == "" {
		return "claude"
	}
	return agent
}

// accounts returns the distinct pacing accounts across all targets, including
// every agent reachable via overflow — so a spill-only account (e.g. codex in a
// claude target's overflow list) still gets its own meter + cap.
func (c *Config) accounts() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range c.Targets {
		for _, a := range t.agentList() {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	if len(out) == 0 {
		out = []string{"claude"}
	}
	return out
}

// claudeQuotaScript prints "<mtime> <json-line>" for the host's newest claude
// statusline event (the statusLine hook tees full payloads — incl. rate_limits
// — to ~/.claude/statusline.jsonl). Empty when no statusline feed exists here.
func (h Host) claudeQuotaScript() string {
	home := h.agentHome()
	return fmt.Sprintf(`export HOME=%s
f="$HOME/.claude/statusline.jsonl"
[ -f "$f" ] || exit 0
m=$(date -r "$f" +%%s 2>/dev/null || stat -c %%Y "$f" 2>/dev/null || echo 0)
printf '%%s ' "$m"; tail -n 1 "$f"`, shq(home))
}

// codexQuotaScript prints "<mtime> <json-line>" for the newest token_count event
// in the host's most-recent codex rollout — codex's account meter lives in
// payload.rate_limits.{primary,secondary} of those events.
func (h Host) codexQuotaScript() string {
	home := h.agentHome()
	return fmt.Sprintf(`export HOME=%s
d="$HOME/.codex/sessions"
[ -d "$d" ] || exit 0
f=$(find "$d" -type f -name '*.jsonl' -printf '%%T@ %%p\n' 2>/dev/null | sort -rn | head -1 | cut -d' ' -f2-)
[ -n "$f" ] || exit 0
m=$(date -r "$f" +%%s 2>/dev/null || stat -c %%Y "$f" 2>/dev/null || echo 0)
l=$(awk '/token_count/ && /rate_limits/{x=$0} END{if(x)print x}' "$f")
[ -n "$l" ] || exit 0
printf '%%s ' "$m"; printf '%%s' "$l"`, shq(home))
}

// parseHostQuota reads one host's "<mtime> <json>" line for an account into a
// reading (ok=false when blank/unparseable/empty meter). mtime is the recency
// key used to pick the freshest host when an account spans several.
func parseHostQuota(account, out string) (five, seven RateLimit, mtime int64, ok bool) {
	out = strings.TrimSpace(out)
	if out == "" {
		return
	}
	sp := strings.IndexByte(out, ' ')
	if sp <= 0 {
		return
	}
	mtime, _ = strconv.ParseInt(out[:sp], 10, 64)
	line := strings.TrimSpace(out[sp+1:])
	if account == "codex" {
		var e struct {
			Payload struct {
				Type       string `json:"type"`
				RateLimits *struct {
					Primary *struct {
						UsedPercent   float64 `json:"used_percent"`
						WindowMinutes int     `json:"window_minutes"`
						ResetsAt      int64   `json:"resets_at"`
					} `json:"primary"`
					Secondary *struct {
						UsedPercent   float64 `json:"used_percent"`
						WindowMinutes int     `json:"window_minutes"`
						ResetsAt      int64   `json:"resets_at"`
					} `json:"secondary"`
				} `json:"rate_limits"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Payload.RateLimits == nil {
			return
		}
		rl := e.Payload.RateLimits
		if w := rl.Primary; w != nil && w.ResetsAt != 0 {
			r := RateLimit{UsedPct: w.UsedPercent, ResetsAt: w.ResetsAt}
			if w.WindowMinutes <= 600 {
				five = r
			} else {
				seven = r
			}
		}
		if w := rl.Secondary; w != nil && w.ResetsAt != 0 {
			r := RateLimit{UsedPct: w.UsedPercent, ResetsAt: w.ResetsAt}
			if w.WindowMinutes <= 600 {
				five = r
			} else {
				seven = r
			}
		}
	} else {
		var e struct {
			RateLimits struct {
				FiveHour struct {
					UsedPct  float64 `json:"used_percentage"`
					ResetsAt int64   `json:"resets_at"`
				} `json:"five_hour"`
				SevenDay struct {
					UsedPct  float64 `json:"used_percentage"`
					ResetsAt int64   `json:"resets_at"`
				} `json:"seven_day"`
			} `json:"rate_limits"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			return
		}
		five = RateLimit{UsedPct: e.RateLimits.FiveHour.UsedPct, ResetsAt: e.RateLimits.FiveHour.ResetsAt}
		seven = RateLimit{UsedPct: e.RateLimits.SevenDay.UsedPct, ResetsAt: e.RateLimits.SevenDay.ResetsAt}
	}
	if five.ResetsAt == 0 && seven.ResetsAt == 0 {
		return // empty meter
	}
	ok = true
	return
}

// sampleQuota reads each account's REAL rate-limit meter off the fleet. Rate
// limits are account-global (the fleet shares one oauth per agent), so for each
// account it takes the freshest reading across hosts. claude reads the
// statusline tee; codex reads the rollout token_count. No clawpatrol, no API
// key, no settings.json mutation — pure reads of the agents' own on-disk data.
func (c *Coord) sampleQuota(ctx context.Context) map[string]quota {
	out := map[string]quota{}
	best := map[string]int64{} // account => freshest mtime seen
	for _, acct := range c.cfg.accounts() {
		for _, h := range c.cfg.Hosts {
			var script string
			if acct == "codex" {
				script = h.codexQuotaScript()
			} else {
				script = h.claudeQuotaScript()
			}
			cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
			raw, err := h.runRemote(cctx, script)
			cancel()
			if err != nil {
				continue
			}
			five, seven, mtime, ok := parseHostQuota(acct, raw)
			if !ok {
				continue
			}
			if cur, seen := best[acct]; !seen || mtime >= cur {
				best[acct] = mtime
				out[acct] = quota{five: five, seven: seven, ok: true, at: time.Now()}
			}
		}
	}
	return out
}

// govDecision is the per-account verdict for one tick.
type govDecision struct {
	cap          int
	binding      string // "weekly" | "5h" | ""
	overPace     bool
	burnWeekly   float64
	targetWeekly float64
	burnFive     float64
	targetFive   float64
	projectedEnd float64 // projected end-of-week used% at current weekly burn
}

// burnRatePerHour estimates one bucket's consumption rate (used% per hour) from
// a sample window. Returns ok=false (=> that bucket adds no constraint) on
// thin/degenerate data. Reset-equality drops pre-rollover points; a robust
// Theil-Sen median slope rejects single outliers. Ported from old orchid.
func burnRatePerHour(samples []QuotaSample, now time.Time, window time.Duration,
	pct func(QuotaSample) float64, reset func(QuotaSample) int64, curReset int64) (rate float64, ok bool) {

	if curReset == 0 || window <= 0 {
		return 0, false
	}
	cutoff := now.Unix() - int64(window/time.Second)
	type pt struct{ t, pct float64 }
	var kept []pt
	for _, s := range samples {
		if s.Ts < cutoff || s.Ts > now.Unix() || reset(s) != curReset {
			continue
		}
		kept = append(kept, pt{t: float64(s.Ts) / 3600.0, pct: pct(s)})
	}
	if len(kept) < govMinSamples {
		return 0, false
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].t < kept[j].t })
	// Keep only the most-recent monotone-non-decreasing segment.
	start := 0
	for i := len(kept) - 1; i > 0; i-- {
		if kept[i-1].pct > kept[i].pct {
			start = i
			break
		}
	}
	seg := kept[start:]
	if len(seg) < govMinSamples {
		return 0, false
	}
	span := seg[len(seg)-1].t - seg[0].t
	if span < govHours(govMinSpan) {
		return 0, false
	}
	var slopes []float64
	for i := 0; i < len(seg); i++ {
		for j := i + 1; j < len(seg); j++ {
			if dt := seg[j].t - seg[i].t; dt > 0 {
				slopes = append(slopes, (seg[j].pct-seg[i].pct)/dt)
			}
		}
	}
	if len(slopes) < 3 {
		rate = (seg[len(seg)-1].pct - seg[0].pct) / span
	} else {
		sort.Float64s(slopes)
		m := len(slopes)
		if m%2 == 1 {
			rate = slopes[m/2]
		} else {
			rate = (slopes[m/2-1] + slopes[m/2]) / 2
		}
	}
	if rate < 0 {
		rate = 0
	}
	return rate, true
}

// targetRatePerHour is the burn rate (%/h) that lands EXACTLY on ceilingPct at
// reset, from the CURRENT used% and remaining time. Recomputing each tick folds
// position error into the setpoint (integral-like, no wind-up).
func targetRatePerHour(now time.Time, curReset int64, usedNow, ceilingPct float64) float64 {
	remaining := time.Unix(curReset, 0).Sub(now)
	if remaining <= govEpsilon {
		remaining = govEpsilon
	}
	budget := ceilingPct - usedNow
	if budget < 0 {
		budget = 0
	}
	return budget / govHours(remaining)
}

// controlBucketCap is the cap control law (§1c) for one bucket: asymmetric
// deadband + ±slew. Growth (under pace) isn't suppressed so the cap recovers
// real headroom; braking keeps a deadband as anti-noise hysteresis.
func controlBucketCap(burn, target float64, active, prevCap, minActive, maxActive int) int {
	desiredRaw := float64(active) * (target / math.Max(burn, govMinRate))
	normErr := (burn - target) / math.Max(target, govMinRate)
	lo, hi := float64(prevCap-govSlewPerTick), float64(prevCap+govSlewPerTick)
	var newCap float64
	switch {
	case normErr < 0, normErr > govCapDeadband:
		newCap = math.Max(lo, math.Min(hi, desiredRaw))
	default:
		newCap = float64(prevCap)
	}
	c := int(math.Round(newCap))
	if c < minActive {
		c = minActive
	}
	if c > maxActive {
		c = maxActive
	}
	return c
}

// decide runs the per-account burn-rate controller. It fails open (cap =
// MaxActive) when the governor is off, the meter is unread, or both buckets are
// thin-data — so a missing/early meter never starves an account. A hard gate
// pauses (cap 0) at/above the ceiling as the binary safety floor.
func (g Gov) decide(now time.Time, q quota, samples []QuotaSample, active, prevCap int) govDecision {
	if !g.Enabled || !q.ok {
		return govDecision{cap: g.MaxActive}
	}
	// Hard safety floor: at/above the ceiling on either window, pause new work.
	if q.seven.ResetsAt != 0 && q.seven.UsedPct >= g.WeeklyCeiling {
		return govDecision{cap: 0, binding: "weekly", overPace: true, projectedEnd: q.seven.UsedPct}
	}
	if q.five.ResetsAt != 0 && q.five.UsedPct >= g.WeeklyCeiling {
		return govDecision{cap: 0, binding: "5h", overPace: true, projectedEnd: q.seven.UsedPct}
	}
	if prevCap <= 0 || prevCap >= g.MaxActive {
		prevCap = g.MaxActive // sane slew anchor on first tick / restart
	}
	min, max, ceil := g.MinActive, g.MaxActive, g.WeeklyCeiling

	d := govDecision{cap: math.MaxInt, projectedEnd: q.seven.UsedPct}
	consider := func(window string, rl RateLimit, w time.Duration, pct func(QuotaSample) float64, reset func(QuotaSample) int64) (burn, target float64, used bool) {
		if rl.ResetsAt == 0 {
			return
		}
		inBand := rl.UsedPct >= ceil-g.Slack // within slack of the ceiling
		burn, ok := burnRatePerHour(samples, now, w, pct, reset, rl.ResetsAt)
		// Act when we have a usable burn estimate OR we're already in the slack
		// band (the static floor below protects a hot window immediately, before
		// enough samples accumulate to estimate burn — e.g. right after start).
		if !ok && !inBand {
			return
		}
		cap := max
		if ok {
			target = targetRatePerHour(now, rl.ResetsAt, rl.UsedPct, ceil)
			if !relaxBucket(rl.UsedPct) {
				cap = controlBucketCap(burn, target, active, prevCap, min, max)
			}
		}
		// Static slack-band floor (the old binary gate): inside the band, hold the
		// cap at MinActive regardless of burn confidence.
		if inBand && cap > min {
			cap = min
		}
		if cap < d.cap {
			d.cap, d.binding = cap, window
		}
		return burn, target, ok
	}

	if b, t, used := consider("weekly", q.seven, g.rateWindowDur(),
		func(s QuotaSample) float64 { return s.SevenPct },
		func(s QuotaSample) int64 { return s.SevenReset }); used {
		d.burnWeekly, d.targetWeekly = b, t
		rem := time.Unix(q.seven.ResetsAt, 0).Sub(now)
		if rem < 0 {
			rem = 0
		}
		d.projectedEnd = q.seven.UsedPct + b*govHours(rem)
	}
	if b, t, used := consider("5h", q.five, g.fiveRateWindowDur(),
		func(s QuotaSample) float64 { return s.FivePct },
		func(s QuotaSample) int64 { return s.FiveReset }); used {
		d.burnFive, d.targetFive = b, t
	}

	if d.cap == math.MaxInt {
		return govDecision{cap: g.MaxActive, projectedEnd: q.seven.UsedPct} // both buckets thin => fail open
	}
	switch d.binding {
	case "weekly":
		d.overPace = d.burnWeekly > d.targetWeekly
	case "5h":
		d.overPace = d.burnFive > d.targetFive
	}
	return d
}

// relaxBucket: below the engage floor the proactive governor reads quantization
// noise as burn, so leave the cap uncapped — the hard gate still backstops the
// real ceiling and the fast 5h bucket engages on any genuine burst.
func relaxBucket(used float64) bool { return used < govEngageFloorPct }

// ============================ coordinator ============================

type Coord struct {
	cfg   *Config
	st    *State
	auth  *AuthStore
	hosts map[string]Host
	dry   bool // dry-run: log spawn/adopt decisions, take no spawning action
	gov   struct {
		mu sync.Mutex
		q  map[string]quota // freshest live meter reading per account
	}
}

// adopt recognizes a live herdr agent for issue n (migration / restart safety)
// and records it WITHOUT spawning — so a cutover or restart supervises existing
// sessions instead of duplicating them.
func (c *Coord) adopt(n int, is Issue, status map[int]agentRef) (*Job, bool) {
	tgt, ok := c.targetFor(is)
	if !ok {
		return nil, false
	}
	ref, found := status[n]
	if !found {
		return nil, false
	}
	agent := ref.Agent
	if agent == "" {
		agent = "claude"
	}
	j := &Job{
		Issue: n, Host: ref.Host, Label: agent + "-" + strconv.Itoa(n),
		Pane: ref.Pane, Workspace: ref.Workspace,
		Target: tgt.Label, Repo: tgt.Repo, Branch: fmt.Sprintf("%s%d", c.cfg.BranchPrefix, n),
		Agent: agent, Title: is.Title, Goal: truncate(is.Body, 1500), SpawnedAt: time.Now(),
	}
	c.st.mu.Lock()
	c.st.Jobs[n] = j
	c.st.mu.Unlock()
	log.Printf("issue #%d: ADOPTED live %s on %s (pane %s)", n, j.Label, ref.Host, ref.Pane)
	return j, true
}

func newCoord(cfg *Config) *Coord {
	hosts := map[string]Host{}
	for _, h := range cfg.Hosts {
		hosts[h.Name] = h
	}
	return &Coord{cfg: cfg, st: loadState(cfg.StateFile), auth: defaultAuth(), hosts: hosts}
}

func (c *Coord) run(ctx context.Context) {
	go c.authSyncLoop(ctx)
	go c.governorLoop(ctx)
	go c.memoryLoop(ctx)
	iv := durOr(c.cfg.PollInterval, 30*time.Second)
	t := time.NewTicker(iv)
	defer t.Stop()
	c.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			c.st.save()
			return
		case <-t.C:
			c.tick(ctx)
		}
	}
}

// authSyncLoop keeps every active host's creds fresh before the ~hourly oauth
// access-token expiry.
func (c *Coord) authSyncLoop(ctx context.Context) {
	t := time.NewTicker(20 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.auth.refreshLocal(ctx)
			c.refreshCanonicalFromHosts(ctx)
			for _, h := range c.activeHosts() {
				if err := c.auth.syncToHost(ctx, h); err != nil {
					log.Printf("authsync: %v", err)
				}
			}
		}
	}
}

// refreshCanonicalFromHosts adopts the freshest self-refreshed claude creds
// across the fleet as the new canonical, so the coordinator's copy tracks the
// latest rotation (for seeding stale/new hosts). Combined with the refresh-safe
// gate in syncToHost, this makes oauth refresh fully decentralized: each host
// self-refreshes, the freshest becomes canonical, and stale hosts get bootstrapped
// — no single point that can clobber the rotation chain.
func (c *Coord) refreshCanonicalFromHosts(ctx context.Context) {
	c.auth.mu.Lock()
	canon := c.auth.ClaudeCreds
	c.auth.mu.Unlock()
	best := credExpiryFile(canon)
	var bestHost *Host
	for _, h := range c.activeHosts() {
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		e := h.credExpiryRemote(hctx)
		cancel()
		if e > best {
			best = e
			hh := h
			bestHost = &hh
		}
	}
	if bestHost == nil {
		return
	}
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := bestHost.scpFrom(fctx, bestHost.agentHome()+"/.claude/.credentials.json", canon); err != nil {
		log.Printf("refreshCanonical from %s: %v", bestHost.Name, err)
		return
	}
	log.Printf("refreshCanonical: adopted %s creds (expiry %s)", bestHost.Name, time.UnixMilli(best).UTC().Format("15:04"))
}

// memoryLoop syncs shared memory SERIALLY across hosts (one committer at a time
// → no push race; union merge → no override). Scoped to the memory subtree.
func (c *Coord) memoryLoop(ctx context.Context) {
	if !c.cfg.Memory.Enabled {
		return
	}
	iv := durOr(c.cfg.Memory.Interval, 5*time.Minute)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, h := range c.cfg.Hosts {
				cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
				if err := h.syncMemory(cctx, c.cfg.Memory, c.cfg.BotLogin, c.cfg.BotEmail); err != nil {
					log.Printf("memory: %v", err)
				}
				cancel()
			}
		}
	}
}

func (c *Coord) governorLoop(ctx context.Context) {
	if !c.cfg.Governor.Enabled {
		return
	}
	t := time.NewTicker(c.cfg.Governor.sampleIntervalDur())
	defer t.Stop()
	for {
		qs := c.sampleQuota(ctx)
		now := time.Now()
		if len(qs) > 0 {
			c.gov.mu.Lock()
			c.gov.q = qs
			c.gov.mu.Unlock()
			// Append fresh readings to each account's burn-rate ring + trim.
			c.st.mu.Lock()
			if c.st.QuotaSamples == nil {
				c.st.QuotaSamples = map[string][]QuotaSample{}
			}
			for a, q := range qs {
				if !q.ok {
					continue
				}
				ring := append(c.st.QuotaSamples[a], QuotaSample{
					Account: a, Ts: now.Unix(),
					FivePct: q.five.UsedPct, FiveReset: q.five.ResetsAt,
					SevenPct: q.seven.UsedPct, SevenReset: q.seven.ResetsAt,
				})
				cutoff := now.Add(-govSampleRetain).Unix()
				trimmed := ring[:0]
				for _, s := range ring {
					if s.Ts >= cutoff {
						trimmed = append(trimmed, s)
					}
				}
				c.st.QuotaSamples[a] = trimmed
			}
			samples := c.st.QuotaSamples
			active := map[string]int{}
			for _, j := range c.st.Jobs {
				active[accountKey(j.Agent)]++
			}
			prev := map[string]int{}
			for a, v := range c.st.PrevCap {
				prev[a] = v
			}
			c.st.mu.Unlock()
			for a, q := range qs {
				if !q.ok {
					continue
				}
				d := c.cfg.Governor.decide(now, q, samples[a], active[a], prev[a])
				log.Printf("governor[%s]: weekly %.0f%% (burn %.2f/h, target %.2f/h) / 5h %.0f%% → cap %d active %d (binding %q, projEnd %.0f%%)",
					a, q.seven.UsedPct, d.burnWeekly, d.targetWeekly, q.five.UsedPct, d.cap, active[a], d.binding, d.projectedEnd)
			}
			c.st.save()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// curCaps returns the adaptive admission cap per account and advances each
// account's PrevCap slew anchor. Called once per poll tick.
func (c *Coord) curCaps() map[string]int {
	now := time.Now()
	c.gov.mu.Lock()
	qs := c.gov.q
	c.gov.mu.Unlock()
	caps := map[string]int{}
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	if c.st.PrevCap == nil {
		c.st.PrevCap = map[string]int{}
	}
	active := map[string]int{}
	for _, j := range c.st.Jobs {
		active[accountKey(j.Agent)]++
	}
	for _, a := range c.cfg.accounts() {
		d := c.cfg.Governor.decide(now, qs[a], c.st.QuotaSamples[a], active[a], c.st.PrevCap[a])
		caps[a] = d.cap
		c.st.PrevCap[a] = d.cap
	}
	return caps
}

func (c *Coord) activeHosts() []Host {
	c.st.mu.Lock()
	defer c.st.mu.Unlock()
	seen := map[string]bool{}
	var out []Host
	for _, j := range c.st.Jobs {
		if h, ok := c.hosts[j.Host]; ok && !seen[j.Host] {
			seen[j.Host] = true
			out = append(out, h)
		}
	}
	return out
}

func (c *Coord) tick(ctx context.Context) {
	// Feeder first: mirror any newly bot-assigned upstream issues into the
	// inbox so they're visible to pollIssues on this same cycle.
	c.assignmentTick(ctx)

	open, allOpen, pollOK := c.pollIssues(ctx)

	// Teardown jobs whose inbox issue is gone — but ONLY when the poll was
	// reliable. A flaky gh list (error, or a transient empty result) must never
	// be read as "all issues closed" and wipe the swarm. Fail-safe: skip teardown
	// unless every target listed cleanly AND we got a non-empty open set.
	if pollOK && len(allOpen) > 0 {
		c.st.mu.Lock()
		for n, j := range c.st.Jobs {
			if _, ok := allOpen[n]; !ok {
				jc := j
				c.st.mu.Unlock()
				// Before tearing down a merged-partial job, give its still-alive worker
				// ONE grace tick to fan out the remaining work into sibling inbox issues
				// (parallel sessions). fanoutGrace nudges the pane and returns true to
				// defer teardown; next tick it returns false and teardown proceeds.
				if c.fanoutGrace(ctx, n, jc) {
					c.st.mu.Lock()
					continue
				}
				c.teardown(ctx, n, jc)
				c.st.mu.Lock()
				delete(c.st.Jobs, n)
			}
		}
		c.st.mu.Unlock()
	} else if !pollOK {
		log.Printf("poll incomplete — skipping teardown pass (never-strand guard)")
	}

	status, up := c.fleetStatus(ctx)

	// Respawn dead sessions: a tracked job whose host responded but whose agent
	// is gone (issue still open) is stalled — drop it so it re-spawns fresh.
	type deadJob struct {
		n    int
		host string
		ws   string
	}
	var dead []deadJob
	c.st.mu.Lock()
	for n, j := range c.st.Jobs {
		if _, alive := status[n]; alive {
			continue
		}
		if !up[j.Host] {
			continue // host unreachable — transient, don't respawn
		}
		if _, stillOpen := allOpen[n]; !stillOpen {
			continue // handled by the teardown pass
		}
		if time.Since(j.SpawnedAt) < 3*time.Minute {
			continue // freshly spawned/adopted — give herdr time to register the agent
		}
		dead = append(dead, deadJob{n: n, host: j.Host, ws: j.Workspace})
		delete(c.st.Jobs, n)
	}
	c.st.mu.Unlock()
	// Close each dead job's workspace before it respawns. Without this every
	// flap-induced respawn leaks an orphan "unknown" workspace in herdr (the
	// agent already exited, but the empty pane lingers forever). Done outside
	// the state lock since closeWorkspace is a network round-trip.
	for _, d := range dead {
		log.Printf("issue #%d: agent gone on %s (host up) — dropping for respawn", d.n, d.host)
		if d.ws == "" {
			continue
		}
		host, ok := c.hosts[d.host]
		if !ok {
			continue
		}
		cctx, ccancel := context.WithTimeout(ctx, 12*time.Second)
		if err := host.closeWorkspace(cctx, d.ws); err != nil {
			log.Printf("issue #%d: closing dead workspace %s failed: %v", d.n, d.ws, err)
		}
		ccancel()
	}

	// Admission budget = per-account governor cap − that account's running count.
	// claude and codex pace independently against their own real meters, so a hot
	// claude window never throttles codex (and vice-versa) — proper agent routing.
	caps := c.curCaps()
	c.st.mu.Lock()
	running := map[string]int{}
	for _, j := range c.st.Jobs {
		running[accountKey(j.Agent)]++
	}
	c.st.mu.Unlock()
	budget := map[string]int{}
	for a, cp := range caps {
		budget[a] = cp - running[a]
	}

	for n, is := range open {
		c.st.mu.Lock()
		j, live := c.st.Jobs[n]
		c.st.mu.Unlock()
		if live {
			c.supervise(ctx, n, j, status)
			continue
		}
		// Adopt a live session before spawning (cutover / restart migration).
		if j2, ok := c.adopt(n, is, status); ok {
			c.supervise(ctx, n, j2, status)
			continue
		}
		tgt, ok := c.targetFor(is)
		if !ok {
			continue
		}
		// Pick the first agent in the target's overflow preference with budget —
		// claude work spills to codex automatically when claude is throttled.
		acct, ok := pickAgent(tgt, budget)
		if !ok {
			continue // every candidate account exhausted this tick
		}
		if c.dry {
			log.Printf("issue #%d: would spawn %s (dry-run)", n, acct)
			budget[acct]--
			continue
		}
		if c.spawn(ctx, n, is, acct) {
			budget[acct]--
		}
	}
	c.st.save()
}

// pollIssues returns the open issues plus ok=false if ANY target's gh list
// errored — so the caller can skip teardown on a flaky poll and never wipe the
// swarm over a transient gh hiccup.
func (c *Coord) pollIssues(ctx context.Context) (open map[int]Issue, all map[int]bool, ok bool) {
	open = map[int]Issue{}
	all = map[int]bool{}
	ok = true
	for _, tgt := range c.cfg.Targets {
		issues, err := ghIssues(ctx, c.cfg.Inbox, tgt.Label)
		if err != nil {
			log.Printf("poll %s/%s: %v", c.cfg.Inbox, tgt.Label, err)
			ok = false
			continue
		}
		for _, is := range issues {
			open[is.Number] = is
			all[is.Number] = true
		}
	}
	return open, all, ok
}

// agentRef is a live herdr agent resolved to an issue number, with the handles
// needed to drive it (pane for send/read, workspace for close).
type agentRef struct {
	Host      string
	Agent     string // claude | codex
	Status    string // idle|working|blocked|done|unknown
	Pane      string // send/read target
	Workspace string // teardown handle
}

var issueCwdRe = regexp.MustCompile(`issue-(\d+)`)

// issueFromCwd extracts the issue number from a worktree cwd like
// ".../orch-work/issue-590" (or a memory path). 0 if none.
func issueFromCwd(cwd string) int {
	m := issueCwdRe.FindStringSubmatch(cwd)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// fleetStatus reads agent_status from every host once and keys by ISSUE number
// (from the agent's cwd) — the reliable adoption key, since agent objects don't
// carry the workspace label.
// fleetStatus returns the live agents keyed by issue, plus the set of hosts that
// responded (so the caller can tell "agent died" from "host unreachable" and
// only respawn in the former case).
func (c *Coord) fleetStatus(ctx context.Context) (map[int]agentRef, map[string]bool) {
	out := map[int]agentRef{}
	up := map[string]bool{}
	for name, h := range c.hosts {
		cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		agents, err := h.agentList(cctx)
		cancel()
		if err != nil {
			log.Printf("fleetStatus %s: %v", name, err)
			continue
		}
		up[name] = true
		for _, a := range agents {
			n := issueFromCwd(a.Cwd)
			if n == 0 {
				continue
			}
			out[n] = agentRef{Host: name, Agent: a.Agent, Status: a.AgentStatus, Pane: a.PaneID, Workspace: a.WorkspaceID}
		}
	}
	return out, up
}

func (c *Coord) targetFor(is Issue) (Target, bool) {
	for _, tgt := range c.cfg.Targets {
		if contains(is.Labels, tgt.Label) {
			return tgt, true
		}
	}
	return Target{}, false
}

// agentList is the target's ordered agent-overflow preference (normalized to
// accounts). Falls back to the single Agent (default claude) when unset.
func (t Target) agentList() []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range t.Agents {
		a = accountKey(a)
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		out = []string{accountKey(t.Agent)}
	}
	return out
}

// agentForIssue is the issue target's PRIMARY (first-choice) agent — used for
// reporting; admission uses pickAgent to honor overflow + live budget.
func (c *Coord) agentForIssue(is Issue) string {
	if t, ok := c.targetFor(is); ok {
		return t.agentList()[0]
	}
	return "claude"
}

// pickAgent returns the first agent in the target's overflow preference whose
// account still has admission budget, decrementing that account's budget. ok =
// false when every candidate account is exhausted (issue waits this tick).
func pickAgent(t Target, budget map[string]int) (string, bool) {
	for _, a := range t.agentList() {
		if budget[a] > 0 {
			return a, true
		}
	}
	return "", false
}

// pickHost returns the least-loaded capable host with free capacity.
func (c *Coord) pickHost(tgt Target) (Host, bool) {
	c.st.mu.Lock()
	load := map[string]int{}
	for _, j := range c.st.Jobs {
		load[j.Host]++
	}
	c.st.mu.Unlock()
	best, bestFree := "", 0
	for name, h := range c.hosts {
		if tgt.NeedCap != "" && !h.has(tgt.NeedCap) {
			continue
		}
		if free := h.Capacity - load[name]; free > bestFree {
			best, bestFree = name, free
		}
	}
	if best == "" {
		return Host{}, false
	}
	return c.hosts[best], true
}

// workerPrompt is the full goal injected into every worker — restored from the
// old orchid production bootstrap_prompt. Tokens: {{issue.number}} {{inbox.repo}}
// {{issue.title}} {{target.repo}} {{workdir}} {{branch}} {{issue.body}}.
const workerPrompt = `You are implementing GitHub issue #{{issue.number}} from {{inbox.repo}}: "{{issue.title}}"

Work repo: {{target.repo}}
Clone: {{workdir}} (this is the worktree; you are already in it, on branch {{branch}})
git + gh are authenticated via GH_TOKEN.

--- issue body ---
{{issue.body}}
--- end issue body ---

## Pre-flight — read the triage report

Run: gh issue view {{issue.number}} --repo {{inbox.repo}} --comments
If a comment starting with "orchid-triage" exists, read it: it lists existing
PRs that may already cover this work, duplicate inbox issues, and starting
pointers. If an open or merged PR already covers the change, VERIFY that
yourself; if confirmed, comment your finding on the inbox issue and stop
instead of duplicating the work.

## Memory — check it FIRST

Past sessions on this repo left notes in your memory (build/test recipes,
environment quirks, maintainer preferences, dead-ends). Before you start reading
the codebase or building, CONSULT YOUR MEMORY — do not re-derive what is already
known there. It is the single biggest way to avoid wasting the session
re-discovering the build command or the right test invocation.

WRITE MEMORY LIBERALLY AND OFTEN — it is part of the job, not an afterthought.
Every time you discover something a future session would otherwise have to
re-derive, save a note IMMEDIATELY (do not batch it to the end of the session):
the exact build / test / lint command that worked, where a subsystem or file
lives, a maintainer's stated preference, a CI quirk, a dead-end that wasted your
time, an API gotcha, the shape of the fix you made. Prefer MANY small, specific,
well-named notes over one big one. A session that ships a PR but leaves no new
memory has under-delivered — aim to leave several notes behind every time, and
update existing notes when you find them stale or incomplete.

## Your job

You are running FULLY AUTONOMOUSLY — no human is watching this session. Never
ask the user a question and never open an interactive prompt or plan-mode menu
(AskUserQuestion / ExitPlanMode): there is nobody to answer, so it strands the
session. When you hit a fork or an ambiguous decision, pick the best option
yourself from the issue's goal and proceed. Ship your judgment in the PR — the
human reviews it there, not mid-session.

Implement this fully. Read the codebase, understand it deeply, make the change.
Large refactors are expected — do not avoid them. If the right fix touches 10
files, touch 10 files. If it requires redesigning a data structure, redesign it.

Do NOT stop early. Do NOT mark anything done without shipping a PR. The only
acceptable outcome is a merged PR or an open PR awaiting review.

If something is hard, work through it. Read more code, try a different approach,
break the problem into smaller pieces — but keep going. Giving up and exiting
without a PR wastes the entire session.

If you get blocked on one approach, try another. Partial implementations that
compile and pass tests are better than nothing — ship what you have and note
what remains in the PR description.

## Commit attribution

When committing, your commit message MUST end with exactly this single
co-author footer and NO other co-author or attribution lines:

  Co-Authored-By: Divy Srivastava <me@littledivy.com>

Do NOT add ` + "`Co-Authored-By: Claude …`" + `, ` + "`Co-Authored-By: Anthropic …`" + `,
` + "`Generated with Claude Code`" + `, or any other AI/tool attribution. The
Divy co-author footer is the only attribution Divy wants on commits in
his repos.

## Open a DRAFT PR early

As soon as you have a meaningful first commit, push and open a DRAFT PR
(add --draft to gh pr create) so progress is visible immediately, then keep
pushing to it. When the work is complete and CI is green, mark it ready with:
gh pr ready <num> --repo {{target.repo}}
Never leave a finished PR in draft.

## When done

Commit, push to ` + "`{{branch}}`" + `, then:

    gh pr create --repo {{target.repo}} \
      --title "..." \
      --body "<summary of the change>

    Closes {{inbox.repo}}#{{issue.number}}"

PR TITLE — use conventional-commit format: ` + "`type(scope): summary`" + ` (e.g.
` + "`fix(node): ...`, `feat(ext/fetch): ...`" + `). Many repos run a "lint title" CI check
that FAILS (and blocks "ci status") on anything else — including a bracketed
issue title. If a PR already exists with a non-conventional title, RENAME it
yourself: gh pr edit <num> --repo {{target.repo}} --title "type(scope): summary".
The title is NOT orchestrator-owned — fixing it is your job.

REQUIRED: the PR body MUST end with this exact line, on its own line:

  Closes {{inbox.repo}}#{{issue.number}}

Do not omit it, do not paraphrase it, and do not change the issue number — it is
how the PR is tied back to the originating issue. If you already opened the PR
without it, edit the body now with gh pr edit --repo {{target.repo}} <pr> --body ...
to add it. (Cross-repo closes don't auto-link, the orchestrator handles teardown —
but the line is still required on every PR.)

REFERENCE the real upstream issue too. Your task title may begin with a
"[owner/repo#N]" tag naming the actual issue you were assigned (the inbox issue
is only a tracking stub). If that repo is {{target.repo}} — this PR's repo —
add a line ABOVE the inbox Closes line referencing issue N. Use YOUR JUDGMENT:
write "Closes #N" ONLY if this PR fully resolves that issue; if it is a partial
fix, one of several PRs, or merely related, write "Refs #N" instead so it links
without auto-closing. Do NOT claim "Closes" for a partial fix. Keep the
"Closes {{inbox.repo}}#{{issue.number}}" line LAST. (If the tagged repo differs
from {{target.repo}}, a Closes keyword cannot auto-close across repos — write
"Refs owner/repo#N".)

If your fix needs a change in an upstream/dependency repo, open that PR too and
reference it in this PR's description (e.g. "Upstream: owner/repo#123").

## Fan out the remaining work (PARALLELISM)

If your PR is a PARTIAL fix ("Refs #N") and the upstream issue N has more
INDEPENDENT work left, split that remainder into discrete subtasks and open ONE
inbox issue per subtask — they become sibling sessions that run IN PARALLEL with
each other. This is how a big issue gets worked by many agents at once instead of
one-at-a-time.

For each independent remaining subtask:

    gh issue create --repo {{inbox.repo}} --label {{target.label}} \
      --title "[{{owner/repo#N}}] <short subtask>" \
      --body "Subtask of {{owner/repo#N}}. Do ONLY this piece; sibling subtasks
    are handled by other sessions — do NOT touch their files. Scope: <files/area>.

    <what to do>"

Rules:
  - Only split work that is genuinely INDEPENDENT (no ordering dependency, minimal
    file overlap). Sequential/dependent remainder → leave ONE follow-up, not many.
  - IDEMPOTENT: first run  gh issue list --repo {{inbox.repo}} --state open --search
    "[{{owner/repo#N}}] in:title"  and skip any subtask that already has an open
    stub. Never double-file.
  - Scope each stub tightly (name the files/module) so siblings don't collide.
  - If NOTHING independent remains (issue is basically done, or only a single
    dependent step is left), do NOT fan out — just write "Closes #N" (if done) or
    leave it for the orchestrator's single follow-up.
  - Open these BEFORE you stop. The orchestrator may also nudge you after merge —
    if you already filed them, reply that they exist and stop.

Then stop and wait. The orchestrator sends a follow-up when reviews, comments,
or CI results arrive. Address them, push fixes, stop again.
The session ends automatically when the PR merges or closes.`

// renderGoal fills the worker prompt template for one issue.
func renderGoal(inbox, targetRepo, label, title, body, workdir, branch string, n int) string {
	// Upstream ref ("owner/repo#N") from the "[owner/repo#N] ..." title tag, so the
	// fan-out instructions can scope sibling stubs to the real issue. Fall back to
	// the target repo + inbox number if the title carries no tag.
	ref := fmt.Sprintf("%s#%d", targetRepo, n)
	if m := inboxTitleRef.FindStringSubmatch(title); m != nil {
		ref = m[1]
	}
	return strings.NewReplacer(
		"{{issue.number}}", strconv.Itoa(n),
		"{{inbox.repo}}", inbox,
		"{{issue.title}}", title,
		"{{target.repo}}", targetRepo,
		"{{target.label}}", label,
		"{{owner/repo#N}}", ref,
		"{{workdir}}", workdir,
		"{{branch}}", branch,
		"{{issue.body}}", truncate(body, 4000),
	).Replace(workerPrompt)
}

func (c *Coord) spawn(ctx context.Context, n int, is Issue, agent string) bool {
	tgt, ok := c.targetFor(is)
	if !ok {
		return false
	}
	host, ok := c.pickHost(tgt)
	if !ok {
		return false
	}
	agent = accountKey(agent)

	// 1. Push fresh creds before the agent starts.
	actx, acancel := context.WithTimeout(ctx, 30*time.Second)
	if err := c.auth.syncToHost(actx, host); err != nil {
		acancel()
		log.Printf("issue #%d: authsync to %s failed, deferring: %v", n, host.Name, err)
		return false
	}
	acancel()

	branch := fmt.Sprintf("%s%d", c.cfg.BranchPrefix, n)
	root := host.WorkdirRoot
	if root == "" {
		root = "/root/orch-work"
	}
	workdir := fmt.Sprintf("%s/issue-%d", strings.TrimRight(root, "/"), n)
	label := fmt.Sprintf("%s-%d", agent, n)

	// 2. Worktree (clone + branch) before the agent runs.
	pctx, pcancel := context.WithTimeout(ctx, 120*time.Second)
	prep := fmt.Sprintf(`set -e
mkdir -p %s; cd %s
if [ ! -d .git ]; then find . -mindepth 1 -maxdepth 1 -exec rm -rf {} + 2>/dev/null || true; git clone --depth=1 https://github.com/%s . ; fi
git fetch --depth=1 origin %s >/dev/null 2>&1 || true
git checkout -fB %s 2>/dev/null || { git reset --hard >/dev/null 2>&1 || true; git clean -fdx >/dev/null 2>&1 || true; git checkout -B %s; }`,
		shq(workdir), shq(workdir), tgt.Repo, shq(branch), shq(branch), shq(branch))
	if out, err := host.runRemote(pctx, prep); err != nil {
		pcancel()
		log.Printf("issue #%d: worktree prep on %s failed: %v: %.120q", n, host.Name, err, out)
		return false
	}
	pcancel()

	// Shared memory: ensure the clone + union driver + autoMemoryDirectory are in
	// place BEFORE claude starts, so it reads the union and writes to the clone.
	if c.cfg.Memory.Enabled {
		mctx, mcancel := context.WithTimeout(ctx, 60*time.Second)
		if err := host.ensureMemory(mctx, c.cfg.Memory, c.cfg.BotLogin, c.cfg.BotEmail); err != nil {
			log.Printf("issue #%d: memory setup on %s: %v", n, host.Name, err)
		}
		mcancel()
	}

	env := map[string]string{
		"GH_TOKEN":            c.auth.token(),
		"GITHUB_TOKEN":        c.auth.token(),
		"GIT_AUTHOR_NAME":     c.cfg.BotLogin,
		"GIT_AUTHOR_EMAIL":    c.cfg.BotEmail,
		"GIT_COMMITTER_NAME":  c.cfg.BotLogin,
		"GIT_COMMITTER_EMAIL": c.cfg.BotEmail,
		// Shared Rust compile cache across every issue dir + agent on the host.
		// Each issue clones into its own workdir with a COLD target/, so without a
		// shared cache every agent recompiles rusty_v8 + all deps from scratch
		// (~1.7G of artifacts per issue). sccache (installed on every host) caches
		// rustc objects host-wide, so a sibling's prior build warms the next.
		// Bare name resolves via the login-shell PATH; concurrency-safe (object
		// cache, not a shared target lock).
		"RUSTC_WRAPPER":      "sccache",
		"SCCACHE_DIR":        host.agentHome() + "/.cache/sccache",
		"SCCACHE_CACHE_SIZE": "30G",
	}
	// Per-target shared memory: claude-native per-process override (no settings
	// race between concurrent sessions). Points at the EXISTING per-target notes
	// in the synced clone — memory/<owner>/<repo>/ — so prior knowledge is reused.
	if c.cfg.Memory.Enabled && agent == "claude" {
		memOverride := host.memRepoDir() + "/" + c.cfg.Memory.Dir + "/" + tgt.Repo
		octx, ocancel := context.WithTimeout(ctx, 15*time.Second)
		_, _ = host.runRemote(octx, "mkdir -p "+shq(memOverride))
		ocancel()
		env["CLAUDE_COWORK_MEMORY_PATH_OVERRIDE"] = memOverride
	}
	// Launch via a login shell so PATH resolves claude/codex (~/.local/bin etc.)
	// — herdr spawns argv with a bare system PATH. exec replaces the shell so the
	// agent is the foreground process herdr's integration detects. The --env vars
	// (creds/token/git identity/memory override) survive into the login shell.
	agentCmd := "claude --dangerously-skip-permissions"
	if agent == "codex" {
		agentCmd = "codex --dangerously-bypass-approvals-and-sandbox"
	}

	// 3. Spawn BARE (no clawpatrol), one agent per dedicated single-pane workspace.
	sctx, scancel := context.WithTimeout(ctx, 40*time.Second)
	pane, ws, err := host.spawnAgent(sctx, label, workdir, env, agentCmd)
	if err != nil {
		scancel()
		log.Printf("issue #%d: spawn on %s failed: %v", n, host.Name, err)
		return false
	}
	scancel()

	// herdr's agent-start result doesn't always carry the pane id; resolve it from
	// the live fleet by cwd so the goal Enter lands on a real pane (send-keys needs
	// a pane id, not a label).
	if pane == "" {
		rc, rcancel := context.WithTimeout(ctx, 12*time.Second)
		if agents, e := host.agentList(rc); e == nil {
			for _, a := range agents {
				if issueFromCwd(a.Cwd) == n {
					pane = a.PaneID
					if ws == "" {
						ws = a.WorkspaceID
					}
					break
				}
			}
		}
		rcancel()
	}

	j := &Job{
		Issue: n, Host: host.Name, Label: label, Pane: pane, Workspace: ws,
		Target: tgt.Label, Repo: tgt.Repo,
		Branch: branch, Agent: agent, Title: is.Title, Goal: truncate(is.Body, 1500), SpawnedAt: time.Now(),
	}
	c.st.mu.Lock()
	c.st.Jobs[n] = j
	c.st.mu.Unlock()

	// 4. Inject the goal — gated on TUI readiness. A freshly-spawned claude
	// renders its prompt a few seconds after start; the old fire-and-forget
	// send landed before then and was silently dropped, leaving the worker
	// idle with no task. injectGoal waits for the prompt, then confirms the
	// send registered and retries.
	goal := renderGoal(c.cfg.Inbox, tgt.Repo, tgt.Label, is.Title, is.Body, workdir, branch, n)
	target := pane
	if target == "" {
		target = label
	}
	gctx, gcancel := context.WithTimeout(ctx, 120*time.Second)
	if err := host.injectGoal(gctx, target, goal); err != nil {
		log.Printf("issue #%d: goal inject failed: %v", n, err)
	}
	gcancel()
	log.Printf("issue #%d: spawned %s on %s/%s (branch %s)", n, agent, host.Name, label, branch)
	return true
}

func (c *Coord) supervise(ctx context.Context, n int, j *Job, status map[int]agentRef) {
	host, ok := c.hosts[j.Host]
	if !ok {
		return
	}
	ref, known := status[n]
	if known {
		// Refresh the live handles each tick (pane/workspace survive across a
		// herdr restart by re-resolving from the agent's cwd).
		j.Pane, j.Workspace = ref.Pane, ref.Workspace
	}
	if c.dry {
		log.Printf("issue #%d: supervise %s/%s status=%s pr=%d (dry-run, no action)", n, j.Host, j.Label, ref.Status, j.PR)
		return
	}

	// Auth-dead detection: resync creds + escalate, but NEVER auto-teardown — a
	// string match on pane scrollback is far too fragile to kill a live session
	// over (stale 401s sit deep in scrollback; central auth-sync prevents real
	// auth-death anyway). Read only the last few lines, and only when idle/blocked.
	if known && (ref.Status == "idle" || ref.Status == "blocked") && j.Pane != "" {
		rctx, rcancel := context.WithTimeout(ctx, 8*time.Second)
		txt := host.read(rctx, j.Pane, 4)
		rcancel()
		if strings.Contains(txt, "Invalid bearer") || strings.Contains(txt, "Please run /login") {
			log.Printf("issue #%d: possible auth-dead on %s — resyncing creds (no teardown)", n, host.Name)
			sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
			_ = c.auth.syncToHost(sctx, host)
			scancel()
			c.notify(fmt.Sprintf("issue #%d possible auth-dead on %s", n, host.Name))
		}
	}

	// PR poll + relay (orchid's existing polling, delivered via herdr send).
	c.pollPR(ctx, n, j, host, ref.Status)

	// Stranded: persistently idle with no PR → re-inject the goal (a poke that
	// re-delivers the task, not a bare Enter — fixes the gcp strandings). Debounced
	// to once per 10m and gated to idle only (not "done", which is often just
	// between-turns) so we never poke-storm healthy sessions.
	if known && (ref.Status == "idle" || ref.Status == "done") && j.PR == 0 && j.Pane != "" && time.Since(j.LastPoke) > 10*time.Minute {
		gctx, gcancel := context.WithTimeout(ctx, 15*time.Second)
		if host.send(gctx, j.Pane, "continue — implement the assigned issue fully, then open a PR. "+j.Goal) == nil {
			j.LastPoke = time.Now()
		}
		gcancel()
	}

	// Blocked: agent waiting for input it can't get in the swarm → escalate.
	if known && ref.Status == "blocked" {
		log.Printf("issue #%d: BLOCKED (needs input) on %s", n, host.Name)
		c.notify(fmt.Sprintf("issue #%d blocked — needs input", n))
	}
}

func (c *Coord) pollPR(ctx context.Context, n int, j *Job, host Host, status string) {
	if j.PR == 0 {
		if pr := ghPRByBranch(ctx, j.Repo, j.Branch, c.cfg.BotLogin); pr != 0 {
			j.PR = pr
		}
	}
	if j.PR == 0 {
		return
	}
	v, err := ghPRView(ctx, j.Repo, j.PR)
	if err != nil {
		return
	}

	// Auto-merge the green PR (opt-in per target) — this closes the autonomous
	// loop: merge ⇒ teardown ⇒ fan-out/continuation ⇒ more green PRs ⇒ … until
	// the upstream issue closes. Gate hard: MERGEABLE (no conflicts) and every CI
	// check green.
	if c.autoMergeEnabled(j.Repo) && v.Mergeable == "MERGEABLE" && checksGreen(v) {
		// A green+mergeable DRAFT whose worker has gone idle/done is finished work
		// waiting only on `gh pr ready`. Promote it ourselves and merge SAME TICK
		// instead of burning the 15m self-repoke window + a worker round-trip —
		// this is the throughput fix (drafts were the merge-cadence bottleneck). A
		// draft the worker is still actively pushing to ("working"/"blocked") is
		// left alone until it goes idle.
		if v.IsDraft {
			if status != "idle" && status != "done" {
				return // worker still building the draft — don't merge under it
			}
			rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
			out, rerr := run(rctx, "gh", "pr", "ready", strconv.Itoa(j.PR), "--repo", j.Repo)
			rcancel()
			if rerr != nil {
				log.Printf("issue #%d: promote green draft PR #%d failed: %v: %s", n, j.PR, rerr, strings.TrimSpace(out))
				return
			}
			log.Printf("issue #%d: promoted green draft PR #%d → ready (worker %s)", n, j.PR, status)
			v.IsDraft = false
		}
		mctx, mcancel := context.WithTimeout(ctx, 30*time.Second)
		out, merr := run(mctx, "gh", "pr", "merge", strconv.Itoa(j.PR), "--repo", j.Repo, "--squash")
		mcancel()
		if merr != nil {
			log.Printf("issue #%d: auto-merge PR #%d failed: %v: %s", n, j.PR, merr, strings.TrimSpace(out))
		} else {
			log.Printf("issue #%d: auto-merged PR #%d (%s, green)", n, j.PR, j.Repo)
		}
		return // merged (or will retry next tick) — skip the relay this cycle
	}

	// SELF-REPOKE: a worker that ships a PR then goes idle ("done"/"idle") will sit
	// forever on a PR that needs ITS action — CI came back red, or it's a green
	// draft that just needs `gh pr ready`. divybot's diff-relay only fires on NEW
	// activity, so an already-seen failure or a static draft strands the worker.
	// This is what made the swarm need hourly hand-nudging. Re-engage the idle
	// worker each tick (debounced 15m) with the concrete blocker so the autonomous
	// loop self-heals instead of waiting on a human.
	if (status == "idle" || status == "done") && j.Pane != "" && time.Since(j.LastPoke) > 15*time.Minute {
		if msg := stuckPRNudge(v); msg != "" {
			rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
			if host.send(rctx, j.Pane, msg) == nil {
				j.LastPoke = time.Now()
				log.Printf("issue #%d: self-repoke PR #%d (%s)", n, j.PR, prShortState(v))
			}
			rcancel()
			return
		}
	}

	lines, changed := diff(&j.Track, v, c.cfg.BotLogin)
	if !changed {
		return
	}
	if j.Pane == "" {
		return
	}
	msg := "New activity on your PR — address each item, push fixes, keep the PR green:\n" + strings.Join(lines, "\n")
	rctx, rcancel := context.WithTimeout(ctx, 20*time.Second)
	if err := host.send(rctx, j.Pane, msg); err != nil {
		log.Printf("issue #%d: relay failed: %v", n, err)
	}
	rcancel()
}

// stuckPRNudge returns a concrete re-engagement message for an idle worker whose
// PR needs ITS action, or "" if the PR is fine to wait on (CI still running, or
// already green and merging). The two stuck states that stranded the swarm:
//   - green DRAFT: ready to merge but auto-merge can't touch a draft → tell it to
//     `gh pr ready` so the loop can merge it;
//   - non-draft with FAILED checks: CI is red and the worker isn't re-engaging →
//     hand it the failing check names so it fixes + pushes.
//
// A PR with only pending checks, or green+non-draft (already merging), returns "".
func stuckPRNudge(v *PRView) string {
	if v.IsDraft {
		if v.Mergeable == "MERGEABLE" && checksGreen(v) {
			return fmt.Sprintf("PR #%d is green and mergeable but still a DRAFT, so it can't auto-merge. "+
				"If it's complete, mark it ready now: gh pr ready %d  — then it merges automatically. "+
				"If more work remains, keep going and mark ready when done.", v.Number, v.Number)
		}
		return "" // draft still being worked / not yet green
	}
	if failed := checksFailed(v); len(failed) > 0 {
		return fmt.Sprintf("PR #%d is NOT mergeable — CI is RED: %s failed. Don't sit idle on a red PR. "+
			"Run  gh pr checks %d  then  gh run view <run-id> --log-failed  to see the real cause, fix it, and push. "+
			"For a deno_core/check-* 'MISSING — baselined but not seen' message the cause is test DISCOVERY, not a "+
			"code bug. Auto-merge fires only when ALL checks are green.", v.Number, strings.Join(failed, ", "), v.Number)
	}
	return "" // non-draft, no failures → pending or green (merging) — nothing to do
}

// checksFailed returns the names of checks that CONCLUDED non-passing (real
// failures, ignoring still-pending/unconcluded checks).
func checksFailed(v *PRView) []string {
	var out []string
	for _, ck := range v.Checks {
		switch strings.ToUpper(ck.Conclusion) {
		case "FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE":
			out = append(out, ck.Name)
		}
	}
	return out
}

// prShortState is a one-word tag for logging the self-repoke reason.
func prShortState(v *PRView) string {
	if v.IsDraft {
		return "green-draft"
	}
	return "red:" + strings.Join(checksFailed(v), ",")
}

// autoMergeEnabled reports whether the target for this repo opted into auto-merge.
func (c *Coord) autoMergeEnabled(repo string) bool {
	for _, t := range c.cfg.Targets {
		if t.Repo == repo {
			return t.AutoMerge
		}
	}
	return false
}

// checksGreen is true when the PR has at least one CI check and every check has
// concluded as passing (SUCCESS/NEUTRAL/SKIPPED) — none pending, failing, or
// unconcluded. Zero checks ⇒ false (don't merge something with no CI signal).
func checksGreen(v *PRView) bool {
	if len(v.Checks) == 0 {
		return false
	}
	for _, ck := range v.Checks {
		switch strings.ToUpper(ck.Conclusion) {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			// passing
		default:
			return false // PENDING, FAILURE, CANCELLED, TIMED_OUT, ACTION_REQUIRED, ""
		}
	}
	return true
}

func (c *Coord) teardown(ctx context.Context, n int, j *Job) {
	host, ok := c.hosts[j.Host]
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	ws := j.Workspace
	if ws == "" {
		// Resolve from the live fleet by issue cwd if we never recorded it.
		if agents, err := host.agentList(cctx); err == nil {
			for _, a := range agents {
				if issueFromCwd(a.Cwd) == n {
					ws = a.WorkspaceID
					break
				}
			}
		}
	}
	if ws != "" {
		_ = host.closeWorkspace(cctx, ws)
	}

	// Reclaim the worktree. Closing the herdr workspace does NOT remove the
	// on-disk clone (orch-work/issue-N), and each carries a multi-GB build/target
	// dir — left behind they accumulate without bound (115 GB of dead worktrees
	// filled the mac-mini and ground builds to a halt). rm it on teardown.
	root := host.WorkdirRoot
	if root == "" {
		root = "/root/orch-work"
	}
	wd := fmt.Sprintf("%s/issue-%d", strings.TrimRight(root, "/"), n)
	if strings.Contains(wd, "/issue-") { // belt-and-suspenders: never rm a root
		rmctx, rmcancel := context.WithTimeout(ctx, 30*time.Second)
		if _, err := host.runRemote(rmctx, "rm -rf "+shq(wd)); err != nil {
			log.Printf("issue #%d: worktree cleanup %s failed: %v", n, wd, err)
		}
		rmcancel()
	}
	log.Printf("issue #%d: torn down (was on %s/%s)", n, j.Host, j.Label)

	// A torn-down job means its inbox stub closed — usually because the PR merged.
	// If that PR was a PARTIAL fix (the worker wrote "Refs #N", so the upstream
	// issue did NOT auto-close on merge), re-file ONE continuation stub for the
	// remaining work. This is EVENT-driven (one merge → at most one re-file),
	// never a backlog scan, so it can't storm the way an open-only feeder dedupe
	// did. Best-effort; failures just skip the continuation.
	c.maybeContinue(ctx, j)
}

// partialMerge reports whether job j shipped a PR that MERGED but only PARTIALLY
// resolved a SAME-repo upstream issue (still OPEN — GitHub would have auto-closed
// it had the worker written "Closes #N", so open ⇒ "Refs #N" partial). Returns
// the upstream ref ("owner/repo#N"), its number, and the PR number. ok=false when
// any gate fails (no tag, cross-repo, no PR, PR not merged, upstream not open).
func (c *Coord) partialMerge(ctx context.Context, j *Job) (ref string, refNum, pr int, ok bool) {
	if j == nil || j.Repo == "" {
		return "", 0, 0, false
	}
	m := inboxTitleRef.FindStringSubmatch(j.Title)
	if m == nil {
		return "", 0, 0, false
	}
	ref = m[1]
	hash := strings.LastIndex(ref, "#")
	if hash < 0 {
		return "", 0, 0, false
	}
	refRepo := ref[:hash]
	refNum, err := strconv.Atoi(ref[hash+1:])
	if err != nil || refNum == 0 || refRepo != j.Repo {
		return "", 0, 0, false
	}
	pr = j.PR
	if pr == 0 {
		pr = ghPRByBranch(ctx, j.Repo, j.Branch, c.cfg.BotLogin)
	}
	if pr == 0 || ghPRState(ctx, j.Repo, pr) != "MERGED" {
		return "", 0, 0, false
	}
	if ghIssueStateByNum(ctx, j.Repo, refNum) != "OPEN" {
		return "", 0, 0, false
	}
	return ref, refNum, pr, true
}

// fanoutGraceWindow is how long teardown waits after the fan-out nudge for the
// worker to wake, decide, and open its sibling inbox issues. Generous on purpose:
// the worker is an idle claude that must process a message and shell out to gh.
const fanoutGraceWindow = 4 * time.Minute

// fanoutGrace handles a teardown-candidate job (its inbox stub closed). If the PR
// was a partial merge, it nudges the still-alive worker to fan out the remaining
// work into sibling inbox issues and DEFERS teardown — returning true — until
// either the worker files a sibling stub or fanoutGraceWindow elapses. Returns
// false (let teardown + the maybeContinue fallback run) when it's not a partial
// merge, the worker is unreachable, the grace has expired, or a sibling already
// exists (the worker fanned out — teardown is safe and the fallback will no-op).
func (c *Coord) fanoutGrace(ctx context.Context, n int, j *Job) bool {
	ref, _, pr, ok := c.partialMerge(ctx, j)
	if !ok {
		return false // not a partial merge — normal teardown
	}
	// Already nudged: decide whether to keep waiting.
	if !j.FanoutNudgedAt.IsZero() {
		if open, err := openStubFor(ctx, c.cfg.Inbox, ref); err == nil && open {
			log.Printf("issue #%d: worker fanned out %s — tearing down", n, ref)
			return false // sibling(s) filed → done waiting
		}
		if time.Since(j.FanoutNudgedAt) >= fanoutGraceWindow {
			log.Printf("issue #%d: fan-out grace expired for %s — tearing down (fallback)", n, ref)
			return false // give up → generic continuation fallback
		}
		return true // keep deferring
	}
	host, hok := c.hosts[j.Host]
	if !hok || j.Pane == "" {
		return false // can't reach the worker — skip straight to teardown+fallback
	}
	msg := fmt.Sprintf(
		"Your PR #%d merged but %s still has work. Before this session ends: split the "+
			"REMAINING independent work into subtasks and open ONE inbox issue per subtask "+
			"(gh issue create --repo %s --label %s --title \"[%s] <subtask>\" --body \"<tight scope; "+
			"do ONLY this>\") so they run in PARALLEL. First  gh issue list --repo %s --state open "+
			"--search \"[%s] in:title\"  and skip any that already exist. If nothing independent "+
			"remains, do nothing — the orchestrator handles a single follow-up.",
		pr, ref, c.cfg.Inbox, c.labelFor(j.Repo), ref, c.cfg.Inbox, ref)
	sctx, scancel := context.WithTimeout(ctx, 20*time.Second)
	err := host.send(sctx, j.Pane, msg)
	scancel()
	if err != nil {
		log.Printf("issue #%d: fan-out nudge failed: %v — tearing down", n, err)
		return false
	}
	j.FanoutNudgedAt = time.Now()
	log.Printf("issue #%d: fan-out nudged (%s, PR #%d) — deferring teardown up to %s", n, ref, pr, fanoutGraceWindow)
	return true
}

// maybeContinue re-files ONE generic continuation stub as a FALLBACK when a
// merged-partial job's worker did NOT fan out any sibling inbox issues itself
// (openStubFor==false). When the worker DID fan out (one or more stubs tag the
// ref), this no-ops — the worker's scoped subtasks supersede the generic one.
// Gates: same as partialMerge, plus "no open stub already exists for the ref".
//
// No cap: every re-file requires a NEW merged PR, and the chain self-terminates
// when the upstream closes. The per-ref counter is kept for the attempt label.
func (c *Coord) maybeContinue(ctx context.Context, j *Job) {
	ref, refNum, pr, ok := c.partialMerge(ctx, j)
	if !ok {
		return
	}
	// Already a stub in flight for this ref (incl. ones the worker just fanned out)?
	// Then the remainder is already covered — don't add a generic one.
	if open, err := openStubFor(ctx, c.cfg.Inbox, ref); err != nil || open {
		return
	}
	// Count re-files per ref (for the attempt label + observability). No cap:
	// each one needs a fresh merged PR, and the chain ends when upstream closes.
	c.st.mu.Lock()
	if c.st.Continued == nil {
		c.st.Continued = map[string]int{}
	}
	c.st.Continued[ref]++
	attempt := c.st.Continued[ref]
	c.st.mu.Unlock()

	label := c.labelFor(j.Repo)
	if label == "" {
		return
	}
	upBody, _ := ghIssueBody(ctx, j.Repo, refNum)
	prs := priorMergedPRs(ctx, j.Repo, refNum)
	banner := fmt.Sprintf(
		"⚠️ CONTINUATION (attempt %d) — PR #%d merged but only PARTIALLY resolved this issue.\n",
		attempt, pr)
	if len(prs) > 0 {
		banner += "Merged PRs already shipped against it:\n" + strings.Join(prs, "\n") + "\n"
	}
	banner += "Read them (gh pr diff) BEFORE starting. Do NOT redo landed work — implement only the " +
		"REMAINING tasks. If nothing is left, write \"Closes #" + strconv.Itoa(refNum) + "\" and close it out.\n\n---\n\n"
	title := fmt.Sprintf("[%s] %s", ref, strings.TrimPrefix(j.Title, "["+ref+"] "))
	body := fmt.Sprintf("Continuation of [%s](https://github.com/%s/issues/%d) after partial PR #%d.\n\n---\n\n%s%s",
		ref, j.Repo, refNum, pr, banner, truncate(upBody, 2000))
	out, err := run(ctx, "gh", "issue", "create", "--repo", c.cfg.Inbox,
		"--label", label, "--title", title, "--body", body)
	if err != nil {
		log.Printf("continuation: gh issue create for %s failed: %v: %s", ref, err, strings.TrimSpace(out))
		return
	}
	log.Printf("continuation: re-filed %s (attempt %d, after PR #%d) → %s",
		ref, attempt, pr, strings.TrimSpace(out))
}

// labelFor returns the inbox label configured for a target repo ("" if none).
func (c *Coord) labelFor(repo string) string {
	for _, t := range c.cfg.Targets {
		if t.Repo == repo {
			return t.Label
		}
	}
	return ""
}

// ghPRState returns a PR's state: OPEN | CLOSED | MERGED ("" on error).
func ghPRState(ctx context.Context, repo string, pr int) string {
	var v struct {
		State string `json:"state"`
	}
	if err := ghJSON(ctx, &v, "pr", "view", strconv.Itoa(pr), "--repo", repo, "--json", "state"); err != nil {
		return ""
	}
	return v.State
}

// ghIssueStateByNum returns an issue's state: OPEN | CLOSED ("" on error).
func ghIssueStateByNum(ctx context.Context, repo string, n int) string {
	var v struct {
		State string `json:"state"`
	}
	if err := ghJSON(ctx, &v, "issue", "view", strconv.Itoa(n), "--repo", repo, "--json", "state"); err != nil {
		return ""
	}
	return v.State
}

// ghIssueBody returns an issue's body text.
func ghIssueBody(ctx context.Context, repo string, n int) (string, error) {
	var v struct {
		Body string `json:"body"`
	}
	if err := ghJSON(ctx, &v, "issue", "view", strconv.Itoa(n), "--repo", repo, "--json", "body"); err != nil {
		return "", err
	}
	return v.Body, nil
}

// openStubFor reports whether an OPEN inbox issue already tags this upstream ref
// ("owner/repo#N") via the "[owner/repo#N] ..." title convention.
func openStubFor(ctx context.Context, inbox, ref string) (bool, error) {
	var raw []struct {
		Title string `json:"title"`
	}
	if err := ghJSON(ctx, &raw, "issue", "list", "--repo", inbox,
		"--state", "open", "--limit", "1000", "--json", "title"); err != nil {
		return false, err
	}
	for _, r := range raw {
		if m := inboxTitleRef.FindStringSubmatch(r.Title); m != nil && m[1] == ref {
			return true, nil
		}
	}
	return false, nil
}

// priorMergedPRs returns the merged PRs already linked to an upstream issue, via
// its timeline cross-references — so a continuation stub can tell the worker what
// already shipped instead of redoing it. Best-effort: nil on any error.
func priorMergedPRs(ctx context.Context, repo string, n int) []string {
	var tl []struct {
		Event  string `json:"event"`
		Source struct {
			Issue struct {
				Number      int    `json:"number"`
				Title       string `json:"title"`
				PullRequest *struct {
					MergedAt *string `json:"merged_at"`
				} `json:"pull_request"`
			} `json:"issue"`
		} `json:"source"`
	}
	if err := ghJSON(ctx, &tl, "api", "-H", "Accept: application/vnd.github+json",
		fmt.Sprintf("repos/%s/issues/%d/timeline?per_page=100", repo, n)); err != nil {
		return nil
	}
	seen := map[int]bool{}
	var out []string
	for _, e := range tl {
		pr := e.Source.Issue
		if e.Event != "cross-referenced" || pr.PullRequest == nil || pr.PullRequest.MergedAt == nil {
			continue
		}
		if pr.Number == 0 || seen[pr.Number] {
			continue
		}
		seen[pr.Number] = true
		out = append(out, fmt.Sprintf("- #%d %s", pr.Number, strings.TrimSpace(pr.Title)))
	}
	return out
}

func (c *Coord) notify(msg string) {
	if c.cfg.NtfyTopic == "" {
		return
	}
	go func() {
		req, err := http.NewRequest("POST", "https://ntfy.sh/"+c.cfg.NtfyTopic, strings.NewReader(msg))
		if err != nil {
			return
		}
		cl := &http.Client{Timeout: 10 * time.Second}
		if resp, err := cl.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
}

// ============================ misc ============================

func durOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil && d > 0 {
		return d
	}
	return def
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ============================ main ============================

func main() {
	cfgPath := flag.String("config", "divybot.json", "path to config json")
	once := flag.Bool("once", false, "run a single tick and exit (for testing)")
	dry := flag.Bool("dryrun", false, "log spawn/adopt decisions, take no spawning/poking action")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.Inbox == "" || len(cfg.Hosts) == 0 || len(cfg.Targets) == 0 {
		log.Fatalf("config: inbox, hosts, and targets are required")
	}
	log.Printf("divybot: inbox=%s hosts=%d targets=%d governor=%v", cfg.Inbox, len(cfg.Hosts), len(cfg.Targets), cfg.Governor.Enabled)

	c := newCoord(cfg)
	c.dry = *dry
	if c.auth.token() == "" {
		log.Printf("WARNING: no gh token resolved (GH_TOKEN / gh auth token) — agents won't be able to push")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		c.tick(ctx)
		c.st.save()
		return
	}
	c.run(ctx)
}
