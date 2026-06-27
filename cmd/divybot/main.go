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
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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
	Label   string `json:"label"`    // inbox label, e.g. "deno"
	Repo    string `json:"repo"`     // e.g. "denoland/deno"
	Agent   string `json:"agent"`    // "claude" | "codex" (default claude)
	NeedCap string `json:"need_cap"` // required host capability, e.g. "build-deno"
}

// Gov holds the weekly-quota pacing knobs (the governor — paces against the Max
// subscription so the swarm never blows the weekly quota).
type Gov struct {
	Enabled      bool    `json:"enabled"`
	WeeklyCeiling float64 `json:"weekly_ceiling_pct"` // pause new work above this used% (default 92)
	Slack        float64 `json:"slack_pct"`          // throttle to MinActive within this of the ceiling (default 8)
	MaxActive    int     `json:"max_active"`         // global cap on concurrent agents (default 16)
	MinActive    int     `json:"min_active"`         // never fully stall under budget (default 1)
	// WeeklyTokenBudget paces against claude's OWN transcript token counts (no
	// clawpatrol, no API key): the governor sums input+output+cache-creation
	// tokens across the fleet's transcripts over the rolling 7 days and treats
	// used/budget as the weekly utilization. Set it to your Max plan's effective
	// weekly token allowance. 0 => token-budget pacing off (static cap).
	WeeklyTokenBudget int64 `json:"weekly_token_budget"`
}

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
	Track     tracker   `json:"track"`
}

type State struct {
	mu   sync.Mutex
	Jobs map[int]*Job `json:"jobs"`
	// Continued counts how many CONTINUATION stubs we've re-filed per upstream ref
	// ("owner/repo#N"), so a never-closing upstream can't churn forever. Persisted.
	Continued map[string]int `json:"continued"`
	path      string
}

func loadState(path string) *State {
	s := &State{Jobs: map[int]*Job{}, Continued: map[string]int{}, path: path}
	if b, err := os.ReadFile(path); err == nil {
		var raw struct {
			Jobs      map[int]*Job   `json:"jobs"`
			Continued map[string]int `json:"continued"`
		}
		if json.Unmarshal(b, &raw) == nil {
			if raw.Jobs != nil {
				s.Jobs = raw.Jobs
			}
			if raw.Continued != nil {
				s.Continued = raw.Continued
			}
		}
	}
	return s
}

func (s *State) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(struct {
		Jobs      map[int]*Job   `json:"jobs"`
		Continued map[string]int `json:"continued"`
	}{s.Jobs, s.Continued}, "", "  ")
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
}

