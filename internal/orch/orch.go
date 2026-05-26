package orch

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	GitHub              GitHubBlock   `hcl:"github,block" json:"github"`
	Orch                OrchBlock     `hcl:"orchestrator,block" json:"orchestrator"`
	BootstrapPrompt     string        `hcl:"bootstrap_prompt" json:"bootstrap_prompt"`
	CronBootstrapPrompt string        `hcl:"cron_bootstrap_prompt,optional" json:"cron_bootstrap_prompt,omitempty"`
	Targets             []TargetBlock `hcl:"target,block" json:"targets"`
	VMs                 []VMBlock     `hcl:"vm,block" json:"vms"`
}

type GitHubBlock struct {
	InboxRepo string `hcl:"inbox_repo" json:"inbox_repo"`
}

type TargetBlock struct {
	Name  string `hcl:",label" json:"name"`
	Label string `hcl:"label" json:"label,omitempty"`
	Repo  string `hcl:"repo" json:"repo"`
}

type OrchBlock struct {
	PollInterval  string         `hcl:"poll_interval" json:"poll_interval"`
	StateDB       string         `hcl:"state_db" json:"state_db"`
	BranchPrefix  string         `hcl:"branch_prefix" json:"branch_prefix"`
	WorkdirRoot   string         `hcl:"workdir_root" json:"workdir_root"`
	HTTPAddr      string         `hcl:"http_addr,optional" json:"http_addr,omitempty"`
	HTTPSecret    string         `hcl:"http_secret,optional" json:"http_secret,omitempty"`
	AllowedLogins []string       `hcl:"allowed_logins,optional" json:"allowed_logins,omitempty"`
	BotLogin      string         `hcl:"bot_login,optional" json:"bot_login,omitempty"`
	BotEmail      string         `hcl:"bot_email,optional" json:"bot_email,omitempty"`
	NtfyTopic     string         `hcl:"ntfy_topic,optional" json:"ntfy_topic,omitempty"`
	BotGithubKey  string         `hcl:"bot_github_key,optional" json:"bot_github_key,omitempty"`
	Mentions      *MentionsBlock `hcl:"mentions,block" json:"mentions,omitempty"`
	Capture       *CaptureBlock  `hcl:"capture,block" json:"capture,omitempty"`
}

type CaptureBlock struct {
	AuthToken     string   `hcl:"auth_token" json:"auth_token"`
	AssetsDir     string   `hcl:"assets_dir,optional" json:"assets_dir,omitempty"`
	PublicURL     string   `hcl:"public_url,optional" json:"public_url,omitempty"`
	DefaultRepo   string   `hcl:"default_repo,optional" json:"default_repo,omitempty"`
	DefaultLabels []string `hcl:"default_labels,optional" json:"default_labels,omitempty"`
	MaxBodyMB     int      `hcl:"max_body_mb,optional" json:"max_body_mb,omitempty"`
	AllowedRepos  []string `hcl:"allowed_repos,optional" json:"allowed_repos,omitempty"`
}

type MentionsBlock struct {
	PollInterval   string   `hcl:"poll_interval,optional"`
	Org            string   `hcl:"org"`
	MaintainerTTL  string   `hcl:"maintainer_ttl,optional"`
	Acknowledge    bool     `hcl:"acknowledge,optional"`
	HumanOverrides []string `hcl:"human_overrides,optional"`
	LLMCommand     []string `hcl:"llm_command,optional"`
}

type VMBlock struct {
	Name            string `hcl:",label" json:"name"`
	Host            string `hcl:"host" json:"host"`
	User            string `hcl:"user,optional" json:"user,omitempty"`
	Key             string `hcl:"key,optional" json:"key,omitempty"`
	Capacity        int    `hcl:"capacity,optional" json:"capacity,omitempty"`
	Sccache         bool   `hcl:"sccache,optional" json:"sccache,omitempty"`
	SccacheDir      string `hcl:"sccache_dir,optional" json:"sccache_dir,omitempty"`
	SessionCmd      string `hcl:"session_cmd,optional" json:"session_cmd,omitempty"`
	SessionHome     string `hcl:"session_home,optional" json:"session_home,omitempty"`
	WorkdirRoot     string `hcl:"workdir_root,optional" json:"workdir_root,omitempty"`
	BotLogin        string `hcl:"bot_login,optional" json:"bot_login,omitempty"`
	BotEmail        string `hcl:"bot_email,optional" json:"bot_email,omitempty"`
	Agent           string `hcl:"agent,optional" json:"agent,omitempty"`
	IdleMarker      string `hcl:"idle_marker,optional" json:"idle_marker,omitempty"`
	BusyMarker      string `hcl:"busy_marker,optional" json:"busy_marker,omitempty"`
	BootstrapPrompt string `hcl:"bootstrap_prompt,optional" json:"bootstrap_prompt,omitempty"`
	JoinManaged     bool   `hcl:"join_managed,optional" json:"join_managed,omitempty"`
}

