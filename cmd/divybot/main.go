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
	path string
}

func loadState(path string) *State {
	s := &State{Jobs: map[int]*Job{}, path: path}
	if b, err := os.ReadFile(path); err == nil {
		var raw struct {
			Jobs map[int]*Job `json:"jobs"`
		}
		if json.Unmarshal(b, &raw) == nil && raw.Jobs != nil {
			s.Jobs = raw.Jobs
		}
	}
	return s
}

func (s *State) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(struct {
		Jobs map[int]*Job `json:"jobs"`
	}{s.Jobs}, "", "  ")
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

// spawnAgent creates a workspace and starts a BARE agent (no clawpatrol).
func (h Host) spawnAgent(ctx context.Context, label, cwd string, env map[string]string, argv ...string) (pane, ws string, err error) {
	args := []string{"agent", "start", label, "--cwd", cwd, "--no-focus"}
	for k, v := range env {
		if v != "" {
			args = append(args, "--env", k+"="+v)
		}
	}
	args = append(args, "--")
	args = append(args, argv...)
	out, err := h.herdr(ctx, args...)
	if err != nil {
		return "", "", fmt.Errorf("%v: %.200q", err, out)
	}
	raw, err := herdrUnwrap(out)
	if err != nil {
		return "", "", err
	}
	var r struct {
		PaneID      string `json:"pane_id"`
		TerminalID  string `json:"terminal_id"`
		WorkspaceID string `json:"workspace_id"`
	}
	_ = json.Unmarshal(raw, &r)
	pane = r.PaneID
	if pane == "" {
		pane = r.TerminalID
	}
	return pane, r.WorkspaceID, nil
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

// syncToHost pushes all canonical creds to one host's agent home.
func (a *AuthStore) syncToHost(ctx context.Context, h Host) error {
	a.mu.Lock()
	creds, cfg, codex, tok := a.ClaudeCreds, a.ClaudeConfig, a.CodexAuth, a.GHToken
	a.mu.Unlock()
	home := h.agentHome()
	var errs []string
	if err := h.scpTo(ctx, creds, filepath.Join(home, ".claude", ".credentials.json")); err != nil {
		errs = append(errs, "claude:"+err.Error())
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
`,
		shq(home), shq(clone), shq(m.Branch), shq(repoURL), shq(repoURL),
		shq(bot), shq(email), shq(memdir), shq(memdir), shq(memdir))
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
		if ck.Conclusion == "" || ck.Conclusion == "SUCCESS" || ck.Conclusion == "NEUTRAL" {
			t.Checks[ck.Name] = ck.Conclusion
			continue
		}
		if t.Checks[ck.Name] == ck.Conclusion {
			continue // already relayed this failure
		}
		t.Checks[ck.Name] = ck.Conclusion
		lines = append(lines, fmt.Sprintf("CI %s: %s", ck.Name, ck.Conclusion))
		changed = true
	}
	if v.Mergeable == "CONFLICTING" && t.HeadOID != v.HeadOID {
		lines = append(lines, "merge-conflict: rebase onto the latest base branch, resolve conflicts, force-push.")
		changed = true
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

var rlRe = regexp.MustCompile(`(?i)anthropic-ratelimit-unified-(5h|7d|7day|week|weekly)-(used|status|limit|reset|remaining)\s*:\s*(\S+)`)

// sampleQuota curls Anthropic's API with the synced oauth token and parses the
// unified rate-limit response headers (5h + weekly used%). Coordinator-side, so
// it uses the canonical creds directly — no clawpatrol, no per-host probe.
func (a *AuthStore) sampleQuota(ctx context.Context) quota {
	a.mu.Lock()
	credsPath := a.ClaudeCreds
	a.mu.Unlock()
	tok := oauthFromCreds(credsPath)
	if tok == "" {
		return quota{}
	}
	body := `{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	out, _ := run(cctx, "curl", "-s", "-D", "-", "-o", "/dev/null", "-X", "POST",
		"https://api.anthropic.com/v1/messages",
		"-H", "authorization: Bearer "+tok,
		"-H", "anthropic-version:2023-06-01",
		"-H", "content-type:application/json",
		"-d", body)
	q := quota{at: time.Now()}
	for _, m := range rlRe.FindAllStringSubmatch(out, -1) {
		bucket, field, val := strings.ToLower(m[1]), strings.ToLower(m[2]), m[3]
		weekly := bucket == "7d" || bucket == "7day" || bucket == "week" || bucket == "weekly"
		switch field {
		case "used", "status":
			if f, err := strconv.ParseFloat(strings.TrimSuffix(val, "%"), 64); err == nil {
				if weekly {
					q.used7d, q.ok = f, true
				} else {
					q.used5h = f
				}
			}
		case "reset":
			if ts, err := strconv.ParseInt(val, 10, 64); err == nil && weekly {
				q.reset7d = time.Unix(ts, 0)
			}
		}
	}
	return q
}

// oauthFromCreds reads the sk-ant-oat access token from a claude credentials file.
func oauthFromCreds(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return ""
	}
	for _, key := range []string{"claudeAiOauth", "oauthAccount"} {
		if m, ok := raw[key].(map[string]any); ok {
			if t, ok := m["accessToken"].(string); ok && t != "" {
				return t
			}
		}
	}
	if t, ok := raw["accessToken"].(string); ok {
		return t
	}
	return ""
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
			for _, h := range c.activeHosts() {
				if err := c.auth.syncToHost(ctx, h); err != nil {
					log.Printf("authsync: %v", err)
				}
			}
		}
	}
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
		q := c.auth.sampleQuota(ctx)
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
	open, allOpen := c.pollIssues(ctx)

	// Teardown jobs whose inbox issue is gone.
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

	status := c.fleetStatus(ctx)

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

func (c *Coord) pollIssues(ctx context.Context) (map[int]Issue, map[int]bool) {
	open := map[int]Issue{}
	all := map[int]bool{}
	for _, tgt := range c.cfg.Targets {
		issues, err := ghIssues(ctx, c.cfg.Inbox, tgt.Label)
		if err != nil {
			log.Printf("poll %s/%s: %v", c.cfg.Inbox, tgt.Label, err)
			continue
		}
		for _, is := range issues {
			open[is.Number] = is
			all[is.Number] = true
		}
	}
	return open, all
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
func (c *Coord) fleetStatus(ctx context.Context) map[int]agentRef {
	out := map[int]agentRef{}
	for name, h := range c.hosts {
		cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		agents, err := h.agentList(cctx)
		cancel()
		if err != nil {
			log.Printf("fleetStatus %s: %v", name, err)
			continue
		}
		for _, a := range agents {
			n := issueFromCwd(a.Cwd)
			if n == 0 {
				continue
			}
			out[n] = agentRef{Host: name, Agent: a.Agent, Status: a.AgentStatus, Pane: a.PaneID, Workspace: a.WorkspaceID}
		}
	}
	return out
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
	var argv []string
	if agent == "codex" {
		argv = []string{"bash", "-lc", "exec codex --dangerously-bypass-approvals-and-sandbox"}
	} else {
		argv = []string{"bash", "-lc", "exec claude --dangerously-skip-permissions"}
	}

	// 3. Spawn BARE (no clawpatrol).
	sctx, scancel := context.WithTimeout(ctx, 40*time.Second)
	pane, ws, err := host.spawnAgent(sctx, label, workdir, env, argv...)
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

	// 4. Inject the goal.
	goal := fmt.Sprintf(
		"You are implementing %s issue #%d in this repo (branch %s, this is the worktree). Title: %s\n\n%s\n\n"+
			"Read the issue, implement the COMPLETE fix, add tests, build and run the relevant tests, then commit and open a PR to %s referencing the issue. Do not stop until the full goal is done.",
		tgt.Repo, n, branch, is.Title, truncate(is.Body, 1500), tgt.Repo)
	target := pane
	if target == "" {
		target = label
	}
	gctx, gcancel := context.WithTimeout(ctx, 20*time.Second)
	if err := host.send(gctx, target, goal); err != nil {
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
	if known && ref.Status == "idle" && j.PR == 0 && j.Pane != "" && time.Since(j.LastPoke) > 10*time.Minute {
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