func ghPRView(ctx context.Context, repo string, n int) (*PRView, error) {
	var raw struct {
		Number          int    `json:"number"`
		Mergeable       string `json:"mergeable"`
		HeadRefOid      string `json:"headRefOid"`
		Reviews         []struct {
			ID     string `json:"id"`
			Author struct{ Login string } `json:"author"`
			Body   string `json:"body"`
			State  string `json:"state"`
		} `json:"reviews"`
		Comments []struct {
			ID     string `json:"id"`
			Author struct{ Login string } `json:"author"`
			Body   string `json:"body"`
		} `json:"comments"`
		StatusCheckRollup []struct {
			Name       string `json:"name"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
		} `json:"statusCheckRollup"`
	}
	if err := ghJSON(ctx, &raw, "pr", "view", strconv.Itoa(n), "--repo", repo,
		"--json", "number,mergeable,headRefOid,reviews,comments,statusCheckRollup"); err != nil {
		return nil, err
	}
	v := &PRView{Number: raw.Number, Mergeable: raw.Mergeable, HeadOID: raw.HeadRefOid}
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
		Number int    `json:"number"`
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

// quota holds the live rate-limit reading for one account.
type quota struct {
	used5h  float64
	used7d  float64
	reset7d time.Time
	at      time.Time
	ok      bool
}

// tokenSumScript sums input+output+cache-creation tokens from a host's claude
// transcripts touched in the last 7 days (cache-read is ~free, excluded). Pure
// local read of claude's own JSONL — no API, no clawpatrol.
func (h Host) tokenSumScript() string {
	home := h.agentHome()
	return fmt.Sprintf(`export HOME=%s
find "$HOME/.claude/projects" -name '*.jsonl' -mtime -7 2>/dev/null -print0 \
 | xargs -0 cat 2>/dev/null \
 | jq -r 'try (.message.usage | (.input_tokens // 0)+(.output_tokens // 0)+(.cache_creation_input_tokens // 0)) catch empty' 2>/dev/null \
 | awk '{s+=$1} END{print s+0}'`, shq(home))
}

// sampleQuota computes weekly utilization from the fleet's transcript token sums
// vs the configured budget. No clawpatrol, no API key — reads claude's own data.
func (c *Coord) sampleQuota(ctx context.Context) quota {
	budget := c.cfg.Governor.WeeklyTokenBudget
	if budget <= 0 {
		return quota{}
	}
	var total int64
	for _, h := range c.cfg.Hosts {
		cctx, cancel := context.WithTimeout(ctx, 40*time.Second)
		out, err := h.runRemote(cctx, h.tokenSumScript())
		cancel()
		if err != nil {
			continue
		}
		if n, e := strconv.ParseInt(strings.TrimSpace(out), 10, 64); e == nil {
			total += n
		}
	}
	return quota{used7d: float64(total) / float64(budget) * 100, ok: total > 0, at: time.Now()}
}

// govCap returns the max concurrent agents the governor allows right now. Slim
// threshold control (the proportional version lived in governor.go): pause new
// work above the weekly ceiling, throttle to MinActive within the slack band,
// otherwise MaxActive. Fail-open (MaxActive) when no quota reading.
func (g Gov) govCap(q quota) int {
	if !g.Enabled || !q.ok {
		return g.MaxActive
	}
	if q.used7d >= g.WeeklyCeiling {
		return 0
	}
	if q.used7d >= g.WeeklyCeiling-g.Slack {
		return g.MinActive
	}
	return g.MaxActive
}

// ============================ coordinator ============================

type Coord struct {
	cfg   *Config
	st    *State
	auth  *AuthStore
	hosts map[string]Host
	dry   bool // dry-run: log spawn/adopt decisions, take no spawning action
	gov   struct {
		mu sync.Mutex
		q  quota
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
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		q := c.sampleQuota(ctx)
		if q.ok {
			c.gov.mu.Lock()
			c.gov.q = q
			c.gov.mu.Unlock()
			log.Printf("governor: weekly %.0f%% used / 5h %.0f%% → cap %d", q.used7d, q.used5h, c.cfg.Governor.govCap(q))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Coord) curCap() int {
	c.gov.mu.Lock()
	q := c.gov.q
	c.gov.mu.Unlock()
	return c.cfg.Governor.govCap(q)
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

	// Admission budget = governor cap − currently running.
	cap := c.curCap()
	c.st.mu.Lock()
	running := len(c.st.Jobs)
	c.st.mu.Unlock()
	budget := cap - running

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
		if budget <= 0 {
			continue
		}
		if c.dry {
			log.Printf("issue #%d: would spawn (dry-run)", n)
			budget--
			continue
		}
		if c.spawn(ctx, n, is) {
			budget--
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

Then stop and wait. The orchestrator sends a follow-up when reviews, comments,
or CI results arrive. Address them, push fixes, stop again.
The session ends automatically when the PR merges or closes.`

// renderGoal fills the worker prompt template for one issue.
func renderGoal(inbox, targetRepo, title, body, workdir, branch string, n int) string {
	return strings.NewReplacer(
		"{{issue.number}}", strconv.Itoa(n),
		"{{inbox.repo}}", inbox,
		"{{issue.title}}", title,
		"{{target.repo}}", targetRepo,
		"{{workdir}}", workdir,
		"{{branch}}", branch,
		"{{issue.body}}", truncate(body, 4000),
	).Replace(workerPrompt)
}

func (c *Coord) spawn(ctx context.Context, n int, is Issue) bool {
	tgt, ok := c.targetFor(is)
	if !ok {
		return false
	}
	host, ok := c.pickHost(tgt)
	if !ok {
		return false
	}
	agent := tgt.Agent
	if agent == "" {
		agent = "claude"
	}

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
	goal := renderGoal(c.cfg.Inbox, tgt.Repo, is.Title, is.Body, workdir, branch, n)
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
	c.pollPR(ctx, n, j, host)

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

func (c *Coord) pollPR(ctx context.Context, n int, j *Job, host Host) {
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
	log.Printf("issue #%d: torn down (was on %s/%s)", n, j.Host, j.Label)

	// A torn-down job means its inbox stub closed — usually because the PR merged.
	// If that PR was a PARTIAL fix (the worker wrote "Refs #N", so the upstream
	// issue did NOT auto-close on merge), re-file ONE continuation stub for the
	// remaining work. This is EVENT-driven (one merge → at most one re-file),
	// never a backlog scan, so it can't storm the way an open-only feeder dedupe
	// did. Best-effort; failures just skip the continuation.
	c.maybeContinue(ctx, j)
}

// maybeContinue re-files a continuation inbox stub when a tracked job's PR
// merged but only PARTIALLY resolved its upstream issue. Gates (all required):
//   - the job shipped a PR, and that PR is MERGED (real progress landed);
//   - the upstream issue is SAME-repo and still OPEN (had the worker written
//     "Closes #N", GitHub would have auto-closed it on merge — open ⇒ partial);
//   - no open inbox stub already exists for the ref (not already in flight).
//
// There is no cap: every re-file requires a NEW merged PR (real progress) and
// the chain self-terminates the moment the worker writes "Closes #N" (upstream
// closes ⇒ gate fails). The per-ref counter is kept only for the attempt label.
func (c *Coord) maybeContinue(ctx context.Context, j *Job) {
	if j == nil || j.Repo == "" {
		return
	}
	m := inboxTitleRef.FindStringSubmatch(j.Title)
	if m == nil {
		return // not an assignment-fed stub (no "[owner/repo#N]" tag)
	}
	ref := m[1] // "owner/repo#N"
	hash := strings.LastIndex(ref, "#")
	if hash < 0 {
		return
	}
	refRepo := ref[:hash]
	refNum, err := strconv.Atoi(ref[hash+1:])
	if err != nil || refNum == 0 {
		return
	}
	// Continuation hinges on GitHub's same-repo auto-close as the partial signal;
	// a cross-repo ref can't auto-close, so we can't tell partial from done.
	if refRepo != j.Repo {
		return
	}

	// Resolve the PR (may not have been recorded if it merged fast); need it MERGED.
	pr := j.PR
	if pr == 0 {
		pr = ghPRByBranch(ctx, j.Repo, j.Branch, c.cfg.BotLogin)
	}
	if pr == 0 || ghPRState(ctx, j.Repo, pr) != "MERGED" {
		return
	}
	// Upstream still open ⇒ the merged PR was a partial ("Refs #N"). Done ⇒ skip.
	if ghIssueStateByNum(ctx, j.Repo, refNum) != "OPEN" {
		return
	}
	// Already a stub in flight for this ref? Don't double up.
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