// Job lifecycle: "oneshot" (default) — issue → session → PR → teardown.
// "cron" — issue stays open, ephemeral session fires every Schedule, no PR.
type Job struct {
	VM                   string            `json:"vm"`
	Tmux                 string            `json:"tmux"`
	Target               string            `json:"target"`
	TargetRepo           string            `json:"target_repo"`
	Branch               string            `json:"branch"`
	IssueTitle           string            `json:"issue_title,omitempty"`
	Lifecycle            string            `json:"lifecycle,omitempty"`
	Schedule             string            `json:"schedule,omitempty"`
	Timeout              string            `json:"timeout,omitempty"`
	NextFireAt           time.Time         `json:"next_fire_at,omitempty"`
	FireStartedAt        time.Time         `json:"fire_started_at,omitempty"`
	PR                   int               `json:"pr,omitempty"`
	SeenReviewIDs        []string          `json:"seen_review_ids,omitempty"`
	SeenThreadCommentIDs []string          `json:"seen_thread_comment_ids,omitempty"`
	SeenIssueCommentIDs  []string          `json:"seen_issue_comment_ids,omitempty"`
	LastHeadOID          string            `json:"last_head_oid,omitempty"`
	LastCheckConclusions map[string]string `json:"last_check_conclusions,omitempty"`
	LastMergeable        string            `json:"last_mergeable,omitempty"`
}

type State struct {
	mu            sync.Mutex
	Jobs          map[int]*Job
	MentionCursor *time.Time
	Maintainers   *MaintainerCache
	store         *Store
	httpSnap      atomic.Value
	Bcast         chan struct{} `json:"-"`
}

// MaintainerCache caches the configured org's member logins. Refreshed
// lazily by the mention poller when older than MentionsBlock.MaintainerTTL.
type MaintainerCache struct {
	FetchedAt time.Time `json:"fetched_at"`
	Members   []string  `json:"members"`
}

// has returns true if login is in the cached member list.
func (c *MaintainerCache) has(login string) bool {
	if c == nil {
		return false
	}
	for _, m := range c.Members {
		if m == login {
			return true
		}
	}
	return false
}

const runAttempts = 4

const maxKillsPerTick = 2

// killBudget tracks dead-session respawns issued so far this tick. Use
// tryUse to attempt a kill; it returns false once the per-tick cap is hit.
type killBudget struct {
	max  int
	used int
}

func (b *killBudget) tryUse() bool {
	if b.used >= b.max {
		return false
	}
	b.used++
	return true
}

var orchBootTime = time.Now()

func run(name string, args ...string) (string, string, error) {
	return runIn("", name, args...)
}

func runIn(stdin string, name string, args ...string) (string, string, error) {
	var lastOut, lastErr string
	var lastE error
	for i := 0; i < runAttempts; i++ {
		cmd := exec.Command(name, args...)
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		var o, e bytes.Buffer
		cmd.Stdout = &o
		cmd.Stderr = &e
		lastE = cmd.Run()
		lastOut, lastErr = o.String(), e.String()
		if lastE == nil {
			return lastOut, lastErr, nil
		}
		if !isTransient(lastE, lastErr) || i == runAttempts-1 {
			break
		}
		backoff := time.Duration(1<<uint(i)) * time.Second
		log.Printf("retry %s: attempt %d/%d transient failure (%v); sleeping %s", name, i+1, runAttempts, lastE, backoff)
		time.Sleep(backoff)
	}
	return lastOut, lastErr, lastE
}

// isTransient classifies clawpatrol-style network blips that should trigger
// a retry. Anything else (gh 404, tmux exit 1 = no session, etc.) returns
// false so we don't waste budget on permanent errors.
func isTransient(err error, stderr string) bool {
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 255 {
		return true
	}
	for _, pat := range []string{
		"error connecting to api.github.com",
		"Could not resolve host",
		"Connection timed out",
		"Connection refused",
		"Connection reset by peer",
		"network is unreachable",
		"i/o timeout",
		"TLS handshake",
		"unexpected EOF",
	} {
		if strings.Contains(stderr, pat) {
			return true
		}
	}
	return false
}

func isLocal(vm VMBlock) bool {
	return vm.Host == "localhost" || vm.Host == "127.0.0.1"
}

func expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return h + p[1:]
	}
	return p
}

// atoiClamp parses s; if invalid returns def. Result is clamped to [lo, hi].
func atoiClamp(s string, def, lo, hi int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}

func sshArgs(vm VMBlock) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-i", expand(vm.Key),
		fmt.Sprintf("%s@%s", vm.User, vm.Host),
	}
}

// sshExec runs a shell command on the VM. For localhost, skips SSH overhead.
func sshExec(vm VMBlock, remote string) (string, string, error) {
	if isLocal(vm) {
		return run("bash", "-c", remote)
	}
	return run("ssh", append(sshArgs(vm), remote)...)
}

func sshExecIn(vm VMBlock, stdin, remote string) (string, string, error) {
	if isLocal(vm) {
		return runIn(stdin, "bash", "-c", remote)
	}
	return runIn(stdin, "ssh", append(sshArgs(vm), remote)...)
}

type Issue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	State  string   `json:"state"`
	Labels []string `json:"labels"`
}

// CronConfig holds parsed cron parameters from an issue's toml frontmatter.
type CronConfig struct {
	Schedule    time.Duration
	ScheduleStr string
	Timeout     time.Duration
	TimeoutStr  string
}

func parseCronFrontmatter(body string) *CronConfig {
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "```toml" {
		return nil
	}
	i++
	cfg := &CronConfig{}
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "```" {
			break
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), "\"'")
		switch key {
		case "schedule":
			d, err := time.ParseDuration(val)
			if err != nil {
				return nil
			}
			cfg.Schedule, cfg.ScheduleStr = d, val
		case "timeout":
			d, err := time.ParseDuration(val)
			if err == nil {
				cfg.Timeout, cfg.TimeoutStr = d, val
			}
		}
	}
	if cfg.Schedule == 0 {
		return nil
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = cfg.Schedule / 2
		cfg.TimeoutStr = cfg.Timeout.String()
	}
	return cfg
}

func tmuxHasSession(vm VMBlock, session string) (bool, error) {
	_, _, err := sshExec(vm, fmt.Sprintf("tmux has-session -t %s 2>/dev/null", session))
	if err == nil {
		return true, nil
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func vmWorkdirRoot(orch OrchBlock, vm VMBlock) string {
	if vm.WorkdirRoot != "" {
		return strings.TrimRight(vm.WorkdirRoot, "/")
	}
	return strings.TrimRight(orch.WorkdirRoot, "/")
}

func bootstrapVM(vm VMBlock) error {
	sccacheDir := vm.SccacheDir
	if sccacheDir == "" {
		sccacheDir = "~/.cache/sccache"
	}
	sccacheSetup := ""
	if vm.Sccache {
		sccacheSetup = fmt.Sprintf(`
# sccache: start tmux server (idempotent), push shared env so every
# session on this VM inherits RUSTC_WRAPPER without per-pane setup.
tmux start-server 2>/dev/null || true
mkdir -p %s
tmux setenv -g RUSTC_WRAPPER sccache
tmux setenv -g SCCACHE_DIR %s
`, sccacheDir, sccacheDir)
	}

	commonScript := fmt.Sprintf(`set -e
mkdir -p ~/.claude
if [ -f ~/.claude/settings.json ]; then
  jq '. + {skipDangerousModePermissionPrompt: true, includeCoAuthoredBy: false}' ~/.claude/settings.json > ~/.claude/settings.json.tmp && mv ~/.claude/settings.json.tmp ~/.claude/settings.json
else
  echo '{"theme":"dark","skipDangerousModePermissionPrompt":true,"includeCoAuthoredBy":false}' > ~/.claude/settings.json
fi
[ -f ~/.claude.json ] || echo '{}' > ~/.claude.json
%s
# github auth check: try ssh first (the historic path), fall back to
# the gh CLI's https auth when ssh is blocked by the VM's network
# (exe.dev and similar shared hosts intercept outbound port 22 to
# github). Either is enough to git clone work repos.
{
  ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -T git@github.com 2>&1 | head -1
  gh auth status -h github.com 2>&1 | grep -m1 -E 'Logged in to github.com|Active account' || true
} | tr '\n' ' '
echo
`, sccacheSetup)

	var out, errStr string
	var err error

	if isLocal(vm) {
		out, errStr, err = runIn(commonScript, "bash", "-s")
	} else if vm.JoinManaged {
		remoteScript := fmt.Sprintf(`set -e
umask 077
mkdir -m 700 -p ~/.ssh
touch ~/.ssh/known_hosts && chmod 644 ~/.ssh/known_hosts
if ! grep -q '^github.com ' ~/.ssh/known_hosts 2>/dev/null; then
  ssh-keyscan -t ed25519,rsa github.com 2>/dev/null >> ~/.ssh/known_hosts
fi
%s`, commonScript)
		out, errStr, err = sshExecIn(vm, remoteScript, "bash -s")
	} else {
		keyPath := expand(vm.Key)
		priv, rerr := os.ReadFile(keyPath)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", keyPath, rerr)
		}
		pub, rerr := os.ReadFile(keyPath + ".pub")
		if rerr != nil {
			return fmt.Errorf("read %s.pub: %w", keyPath, rerr)
		}
		remoteScript := fmt.Sprintf(`set -e
umask 077
mkdir -m 700 -p ~/.ssh
if [ ! -e ~/.ssh/id_ed25519 ]; then
  echo %s | base64 -d > ~/.ssh/id_ed25519
  chmod 600 ~/.ssh/id_ed25519
  echo %s | base64 -d > ~/.ssh/id_ed25519.pub
  chmod 644 ~/.ssh/id_ed25519.pub
fi
touch ~/.ssh/known_hosts && chmod 644 ~/.ssh/known_hosts
if ! grep -q '^github.com ' ~/.ssh/known_hosts 2>/dev/null; then
  ssh-keyscan -t ed25519,rsa github.com 2>/dev/null >> ~/.ssh/known_hosts
fi
%s`,
			base64.StdEncoding.EncodeToString(priv),
			base64.StdEncoding.EncodeToString(pub),
			commonScript)
		out, errStr, err = sshExecIn(vm, remoteScript, "bash -s")
	}

	if err != nil {
		return fmt.Errorf("%v: %s", err, errStr)
	}
	if !strings.Contains(out, "successfully authenticated") &&
		!strings.Contains(out, "Logged in to github.com") {
		return fmt.Errorf("github auth check failed (no ssh + no gh login): %q", strings.TrimSpace(out))
	}
	return nil
}

func tmuxStart(vm VMBlock, session, workdir, sharedDir, repo, branch, sessionCmdOverride, botLogin, botEmail string) error {
	sessionCmd := sessionCmdOverride
	if sessionCmd == "" {
		sessionCmd = vm.SessionCmd
	}
	if sessionCmd == "" {
		sessionCmd = "clawpatrol run -- claude --dangerously-skip-permissions"
	}
	sessionHome := vm.SessionHome
	agent := vmAgent(vm).name
	script := fmt.Sprintf(`set -e
SHARED=%q
REPO=%q
WORKDIR=%q
BRANCH=%q
SESSION=%q
SESSION_CMD=%q
SESSION_HOME=%q
BOT_LOGIN=%q
BOT_EMAIL=%q
AGENT=%q

# 1) shared clone (once per repo per VM); always fetch fresh refs.
# Prefer ssh when the bot has an id_ed25519 wired to github; fall back
# to https via gh's credential helper when port 22 is blocked
# (shared-tenant hosts like exe.dev). Either way refs get fetched.
if [ ! -d "$SHARED/.git" ]; then
  mkdir -p "$(dirname "$SHARED")"
  if ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -T git@github.com 2>&1 | grep -q 'successfully authenticated'; then
    git clone "git@github.com:$REPO.git" "$SHARED"
  else
    gh auth setup-git -h github.com >/dev/null 2>&1 || true
    git clone "https://github.com/$REPO.git" "$SHARED"
  fi
fi
git -C "$SHARED" fetch origin --prune --quiet

# 2) per-issue worktree off the shared clone (worktree's .git is a file,
#    so check -e not -d). If a previous worktree dir is gone we prune
#    stale references from the shared clone before re-adding.
if [ ! -e "$WORKDIR/.git" ]; then
  rm -rf "$WORKDIR"
  git -C "$SHARED" worktree prune
  if git -C "$SHARED" ls-remote --exit-code --heads origin "$BRANCH" >/dev/null 2>&1; then
    git -C "$SHARED" worktree add -B "$BRANCH" "$WORKDIR" "origin/$BRANCH"
  else
    git -C "$SHARED" worktree add -B "$BRANCH" "$WORKDIR" origin/main
  fi
fi

# 3) bot identity (worktrees inherit from shared clone, but pin locally)
git -C "$WORKDIR" config user.name "$BOT_LOGIN"
git -C "$WORKDIR" config user.email "$BOT_EMAIL"

# 3b) if session runs as a different user, chown worktree + shared clone to them
if [ -n "$SESSION_HOME" ] && [ "$SESSION_HOME" != "~" ]; then
  SESSION_USER=$(stat -c '%%U' "$SESSION_HOME")
  chown -R "$SESSION_USER:$SESSION_USER" "$WORKDIR" "$SHARED" 2>/dev/null || true
fi

# 4) agent-specific pre-warm. Claude needs its per-folder trust dialog
# pre-stamped so the TUI does not prompt; codex stores trust in ~/.codex/
# and is operator-onboarded via the codex login subcommand once per VM,
# no per-folder stamping. Stamp $HOME (the user running this script) and
# SESSION_HOME if set to a different user.
if [ "$AGENT" = "claude" ]; then
  stamp_trust() {
    local CHOME="$1"
    [ -z "$CHOME" ] && return
    local CJSON="$CHOME/.claude.json"
    [ -f "$CJSON" ] || echo '{}' > "$CJSON"
    jq --arg d "$WORKDIR" '.projects[$d].hasTrustDialogAccepted = true' "$CJSON" > "$CJSON.tmp" && mv "$CJSON.tmp" "$CJSON"
  }
  stamp_trust "$HOME"
  if [ -n "$SESSION_HOME" ] && [ "$SESSION_HOME" != "~" ] && [ "$SESSION_HOME" != "$HOME" ]; then
    stamp_trust "$SESSION_HOME"
  fi
fi

# 5) launch the pane
tmux kill-session -t "$SESSION" 2>/dev/null || true
tmux new-session -d -c "$WORKDIR" -s "$SESSION" "$SESSION_CMD"
`, sharedDir, repo, workdir, branch, session, sessionCmd, sessionHome, botLogin, botEmail, agent)

	_, errStr, err := sshExecIn(vm, script, "bash -s")
	if err != nil {
		return fmt.Errorf("tmux start: %v: %s", err, errStr)
	}
	return nil
}

func tmuxKill(vm VMBlock, session string) {
	_, _, _ = sshExec(vm, fmt.Sprintf("tmux kill-session -t %s 2>/dev/null", session))
}

func tmuxIdle(vm VMBlock, session string) (idle bool, detected string, err error) {
	out, _, e := sshExec(vm, fmt.Sprintf("tmux capture-pane -p -t %s", session))
	if e != nil {
		return false, "", e
	}
	detected = detectAgentFromPane(out)
	spec := vmAgent(vm)
	if !strings.Contains(out, spec.idleMarker) {
		return false, detected, nil
	}
	if spec.busyMarker != "" && strings.Contains(out, spec.busyMarker) {
		return false, detected, nil
	}
	if panePrompted(out, spec) {
		return false, detected, nil
	}
	return true, detected, nil
}

func panePrompted(pane string, spec agentSpec) bool {
	if spec.busyMarker != "" && strings.Contains(pane, spec.busyMarker) {
		return false
	}
	for _, m := range spec.promptMarkers {
		if m != "" && strings.Contains(pane, m) {
			return true
		}
	}
	return false
}

// detectAgentFromPane reads the pane content and returns the agent name
// that's actually running in the pane (claude or codex), or "" if no
// known marker matches. Independent of vm.Agent config.
func detectAgentFromPane(pane string) string {
	switch {
	case strings.Contains(pane, "gpt-"):
		return "codex"
	case strings.Contains(pane, "bypass permissions"):
		return "claude"
	}
	return ""
}

// Per-session pane-activity ring. Updated by paneActivityLoop, read by
// /api/state so the dashboard can colour cards as working / idle without
// each browser polling tmux directly.
const paneActivityWindow = 20

type paneActivityRecord struct {
	prevHash uint64
	ring     []int
}

var (
	paneActivityMu sync.Mutex
	paneActivity   = map[string]*paneActivityRecord{}
)

func paneActivitySnapshot(tmux string) []int {
	paneActivityMu.Lock()
	defer paneActivityMu.Unlock()
	r, ok := paneActivity[tmux]
	if !ok || len(r.ring) == 0 {
		return nil
	}
	out := make([]int, len(r.ring))
	copy(out, r.ring)
	return out
}

// paneActivityRecordTick records a sample for `tmux` and returns true
// if this tick saw activity (pane hash changed since the previous sample).
func paneActivityRecordTick(tmux string, hash uint64) bool {
	paneActivityMu.Lock()
	defer paneActivityMu.Unlock()
	r, ok := paneActivity[tmux]
	if !ok {
		paneActivity[tmux] = &paneActivityRecord{prevHash: hash}
		return false
	}
	v := 0
	if hash != r.prevHash {
		v = 1
	}
	r.prevHash = hash
	r.ring = append(r.ring, v)
	if len(r.ring) > paneActivityWindow {
		r.ring = r.ring[len(r.ring)-paneActivityWindow:]
	}
	return v == 1
}

func paneActivityPrune(live map[string]bool) {
	paneActivityMu.Lock()
	defer paneActivityMu.Unlock()
	for k := range paneActivity {
		if !live[k] {
			delete(paneActivity, k)
		}
	}
}

var (
	paneNeedsInputMu sync.Mutex
	paneNeedsInput   = map[string]bool{}
)

func paneNeedsInputSnapshot(tmux string) bool {
	paneNeedsInputMu.Lock()
	defer paneNeedsInputMu.Unlock()
	return paneNeedsInput[tmux]
}

func paneNeedsInputSet(tmux string, needs bool) (changed bool) {
	paneNeedsInputMu.Lock()
	defer paneNeedsInputMu.Unlock()
	prev, had := paneNeedsInput[tmux]
	if needs {
		paneNeedsInput[tmux] = true
	} else {
		delete(paneNeedsInput, tmux)
	}
	if !had {
		return needs
	}
	return prev != needs
}

func paneNeedsInputPrune(live map[string]bool) {
	paneNeedsInputMu.Lock()
	defer paneNeedsInputMu.Unlock()
	for k := range paneNeedsInput {
		if !live[k] {
			delete(paneNeedsInput, k)
		}
	}
}

// fnv64 hashes a string without pulling in the hash/fnv package. Cheap
// non-cryptographic fingerprint — collisions are harmless here, they'd
// only mask one tick of activity.
func fnv64(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

var globalConfigPath string

var tmuxPasteSeq atomic.Uint64

func tmuxPasteBuf() string {
	return fmt.Sprintf("orch-%d-%d", time.Now().UnixNano(), tmuxPasteSeq.Add(1))
}

func tmuxPaste(vm VMBlock, session, msg string) error {
	buf := tmuxPasteBuf()
	if _, errStr, err := sshExecIn(vm, msg, fmt.Sprintf("tmux load-buffer -b %s -", buf)); err != nil {
		return fmt.Errorf("load-buffer: %v: %s", err, errStr)
	}
	cmd := fmt.Sprintf("tmux paste-buffer -b %s -t %s -d; status=$?; tmux delete-buffer -b %s 2>/dev/null || true; [ $status -eq 0 ] || exit $status; sleep 1; tmux send-keys -t %s C-m", buf, session, buf, session)
	if _, errStr, err := sshExec(vm, cmd); err != nil {
		return fmt.Errorf("paste-buffer+enter: %v: %s", err, errStr)
	}
	return nil
}

// sessionName picks a tmux session id that reflects the agent running in
// the pane. Empty agent falls back to "claude" for back-compat.
func sessionName(issue int, agent string) string {
	if agent == "" {
		agent = "claude"
	}
	return fmt.Sprintf("%s-%d", agent, issue)
}

func freeVM(cfg *Config, st *State) *VMBlock {
	if len(cfg.VMs) == 0 {
		return nil
	}
	load := map[string]int{}
	for i := range cfg.VMs {
		load[cfg.VMs[i].Name] = 0
	}
	for _, j := range st.Jobs {
		load[j.VM]++
	}
	idx := make([]int, 0, len(cfg.VMs))
	for i := range cfg.VMs {
		vm := &cfg.VMs[i]
		if vm.Capacity > 0 && load[vm.Name] >= vm.Capacity {
			continue
		}
		idx = append(idx, i)
	}
	if len(idx) == 0 {
		return nil
	}
	sort.Slice(idx, func(a, b int) bool {
		na, nb := cfg.VMs[idx[a]].Name, cfg.VMs[idx[b]].Name
		if load[na] != load[nb] {
			return load[na] < load[nb]
		}
		return na < nb
	})
	return &cfg.VMs[idx[0]]
}

func vmByName(cfg *Config, name string) *VMBlock {
	for i := range cfg.VMs {
		if cfg.VMs[i].Name == name {
			return &cfg.VMs[i]
		}
	}
	return nil
}

// agentSpec describes the per-agent quirks orch needs to drive a worker
// session: how to detect the TUI's idle/busy state by capturing the pane,
// and how to transform a fresh start command into a resume command.
type agentSpec struct {
	name          string
	idleMarker    string
	busyMarker    string
	promptMarkers []string
	resumeXform   func(sessionCmd string) string
}

// agentSpecs are the built-in defaults; per-VM idle_marker/busy_marker can override.
var agentSpecs = map[string]agentSpec{
	"claude": {
		name:          "claude",
		idleMarker:    "bypass permissions",
		busyMarker:    "esc to interrupt",
		promptMarkers: []string{"Esc to cancel"},
		resumeXform: func(s string) string {
			return strings.Replace(s,
				"claude --dangerously-skip-permissions",
				"claude --dangerously-skip-permissions --resume", 1)
		},
	},
	"codex": {
		name:       "codex",
		idleMarker: "gpt-",
		busyMarker: "esc to interrupt",
		resumeXform: func(s string) string {
			if strings.Contains(s, "exec codex") {
				return strings.Replace(s, "exec codex", "exec codex resume --last", 1)
			}
			if i := strings.Index(s, "/bin/codex"); i >= 0 {
				j := i + len("/bin/codex")
				return s[:j] + " resume --last" + s[j:]
			}
			return s
		},
	},
}

// vmAgent returns the agent spec for vm, applying per-VM marker overrides
// on top of the agent's built-in defaults. Unknown agent name falls back
// to claude.
func vmAgent(vm VMBlock) agentSpec {
	name := vm.Agent
	if name == "" {
		name = "claude"
	}
	spec, ok := agentSpecs[name]
	if !ok {
		log.Printf("vm %q: unknown agent %q, falling back to claude", vm.Name, name)
		spec = agentSpecs["claude"]
	}
	if vm.IdleMarker != "" {
		spec.idleMarker = vm.IdleMarker
	}
	if vm.BusyMarker != "" {
		spec.busyMarker = vm.BusyMarker
	}
	return spec
}

// vmBotIdentity resolves the git user.name / user.email used for commits in
// sessions on this VM. Per-VM bot_login/bot_email override the orchestrator
// defaults; bot_email falls back to <bot_login>@users.noreply.github.com.
func vmBotIdentity(orch OrchBlock, vm VMBlock) (login, email string) {
	login = vm.BotLogin
	if login == "" {
		login = orch.BotLogin
	}
	email = vm.BotEmail
	if email == "" {
		email = orch.BotEmail
	}
	if email == "" && login != "" {
		email = login + "@users.noreply.github.com"
	}
	return
}

var includePattern = regexp.MustCompile(`\[(prompt|skill):([^\]]+)\]`)

func resolveIncludeAPI(kind, ref, inboxRepo string) (string, error) {
	if strings.HasPrefix(ref, "https://github.com/") {
		u := strings.TrimPrefix(ref, "https://github.com/")
		parts := strings.SplitN(u, "/", 5)
		if len(parts) < 5 || parts[2] != "blob" {
			return "", fmt.Errorf("malformed github URL: %s", ref)
		}
		owner, repo, gitRef, path := parts[0], parts[1], parts[3], parts[4]
		return fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s", owner, repo, path, gitRef), nil
	}
	return fmt.Sprintf("repos/%s/contents/%ss/%s", inboxRepo, kind, ref), nil
}

// fetchInclude pulls a single include's content from GitHub via `gh api`,
// decoding the base64 contents response.
func fetchInclude(kind, ref, inboxRepo string) (string, error) {
	apiPath, err := resolveIncludeAPI(kind, ref, inboxRepo)
	if err != nil {
		return "", err
	}
	out, errStr, err := run("gh", "api", apiPath, "--jq", ".content")
	if err != nil {
		return "", fmt.Errorf("gh api %s: %v: %s", apiPath, err, strings.TrimSpace(errStr))
	}
	raw := strings.ReplaceAll(strings.TrimSpace(out), "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	return string(decoded), nil
}

func expandIncludes(text, inboxRepo string) string {
	return includePattern.ReplaceAllStringFunc(text, func(match string) string {
		m := includePattern.FindStringSubmatch(match)
		kind, ref := m[1], m[2]
		content, err := fetchInclude(kind, ref, inboxRepo)
		if err != nil {
			log.Printf("include %s: %v", match, err)
			return fmt.Sprintf("<!-- include failed: %s: %v -->", match, err)
		}
		return content
	})
}

func renderBootstrap(tmpl string, is Issue, branch, targetName, targetRepo, inboxRepo, workdir, schedule string) string {
	return strings.NewReplacer(
		"{{issue.number}}", fmt.Sprint(is.Number),
		"{{issue.title}}", is.Title,
		"{{issue.body}}", is.Body,
		"{{branch}}", branch,
		"{{target.name}}", targetName,
		"{{target.repo}}", targetRepo,
		"{{inbox.repo}}", inboxRepo,
		"{{workdir}}", workdir,
		"{{schedule}}", schedule,
	).Replace(tmpl)
}

func loadState(dbPath string) (*State, error) {
	store, err := openStore(dbPath)
	if err != nil {
		return nil, err
	}
	legacyState := filepath.Join(filepath.Dir(dbPath), "state.json")
	legacySnap := filepath.Join(filepath.Dir(dbPath), "snap.json")
	if err := store.migrateLegacyJSON(legacyState, legacySnap); err != nil {
		_ = store.Close()
		return nil, err
	}
	jobs, cursor, maint, err := store.LoadState()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	s := &State{
		Jobs:          jobs,
		MentionCursor: cursor,
		Maintainers:   maint,
		store:         store,
		Bcast:         make(chan struct{}, 1),
	}
	return s, nil
}

// saveState writes the in-memory State back to sqlite and refreshes the
// lock-free HTTP snapshot. Caller must hold s.mu.
func saveState(s *State) error {
	if err := s.store.SaveState(s.Jobs, s.MentionCursor, s.Maintainers); err != nil {
		return err
	}
	snap := make(map[int]Job, len(s.Jobs))
	for n, j := range s.Jobs {
		snap[n] = *j
	}
	s.httpSnap.Store(snap)
	if s.Bcast != nil {
		select {
		case s.Bcast <- struct{}{}:
		default:
		}
	}
	return nil
}

func tearDown(cfg *Config, st *State, issue int) {
	j := st.Jobs[issue]
	if j == nil {
		return
	}
	vm := vmByName(cfg, j.VM)
	if vm != nil {
		tmuxKill(*vm, j.Tmux)
		pruneWorkdir(*vm, vmWorkdirRoot(cfg.Orch, *vm), issue)
	}
	delete(st.Jobs, issue)
	log.Printf("issue #%d: torn down (was on %s/%s)", issue, j.VM, j.Tmux)
}

// pruneWorkdir removes the per-issue workdir (git worktree + build artifacts).
// Called on teardown and periodically for orphans.
func pruneWorkdir(vm VMBlock, root string, issue int) {
	root = strings.TrimRight(root, "/")
	dir := fmt.Sprintf("%s/issue-%d", root, issue)
	pruneCmd := fmt.Sprintf(
		"cd %s/issue-%d 2>/dev/null && git worktree remove --force %s/issue-%d 2>/dev/null || true; rm -rf %s/issue-%d",
		root, issue, root, issue, root, issue,
	)
	if _, _, err := sshExec(vm, pruneCmd); err != nil {
		log.Printf("prune workdir %s: %v", dir, err)
	} else {
		log.Printf("pruned workdir %s", dir)
	}
}

// pruneOrphanWorkdirs removes workdirs for issues no longer in active state.
// Runs periodically so long-dead workdirs don't fill the disk.
func pruneOrphanWorkdirs(cfg *Config, st *State) {
	st.mu.Lock()
	active := make(map[int]string)
	for n, j := range st.Jobs {
		active[n] = j.VM
	}
	st.mu.Unlock()

	for _, vm := range cfg.VMs {
		root := vmWorkdirRoot(cfg.Orch, vm)
		out, _, err := sshExec(vm, fmt.Sprintf("ls -d %s/issue-* 2>/dev/null", root))
		if err != nil || out == "" {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.Split(line, "/")
			base := parts[len(parts)-1]
			if !strings.HasPrefix(base, "issue-") {
				continue
			}
			n, err := strconv.Atoi(strings.TrimPrefix(base, "issue-"))
			if err != nil {
				continue
			}
			if _, ok := active[n]; ok {
				continue
			}
			log.Printf("pruning orphan workdir %s on %s", line, vm.Name)
			pruneWorkdir(vm, root, n)
		}
	}
}

func mergeableTransition(prev, cur string) string {
	if cur == "" || cur == "UNKNOWN" {
		return ""
	}
	if prev == cur {
		return ""
	}
	if prev == "" && cur == "MERGEABLE" {
		return ""
	}
	return cur
}

func isActionableCheck(conclusion string) bool {
	switch conclusion {
	case "SUCCESS", "NEUTRAL", "SKIPPED", "STALE":
		return false
	}
	return true
}

func diffPR(j *Job, v *PRView, botLogin string) (
	visibleReviews, visibleThread, visibleIssue []string,
	silentReviews, silentThread, silentIssue []string,
	pushed bool, checkChanges []string, mergeable string,
) {
	seen := func(ids []string) map[string]bool {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	isBot := func(login string) bool {
		return botLogin != "" && login == botLogin
	}
	rs := seen(j.SeenReviewIDs)
	for _, r := range v.Reviews {
		if rs[r.ID] {
			continue
		}
		if isBot(r.Author.Login) {
			silentReviews = append(silentReviews, r.ID)
		} else {
			visibleReviews = append(visibleReviews, r.ID)
		}
	}
	tc := seen(j.SeenThreadCommentIDs)
	for _, t := range v.ReviewThreads {
		for _, c := range t.Comments {
			if tc[c.ID] {
				continue
			}
			if isBot(c.Author.Login) {
				silentThread = append(silentThread, c.ID)
			} else {
				visibleThread = append(visibleThread, c.ID)
			}
		}
	}
	ic := seen(j.SeenIssueCommentIDs)
	for _, c := range v.Comments {
		if ic[c.ID] {
			continue
		}
		if isBot(c.Author.Login) {
			silentIssue = append(silentIssue, c.ID)
		} else {
			visibleIssue = append(visibleIssue, c.ID)
		}
	}
	if j.LastHeadOID != "" && j.LastHeadOID != v.HeadRefOid {
		pushed = true
		if botLogin != "" {
			for _, c := range v.Commits {
				if c.Oid != v.HeadRefOid || len(c.Authors) == 0 {
					continue
				}
				pushed = false
				for _, a := range c.Authors {
					if a.Login != botLogin {
						pushed = true
						break
					}
				}
				break
			}
		}
	}
	latest := map[string]string{}
	latestAt := map[string]string{}
	for _, c := range v.StatusCheckRollup {
		if c.Status != "COMPLETED" {
			continue
		}
		if c.CompletedAt > latestAt[c.Name] {
			latestAt[c.Name] = c.CompletedAt
			latest[c.Name] = c.Conclusion
		}
	}
	prev := j.LastCheckConclusions
	for name, conclusion := range latest {
		if prev[name] != conclusion && isActionableCheck(conclusion) {
			checkChanges = append(checkChanges, fmt.Sprintf("%s: %s", name, conclusion))
		}
	}
	mergeable = mergeableTransition(j.LastMergeable, v.Mergeable)
	return
}

func ntfyNotify(topic, title, msg, clickURL string) {
	if topic == "" {
		return
	}
	req, err := http.NewRequest("POST", "https://ntfy.sh/"+topic, strings.NewReader(msg))
	if err != nil {
		return
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", "default")
	if clickURL != "" {
		req.Header.Set("Click", clickURL)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("ntfy: %v", err)
		return
	}
	resp.Body.Close()
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

func summarize(v *PRView, nr, ntc, nic []string, pushed bool, checks []string, mergeable string) string {
	var b strings.Builder
	b.WriteString("PR update from orchestrator:\n\n")
	for _, id := range nr {
		for _, r := range v.Reviews {
			if r.ID == id {
				b.WriteString(fmt.Sprintf("- New review by @%s [%s]: %s\n", r.Author.Login, r.State, oneLine(r.Body, 200)))
			}
		}
	}
	for _, id := range ntc {
		for _, t := range v.ReviewThreads {
			for _, c := range t.Comments {
				if c.ID == id {
					b.WriteString(fmt.Sprintf("- New review comment by @%s on %s:%d: %s\n", c.Author.Login, t.Path, t.Line, oneLine(c.Body, 200)))
				}
			}
		}
	}
	for _, id := range nic {
		for _, c := range v.Comments {
			if c.ID == id {
				b.WriteString(fmt.Sprintf("- New PR comment by @%s: %s\n", c.Author.Login, oneLine(c.Body, 200)))
			}
		}
	}
	if pushed {
		head := v.HeadRefOid
		if len(head) > 8 {
			head = head[:8]
		}
		b.WriteString(fmt.Sprintf("- New commits pushed to PR (head=%s)\n", head))
	}
	if len(checks) > 0 {
		b.WriteString(fmt.Sprintf("- CI status changes: %s\n", strings.Join(checks, ", ")))
	}
	switch mergeable {
	case "CONFLICTING":
		b.WriteString("- PR now CONFLICTS with the base branch. Resolve it before continuing:\n")
		b.WriteString("    git fetch origin && git rebase origin/<base>   # or `git merge origin/<base>`\n")
		b.WriteString("    # resolve conflicts, then:\n")
		b.WriteString("    git add -A && git rebase --continue            # or `git commit` for merge\n")
		b.WriteString("    git push --force-with-lease                    # rebase only; plain push for merge\n")
		b.WriteString("  Replace <base> with the PR's base branch (usually main). Do not skip this — CI and review are blocked until the conflict clears.\n")
	case "MERGEABLE":
		b.WriteString("- PR conflicts resolved — mergeable again. No action required for this item.\n")
	}
	b.WriteString("\nAddress these, push fixes if needed, then stop and wait for the next message.")
	return b.String()
}

// startSession does the workdir + tmux + bootstrap-paste dance for one
// session. It does NOT touch State.Jobs — the caller decides whether this
// is a fresh oneshot job or a recurring cron tick.
