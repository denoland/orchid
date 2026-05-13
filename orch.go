package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

type Config struct {
	GitHub              GitHubBlock   `hcl:"github,block"`
	Orch                OrchBlock     `hcl:"orchestrator,block"`
	BootstrapPrompt     string        `hcl:"bootstrap_prompt"`
	CronBootstrapPrompt string        `hcl:"cron_bootstrap_prompt,optional"` // template for cron-lifecycle issues; required if any inbox issue carries the `cron` label
	Targets             []TargetBlock `hcl:"target,block"`
	VMs                 []VMBlock     `hcl:"vm,block"`
}

type GitHubBlock struct {
	InboxRepo string `hcl:"inbox_repo"`
}

type TargetBlock struct {
	Name  string `hcl:",label"`
	Label string `hcl:"label"`
	Repo  string `hcl:"repo"`
}

type OrchBlock struct {
	PollInterval string `hcl:"poll_interval"`
	StateFile    string `hcl:"state_file"`
	BranchPrefix string `hcl:"branch_prefix"`
	WorkdirRoot  string `hcl:"workdir_root"`
	HTTPAddr     string `hcl:"http_addr,optional"`
	HTTPSecret   string `hcl:"http_secret,optional"` // bearer token for dashboard; empty = no auth
	BotLogin     string `hcl:"bot_login,optional"`   // default git user.name; per-VM override available
	BotEmail     string `hcl:"bot_email,optional"`   // default git user.email; falls back to <bot_login>@users.noreply.github.com
	NtfyTopic    string `hcl:"ntfy_topic,optional"`
}

type VMBlock struct {
	Name        string `hcl:",label"`
	Host        string `hcl:"host"`
	User        string `hcl:"user,optional"`
	Key         string `hcl:"key,optional"`      // not needed for localhost
	Capacity    int    `hcl:"capacity,optional"` // 0 = unlimited
	Sccache     bool   `hcl:"sccache,optional"`
	SccacheDir  string `hcl:"sccache_dir,optional"`  // default ~/.cache/sccache
	SessionCmd  string `hcl:"session_cmd,optional"`  // default: clawpatrol run -- claude --dangerously-skip-permissions
	SessionHome string `hcl:"session_home,optional"` // home dir of user running the session (for trust stamp)
	BotLogin    string `hcl:"bot_login,optional"`    // overrides orchestrator.bot_login for sessions on this VM
	BotEmail    string `hcl:"bot_email,optional"`    // overrides orchestrator.bot_email for sessions on this VM
}

// Job lifecycle: "oneshot" (default) — issue → session → PR → teardown.
// "cron" — issue stays open, ephemeral session fires every Schedule, no PR.
type Job struct {
	VM                   string            `json:"vm"`
	Tmux                 string            `json:"tmux"`
	Target               string            `json:"target"`      // target block name
	TargetRepo           string            `json:"target_repo"` // resolved (e.g. denoland/deno)
	Branch               string            `json:"branch"`
	Lifecycle            string            `json:"lifecycle,omitempty"`       // "oneshot" (default) or "cron"
	Schedule             string            `json:"schedule,omitempty"`        // cron only: parseable by time.ParseDuration
	Timeout              string            `json:"timeout,omitempty"`         // cron only: max runtime per tick before orch kills the pane
	NextFireAt           time.Time         `json:"next_fire_at,omitempty"`    // cron only: when to spawn the next ephemeral tick
	FireStartedAt        time.Time         `json:"fire_started_at,omitempty"` // cron only: when the current tick started (used to enforce Timeout)
	IssueTitle           string            `json:"issue_title,omitempty"`
	PR                   int               `json:"pr,omitempty"`
	SeenReviewIDs        []string          `json:"seen_review_ids,omitempty"`
	SeenThreadCommentIDs []string          `json:"seen_thread_comment_ids,omitempty"`
	SeenIssueCommentIDs  []string          `json:"seen_issue_comment_ids,omitempty"`
	LastHeadOID          string            `json:"last_head_oid,omitempty"`
	LastCheckConclusions map[string]string `json:"last_check_conclusions,omitempty"`
}

type State struct {
	mu       sync.Mutex
	Jobs     map[int]*Job `json:"jobs"`
	httpSnap atomic.Value // stores map[int]Job; refreshed at tick start, lock-free reads
}

// retry wraps an exec.Command-style call with bounded retries on non-zero
// exit. clawpatrol's MITM proxy is known to drop connections sporadically
// (gh: "error connecting to api.github.com", ssh: exit 255); this hides
// those blips so a single tick doesn't lose work. Backoff: 1s, 2s, 4s.
const runAttempts = 4

// maxKillsPerTick caps how many dead-session respawns the polling loop will
// fire in a single tick. Raised from 2 to 5 after removing the clawpatrol WG
// relay dependency (the original cap was to avoid overwhelming the relay with
// simultaneous peer registrations).
const maxKillsPerTick = 5

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

func run(name string, args ...string) (string, string, error) {
	return runWithStdin(nil, name, args...)
}

func runIn(stdin string, name string, args ...string) (string, string, error) {
	return runWithStdin(strings.NewReader(stdin), name, args...)
}

func runWithStdin(stdin *strings.Reader, name string, args ...string) (string, string, error) {
	var lastOut, lastErr string
	var lastE error
	for i := 0; i < runAttempts; i++ {
		cmd := exec.Command(name, args...)
		if stdin != nil {
			_, _ = stdin.Seek(0, 0)
			cmd.Stdin = stdin
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
		return true // ssh connection failure
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

// sshExecIn runs a shell command on the VM with stdin. For localhost, skips SSH
// and runs the command directly (splits remote into argv).
func sshExecIn(vm VMBlock, stdin, remote string) (string, string, error) {
	if isLocal(vm) {
		parts := strings.Fields(remote)
		if len(parts) == 0 {
			return "", "", fmt.Errorf("empty command")
		}
		return runIn(stdin, parts[0], parts[1:]...)
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

func ghIssueList(repo, label string) ([]Issue, error) {
	out, errStr, err := run("gh", "issue", "list",
		"--repo", repo, "--label", label, "--state", "open",
		"--limit", "200", "--json", "number,title,body,state,labels")
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %v: %s", err, errStr)
	}
	// gh returns labels as [{name, ...}, ...]; flatten to []string.
	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, err
	}
	issues := make([]Issue, 0, len(raw))
	for _, r := range raw {
		is := Issue{Number: r.Number, Title: r.Title, Body: r.Body, State: r.State}
		for _, l := range r.Labels {
			is.Labels = append(is.Labels, l.Name)
		}
		issues = append(issues, is)
	}
	return issues, nil
}

// hasLabel returns true if the issue carries the given label name.
func (is Issue) hasLabel(name string) bool {
	for _, l := range is.Labels {
		if l == name {
			return true
		}
	}
	return false
}

// CronConfig holds parsed cron parameters from an issue's toml frontmatter.
type CronConfig struct {
	Schedule    time.Duration
	ScheduleStr string
	// Timeout bounds a single tick — if the claude session is still alive
	// after this much time, orch kills the pane. Defaults to Schedule/2
	// when not explicitly set, so there's always slack before the next
	// fire is due.
	Timeout    time.Duration
	TimeoutStr string
}

// parseCronFrontmatter extracts cron parameters from a fenced toml block
// at the top of an issue body. Returns nil when no valid frontmatter is
// present (no fence, no schedule key, or schedule unparseable).
//
// Recognized shape:
//
//	```toml
//	schedule = "30m"
//	timeout  = "5m"   # optional, default = schedule / 2
//	... (other keys ignored)
//	```
//
// Anything before the opening fence (e.g. blank lines) is allowed.
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

func ghIssueIsOpen(repo string, n int) (bool, error) {
	out, errStr, err := run("gh", "issue", "view", fmt.Sprint(n),
		"--repo", repo, "--json", "state")
	if err != nil {
		return false, fmt.Errorf("gh issue view: %v: %s", err, errStr)
	}
	var s struct{ State string }
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		return false, err
	}
	return s.State == "OPEN", nil
}

type PRSummary struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

// ghFindPRByBranch looks up an existing PR for (repo, branch). If author is
// non-empty, the search is restricted to PRs opened by that GitHub user —
// without this filter, two orch instances sharing a branch_prefix can
// spuriously match each other's PRs in the same target repo.
func ghFindPRByBranch(repo, branch, author string) (*PRSummary, error) {
	args := []string{"pr", "list",
		"--repo", repo, "--head", branch, "--state", "all",
		"--limit", "5", "--json", "number,state"}
	if author != "" {
		args = append(args, "--author", author)
	}
	out, errStr, err := run("gh", args...)
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %v: %s", err, errStr)
	}
	var prs []PRSummary
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	for _, p := range prs {
		if p.State == "OPEN" {
			return &p, nil
		}
	}
	return &prs[0], nil
}

// ghBranchAhead returns true if branch exists on remote and has at least one
// commit ahead of the base branch (main). Returns false (not error) if the
// branch doesn't exist yet.
func ghBranchAhead(repo, branch string) (bool, error) {
	out, errStr, err := run("gh", "api",
		fmt.Sprintf("repos/%s/compare/main...%s", repo, branch),
		"--jq", ".ahead_by")
	if err != nil {
		if strings.Contains(errStr, "No commit found") || strings.Contains(errStr, "Not Found") {
			return false, nil
		}
		return false, fmt.Errorf("gh api compare: %v: %s", err, errStr)
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n > 0, nil
}

// ghAutoCreatePR checks if the job's branch has been pushed with commits, and
// if so creates a PR from the orchestrator (bypassing the clawpatrol MITM proxy
// which blocks api.github.com credential injection for worker sessions).
// Returns the new PR number, or 0 if the branch has no commits yet.
func ghAutoCreatePR(cfg *Config, n int, j *Job, is Issue) (int, error) {
	ahead, err := ghBranchAhead(j.TargetRepo, j.Branch)
	if err != nil {
		return 0, err
	}
	if !ahead {
		return 0, nil
	}
	body := fmt.Sprintf("Closes %s#%d", cfg.GitHub.InboxRepo, n)
	out, errStr, err := run("gh", "pr", "create",
		"--repo", j.TargetRepo,
		"--head", j.Branch,
		"--base", "main",
		"--title", is.Title,
		"--body", body)
	if err != nil {
		return 0, fmt.Errorf("gh pr create: %v: %s", err, errStr)
	}
	// Output is the PR URL: https://github.com/owner/repo/pull/123
	u := strings.TrimSpace(out)
	parts := strings.Split(u, "/")
	num, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0, fmt.Errorf("parse PR number from %q: %w", u, err)
	}
	log.Printf("issue #%d: auto-created PR #%d (%s)", n, num, u)
	return num, nil
}

type PRView struct {
	State      string `json:"state"`
	HeadRefOid string `json:"headRefOid"`
	Reviews    []struct {
		ID     string                 `json:"id"`
		Author struct{ Login string } `json:"author"`
		State  string                 `json:"state"`
		Body   string                 `json:"body"`
	} `json:"reviews"`
	ReviewThreads []struct {
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Comments []struct {
			ID     string                 `json:"id"`
			Author struct{ Login string } `json:"author"`
			Body   string                 `json:"body"`
		} `json:"comments"`
	} `json:"reviewThreads"`
	Comments []struct {
		ID     string                 `json:"id"`
		Author struct{ Login string } `json:"author"`
		Body   string                 `json:"body"`
	} `json:"comments"`
	StatusCheckRollup []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"statusCheckRollup"`
}

func ghPRView(repo string, n int) (*PRView, error) {
	out, errStr, err := run("gh", "pr", "view", fmt.Sprint(n),
		"--repo", repo,
		"--json", "state,headRefOid,reviews,comments,statusCheckRollup")
	if err != nil {
		return nil, fmt.Errorf("gh pr view: %v: %s", err, errStr)
	}
	var v PRView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, err
	}
	return &v, nil
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

// bootstrapVM provisions outbound github auth on the VM: copies the local
// ssh key (the same one orch uses to reach the VM, which we assume also
// authorizes github access for the bot account) and primes known_hosts.
// For localhost VMs, key copy is skipped — keys are already present.
// Idempotent — safe to call on every orch start.
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

	// common: claude settings + optional sccache + github auth check
	commonScript := fmt.Sprintf(`set -e
mkdir -p ~/.claude
if [ -f ~/.claude/settings.json ]; then
  jq '. + {skipDangerousModePermissionPrompt: true, includeCoAuthoredBy: false}' ~/.claude/settings.json > ~/.claude/settings.json.tmp && mv ~/.claude/settings.json.tmp ~/.claude/settings.json
else
  echo '{"theme":"dark","skipDangerousModePermissionPrompt":true,"includeCoAuthoredBy":false}' > ~/.claude/settings.json
fi
[ -f ~/.claude.json ] || echo '{}' > ~/.claude.json
%s
ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -T git@github.com 2>&1 | head -1
`, sccacheSetup)

	var out, errStr string
	var err error

	if isLocal(vm) {
		out, errStr, err = runIn(commonScript, "bash", "-s")
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
		// Idempotent: only install ~/.ssh/id_ed25519 if it isn't already
		// present. A pre-existing key on the worker (e.g. when one orch
		// drives multiple worker VMs that each have their own bot
		// identity registered with GitHub) is left alone — overwriting
		// it would break that VM's `git push`.
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
	if !strings.Contains(out, "successfully authenticated") {
		return fmt.Errorf("github ssh auth check unexpected: %q", strings.TrimSpace(out))
	}
	return nil
}

// tmuxStart prepares a per-issue working tree on the VM and launches an
// interactive clawpatrol-wrapped claude session in tmux. Layout per VM:
//
//	<workdir_root>/repos/<owner-repo>/   single shared clone per (vm, repo)
//	<workdir_root>/issue-<N>/            git worktree off the shared clone
//
// Worktrees share .git/objects with the shared clone, so adding one is fast
// and disk-cheap — no full reclone per issue. Whole setup runs as one bash
// script piped over ssh stdin to dodge nested quoting.
func tmuxStart(vm VMBlock, session, workdir, sharedDir, repo, branch, sessionCmdOverride, botLogin, botEmail string) error {
	sessionCmd := sessionCmdOverride
	if sessionCmd == "" {
		sessionCmd = vm.SessionCmd
	}
	if sessionCmd == "" {
		sessionCmd = "clawpatrol run -- claude --dangerously-skip-permissions"
	}
	// SessionHome is only needed when the session runs as a different user
	// from the orch process (e.g. via runuser). When unset, the bootstrap
	// script just stamps $HOME and skips the second stamp.
	sessionHome := vm.SessionHome
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

# 1) shared clone (once per repo per VM); always fetch fresh refs
if [ ! -d "$SHARED/.git" ]; then
  mkdir -p "$(dirname "$SHARED")"
  git clone "git@github.com:$REPO.git" "$SHARED"
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

# 4) pre-stamp claude's per-folder trust flag so the TUI doesn't prompt.
# Stamp $HOME (the user running this script) and SESSION_HOME if it's set
# to something different (i.e. session runs as another user via runuser).
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

# 5) launch the pane
tmux kill-session -t "$SESSION" 2>/dev/null || true
tmux new-session -d -c "$WORKDIR" -s "$SESSION" "$SESSION_CMD"
`, sharedDir, repo, workdir, branch, session, sessionCmd, sessionHome, botLogin, botEmail)

	_, errStr, err := sshExecIn(vm, script, "bash -s")
	if err != nil {
		return fmt.Errorf("tmux start: %v: %s", err, errStr)
	}
	return nil
}

func tmuxKill(vm VMBlock, session string) {
	_, _, _ = sshExec(vm, fmt.Sprintf("tmux kill-session -t %s 2>/dev/null", session))
}

// tmuxIdle is a heuristic for "claude TUI is at its input prompt and not
// processing". The status bar "bypass permissions" line is always rendered
// once the TUI is up; "esc to interrupt" is appended only while claude is
// working. False negatives just defer the poke by one tick — safe.
//
// We capture the entire visible pane (not `tail -N`) because claude's welcome
// screen leaves trailing blank rows below the footer; a small tail window
// would miss the "bypass permissions" line and falsely report not-idle.
func tmuxIdle(vm VMBlock, session string) (bool, error) {
	out, _, err := sshExec(vm, fmt.Sprintf("tmux capture-pane -p -t %s", session))
	if err != nil {
		return false, err
	}
	if !strings.Contains(out, "bypass permissions") {
		return false, nil
	}
	return !strings.Contains(out, "esc to interrupt"), nil
}

func tmuxPaste(vm VMBlock, session, msg string) error {
	if _, errStr, err := sshExecIn(vm, msg, "tmux load-buffer -b orch -"); err != nil {
		return fmt.Errorf("load-buffer: %v: %s", err, errStr)
	}
	if _, errStr, err := sshExec(vm, fmt.Sprintf("tmux paste-buffer -b orch -t %s -d", session)); err != nil {
		return fmt.Errorf("paste-buffer: %v: %s", err, errStr)
	}
	if _, errStr, err := sshExec(vm, fmt.Sprintf("tmux send-keys -t %s Enter", session)); err != nil {
		return fmt.Errorf("send-keys: %v: %s", err, errStr)
	}
	return nil
}

func sessionName(issue int) string { return fmt.Sprintf("claude-%d", issue) }

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
			continue // at cap
		}
		idx = append(idx, i)
	}
	if len(idx) == 0 {
		return nil
	}
	// pick least-loaded VM; break ties by name for determinism
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

// includePattern matches `[prompt:<ref>]` and `[skill:<ref>]` inclusions
// anywhere in the rendered bootstrap message. The reference can be a
// relative filename (resolved as `<type>s/<filename>` in the inbox repo,
// note the plural) or an absolute GitHub blob URL pointing at any repo.
var includePattern = regexp.MustCompile(`\[(prompt|skill):([^\]]+)\]`)

// resolveIncludeAPI returns the gh-api path for fetching one include. It's
// the pure-logic part of expandIncludes, factored out so it's testable
// without hitting GitHub.
//
// GitHub URLs of the form https://github.com/<owner>/<repo>/blob/<ref>/<path>
// are split naively at slashes — branch names containing `/` cannot be
// disambiguated from the path component without asking the server, so use
// a single-segment ref (e.g. `main`, `master`, a tag) or a commit SHA.
func resolveIncludeAPI(kind, ref, inboxRepo string) (string, error) {
	if strings.HasPrefix(ref, "https://github.com/") {
		u := strings.TrimPrefix(ref, "https://github.com/")
		// owner / repo / "blob" / ref / path...
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
	// GitHub returns base64 with embedded newlines; strip them before decoding.
	raw := strings.ReplaceAll(strings.TrimSpace(out), "\n", "")
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	return string(decoded), nil
}

// expandIncludes substitutes `[prompt:foo.md]` / `[skill:foo.md]` references
// in the rendered bootstrap message with the contents of the referenced
// file fetched from GitHub. Non-matching text is left alone.
//
// Failures are logged and replaced with an HTML-comment marker so the
// operator can spot them in the pane (or orch.log) without the entire
// spawn aborting on a single bad reference.
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

func loadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &State{Jobs: map[int]*Job{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Jobs == nil {
		s.Jobs = map[int]*Job{}
	}
	return &s, nil
}

func saveState(path string, s *State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	// refresh HTTP snapshot after every state write (caller holds s.mu)
	snap := make(map[int]Job, len(s.Jobs))
	for n, j := range s.Jobs {
		snap[n] = *j
	}
	s.httpSnap.Store(snap)
	return nil
}

func tearDown(cfg *Config, st *State, issue int) {
	j := st.Jobs[issue]
	if j == nil {
		return
	}
	if vm := vmByName(cfg, j.VM); vm != nil {
		tmuxKill(*vm, j.Tmux)
	}
	delete(st.Jobs, issue)
	log.Printf("issue #%d: torn down (was on %s/%s)", issue, j.VM, j.Tmux)
}

func diffPR(j *Job, v *PRView) (newReviews, newThreadComments, newIssueComments []string, pushed bool, checkChanges []string) {
	seen := func(ids []string) map[string]bool {
		m := map[string]bool{}
		for _, id := range ids {
			m[id] = true
		}
		return m
	}
	rs := seen(j.SeenReviewIDs)
	for _, r := range v.Reviews {
		if !rs[r.ID] {
			newReviews = append(newReviews, r.ID)
		}
	}
	tc := seen(j.SeenThreadCommentIDs)
	for _, t := range v.ReviewThreads {
		for _, c := range t.Comments {
			if !tc[c.ID] {
				newThreadComments = append(newThreadComments, c.ID)
			}
		}
	}
	ic := seen(j.SeenIssueCommentIDs)
	for _, c := range v.Comments {
		if !ic[c.ID] {
			newIssueComments = append(newIssueComments, c.ID)
		}
	}
	if j.LastHeadOID != "" && j.LastHeadOID != v.HeadRefOid {
		pushed = true
	}
	prev := j.LastCheckConclusions
	for _, c := range v.StatusCheckRollup {
		if c.Status != "COMPLETED" {
			continue
		}
		if prev[c.Name] != c.Conclusion {
			checkChanges = append(checkChanges, fmt.Sprintf("%s: %s", c.Name, c.Conclusion))
		}
	}
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

func summarize(v *PRView, nr, ntc, nic []string, pushed bool, checks []string) string {
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
	b.WriteString("\nAddress these, push fixes if needed, then stop and wait for the next message.")
	return b.String()
}

// startSession does the workdir + tmux + bootstrap-paste dance for one
// session. It does NOT touch State.Jobs — the caller decides whether this
// is a fresh oneshot job or a recurring cron tick.
func startSession(cfg *Config, vm *VMBlock, is Issue, target TargetBlock, lifecycle, schedule string) error {
	session := sessionName(is.Number)
	branch := cfg.Orch.BranchPrefix + fmt.Sprint(is.Number)
	root := strings.TrimRight(cfg.Orch.WorkdirRoot, "/")
	workdir := fmt.Sprintf("%s/issue-%d", root, is.Number)
	sharedDir := fmt.Sprintf("%s/repos/%s", root, strings.ReplaceAll(target.Repo, "/", "-"))
	botLogin, botEmail := vmBotIdentity(cfg.Orch, *vm)
	if err := tmuxStart(*vm, session, workdir, sharedDir, target.Repo, branch, "", botLogin, botEmail); err != nil {
		return err
	}
	// Defensive: dismiss claude's per-folder trust dialog if it appears.
	// Default is "Yes, I trust this folder" so plain Enter accepts.
	// Settings.json provisioned by bootstrapVM kills the dangerous-mode
	// warnings, so trust is the only dialog we should see at first launch.
	time.Sleep(3 * time.Second)
	_, _, _ = sshExec(*vm, fmt.Sprintf("tmux send-keys -t %s Enter", session))
	// 3 minutes covers slow claude TUI startup in heavy worktrees (e.g.
	// fresh deno checkout: lockfile parse + project scan can push first
	// idle past the 60s mark on a contended VM).
	const idleWaitTimeout = 3 * time.Minute
	deadline := time.Now().Add(idleWaitTimeout)
	sawIdle := false
	for time.Now().Before(deadline) {
		if idle, err := tmuxIdle(*vm, session); err == nil && idle {
			sawIdle = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	// If claude never reaches its idle prompt, pasting the bootstrap message
	// is useless — it lands on whatever screen is showing (login, trust
	// dialog, error). Fail with the pane tail so the operator can diagnose.
	if !sawIdle {
		pane, _, _ := sshExec(*vm, fmt.Sprintf("tmux capture-pane -p -t %s | tail -15", session))
		tmuxKill(*vm, session)
		return fmt.Errorf("session never reached idle prompt within %s (claude not authenticated?); pane tail:\n%s", idleWaitTimeout, strings.TrimSpace(pane))
	}
	// Send slash commands and wait for idle after each.
	sendSlash := func(cmd string) {
		if err := tmuxPaste(*vm, session, cmd); err != nil {
			log.Printf("issue #%d: %s failed (non-fatal): %v", is.Number, cmd, err)
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if idle, _ := tmuxIdle(*vm, session); idle {
				break
			}
			time.Sleep(1 * time.Second)
		}
	}
	sendSlash(fmt.Sprintf("/goal Implement issue #%d fully and open a PR. Do not defer work to follow-up PRs. Ship everything in one PR.", is.Number))
	sendSlash("/remote-control")

	tmpl := cfg.BootstrapPrompt
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
		VM: vm.Name, Tmux: sessionName(is.Number),
		Target: target.Name, TargetRepo: target.Repo,
		Branch: branch, Lifecycle: "oneshot",
		IssueTitle:           is.Title,
		LastCheckConclusions: map[string]string{},
	}
	log.Printf("issue #%d: spawned on %s/%s, target=%s (%s), branch=%s",
		is.Number, vm.Name, sessionName(is.Number), target.Name, target.Repo, branch)
	return nil
}

// spawnResume restarts a dead session that had an open PR, using --resume so
// claude recovers its conversation context, then pastes a short situation report.
func spawnResume(cfg *Config, st *State, vm *VMBlock, n int, j *Job) error {
	session := sessionName(n)
	root := strings.TrimRight(cfg.Orch.WorkdirRoot, "/")
	workdir := fmt.Sprintf("%s/issue-%d", root, n)
	sharedDir := fmt.Sprintf("%s/repos/%s", root, strings.ReplaceAll(j.TargetRepo, "/", "-"))

	base := vm.SessionCmd
	if base == "" {
		base = "clawpatrol run -- claude --dangerously-skip-permissions"
	}
	resumeCmd := strings.Replace(base,
		"claude --dangerously-skip-permissions",
		"claude --dangerously-skip-permissions --resume", 1)

	botLogin, botEmail := vmBotIdentity(cfg.Orch, *vm)
	if err := tmuxStart(*vm, session, workdir, sharedDir, j.TargetRepo, j.Branch, resumeCmd, botLogin, botEmail); err != nil {
		return err
	}
	// Same 3-minute window as startSession; claude --resume on a heavy
	// worktree replays the conversation and can take a while. While waiting,
	// periodically send Enter to dismiss the session-picker UI if it appears
	// (the picker renders after startup, so a single pre-sleep Enter often
	// fires too early and misses it).
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if idle, err := tmuxIdle(*vm, session); err == nil && idle {
			break
		}
		// Dismiss session picker if visible (shows "Resume session" header).
		if out, _, err := sshExec(*vm, fmt.Sprintf("tmux capture-pane -p -t %s", session)); err == nil &&
			strings.Contains(out, "Resume session") {
			_, _, _ = sshExec(*vm, fmt.Sprintf("tmux send-keys -t %s Enter", session))
		}
		time.Sleep(2 * time.Second)
	}

	sendSlash := func(cmd string) {
		if err := tmuxPaste(*vm, session, cmd); err != nil {
			log.Printf("issue #%d: %s failed (non-fatal): %v", n, cmd, err)
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if idle, _ := tmuxIdle(*vm, session); idle {
				break
			}
			time.Sleep(1 * time.Second)
		}
	}
	sendSlash(fmt.Sprintf("/goal Implement issue #%d fully and open a PR. Do not defer work to follow-up PRs. Ship everything in one PR.", n))
	sendSlash("/remote-control")

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
	// Update the existing Job's VM/Tmux pointer (vm may have changed across
	// fires if capacity shifts, though in MVP it shouldn't).
	if j := st.Jobs[is.Number]; j != nil {
		j.VM = vm.Name
		j.Tmux = sessionName(is.Number)
	}
	log.Printf("issue #%d: cron tick fired on %s/%s (schedule=%s)",
		is.Number, vm.Name, sessionName(is.Number), schedule)
	return nil
}

// tickCron is the lifecycle for cron jobs. State machine per cron Job:
//
//   - tmux session alive, within Timeout budget → claude is mid-tick; do nothing.
//   - tmux session alive, past Timeout → kill the pane (claude didn't /exit cleanly).
//   - tmux dead, now < NextFireAt → still waiting for the next fire.
//   - tmux dead, now >= NextFireAt → fire ephemeral session, update timestamps.
//
// Schedule and timeout are re-parsed from the live issue body each tick so
// an operator can change either by editing the issue.
func tickCron(cfg *Config, st *State, n int, j *Job, is Issue, target TargetBlock) {
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
		_ = saveState(cfg.Orch.StateFile, st)
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
				// Enforce per-tick timeout: claude often forgets to /exit
				// and leaves the pane idle at the prompt. Kill once the
				// budget is exhausted so the next fire can happen.
				if !j.FireStartedAt.IsZero() && now.Sub(j.FireStartedAt) > cron.Timeout {
					log.Printf("issue #%d: cron tick exceeded timeout %s, killing pane", n, cron.Timeout)
					tmuxKill(*vm, j.Tmux)
					j.Tmux = ""
					j.VM = ""
					j.FireStartedAt = time.Time{}
					_ = saveState(cfg.Orch.StateFile, st)
				}
				return
			}
			// Session is gone — claude finished or exited. Clear the
			// stale Tmux marker so the next fire-due check spawns fresh.
			j.Tmux = ""
			j.VM = ""
			j.FireStartedAt = time.Time{}
			_ = saveState(cfg.Orch.StateFile, st)
		}
	}
	if now.Before(j.NextFireAt) {
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
	// Stamp timestamps with the post-fire wall clock — fireCron takes
	// ~70s (tmux setup + idle wait), so the captured `now` from the top
	// of this function is already stale and would make the very next
	// tick believe the timeout was exceeded.
	fireDoneAt := time.Now()
	j.NextFireAt = fireDoneAt.Add(cron.Schedule)
	j.FireStartedAt = fireDoneAt
	_ = saveState(cfg.Orch.StateFile, st)
}
func tick(cfg *Config, st *State) {
	st.mu.Lock()
	defer st.mu.Unlock()
	// Publish a lock-free snapshot for the HTTP handler before doing any I/O.
	snap := make(map[int]Job, len(st.Jobs))
	for n, j := range st.Jobs {
		snap[n] = *j
	}
	st.httpSnap.Store(snap)
	// Map issue number -> (Issue, Target). First target whose label matches wins.
	type routed struct {
		is     Issue
		target TargetBlock
	}
	open := map[int]routed{}
	for _, t := range cfg.Targets {
		issues, err := ghIssueList(cfg.GitHub.InboxRepo, t.Label)
		if err != nil {
			log.Printf("list issues for target %s: %v", t.Name, err)
			continue
		}
		for _, is := range issues {
			if _, dup := open[is.Number]; dup {
				continue
			}
			open[is.Number] = routed{is: is, target: t}
		}
	}

	for n, r := range open {
		if _, exists := st.Jobs[n]; exists {
			continue
		}
		// Cron jobs: register the Job up front with NextFireAt=zero so the
		// first fire happens on the next pass through the existing-jobs
		// loop below. We don't need a free VM at registration time, only
		// at fire time.
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
			st.Jobs[n] = &Job{
				Target: r.target.Name, TargetRepo: r.target.Repo,
				Branch:     cfg.Orch.BranchPrefix + fmt.Sprint(n),
				Lifecycle:  "cron",
				Schedule:   cron.ScheduleStr,
				Timeout:    cron.TimeoutStr,
				IssueTitle: r.is.Title,
			}
			log.Printf("issue #%d: registered cron job (target=%s, schedule=%s, timeout=%s)",
				n, r.target.Name, cron.ScheduleStr, cron.TimeoutStr)
			_ = saveState(cfg.Orch.StateFile, st)
			continue
		}
		// Oneshot: spawn immediately.
		vm := freeVM(cfg, st)
		if vm == nil {
			log.Printf("issue #%d: no free VM, skipping", n)
			continue
		}
		if err := spawn(cfg, st, vm, r.is, r.target); err != nil {
			log.Printf("issue #%d: spawn failed on %s: %v", n, vm.Name, err)
			continue
		}
		_ = saveState(cfg.Orch.StateFile, st)
	}

	// Bound how many dead-session respawns we issue per tick. Each respawn
	// registers a new peer on the clawpatrol WG relay; firing several at
	// once overwhelms it and the freshly-spawned sessions die within
	// minutes (see denoland/clawpatrol#306). Sessions still down on the
	// next tick are picked up then, so kills stagger naturally.
	budget := killBudget{max: maxKillsPerTick}
	for n, j := range st.Jobs {
		if r, inOpen := open[n]; inOpen && j.IssueTitle == "" {
			j.IssueTitle = r.is.Title
		}
		if _, stillOpen := open[n]; !stillOpen {
			isOpen, err := ghIssueIsOpen(cfg.GitHub.InboxRepo, n)
			if err != nil {
				log.Printf("issue #%d: check open failed: %v", n, err)
			} else if !isOpen {
				tearDown(cfg, st, n)
				_ = saveState(cfg.Orch.StateFile, st)
				continue
			}
		}
		// Cron lifecycle: fire ephemeral sessions on schedule, no PR watch.
		if j.Lifecycle == "cron" {
			r, ok := open[n]
			if !ok {
				// Issue gone from inbox-list (closed, label removed). Earlier
				// block above triggers tearDown when the issue is actually
				// closed; if we get here the label was removed but the
				// issue's still open — drop the Job either way.
				log.Printf("issue #%d: cron job no longer in open list, dropping", n)
				tearDown(cfg, st, n)
				_ = saveState(cfg.Orch.StateFile, st)
				continue
			}
			tickCron(cfg, st, n, j, r.is, r.target)
			continue
		}
		vm := vmByName(cfg, j.VM)
		if vm == nil {
			log.Printf("issue #%d: vm %q gone from config, dropping", n, j.VM)
			delete(st.Jobs, n)
			_ = saveState(cfg.Orch.StateFile, st)
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
				// Session died with an open PR — respawn using --resume so
				// claude recovers its conversation context.
				log.Printf("issue #%d: tmux session %q gone, respawning with --resume (PR #%d)", n, j.Tmux, j.PR)
				if err := spawnResume(cfg, st, vm, n, j); err != nil {
					log.Printf("issue #%d: resume failed, tearing down: %v", n, err)
					tearDown(cfg, st, n)
				}
			} else {
				log.Printf("issue #%d: tmux session %q gone, tearing down", n, j.Tmux)
				tearDown(cfg, st, n)
			}
			_ = saveState(cfg.Orch.StateFile, st)
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
				// Workers can't open PRs through the clawpatrol MITM proxy
				// (gateway doesn't inject GitHub credentials for api.github.com).
				// Auto-create from the orchestrator once the branch has commits.
				if r, ok := open[n]; ok {
					prNum, err := ghAutoCreatePR(cfg, n, j, r.is)
					if err != nil {
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
			_ = saveState(cfg.Orch.StateFile, st)
		}
		v, err := ghPRView(j.TargetRepo, j.PR)
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
			tearDown(cfg, st, n)
			_ = saveState(cfg.Orch.StateFile, st)
			continue
		}
		nr, ntc, nic, pushed, checks := diffPR(j, v)
		if len(nr) == 0 && len(ntc) == 0 && len(nic) == 0 && !pushed && len(checks) == 0 {
			j.LastHeadOID = v.HeadRefOid
			continue
		}
		idle, err := tmuxIdle(*vm, j.Tmux)
		if err != nil {
			log.Printf("issue #%d: idle check failed: %v", n, err)
			continue
		}
		if !idle {
			log.Printf("issue #%d: pane busy, deferring poke", n)
			continue
		}
		msg := summarize(v, nr, ntc, nic, pushed, checks)
		if err := tmuxPaste(*vm, j.Tmux, msg); err != nil {
			log.Printf("issue #%d: poke failed: %v", n, err)
			continue
		}
		j.SeenReviewIDs = append(j.SeenReviewIDs, nr...)
		j.SeenThreadCommentIDs = append(j.SeenThreadCommentIDs, ntc...)
		j.SeenIssueCommentIDs = append(j.SeenIssueCommentIDs, nic...)
		j.LastHeadOID = v.HeadRefOid
		if j.LastCheckConclusions == nil {
			j.LastCheckConclusions = map[string]string{}
		}
		for _, c := range v.StatusCheckRollup {
			if c.Status == "COMPLETED" {
				j.LastCheckConclusions[c.Name] = c.Conclusion
			}
		}
		_ = saveState(cfg.Orch.StateFile, st)
		log.Printf("issue #%d: poked PR #%d", n, j.PR)
	}
}

// --- HTTP UI ---

//go:embed all:www/dist
var wwwFS embed.FS

type apiJobEntry struct {
	Issue int `json:"issue"`
	Job
}

type apiVMEntry struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Capacity int    `json:"capacity"`
	Used     int    `json:"used"`
}

type apiStateResp struct {
	Jobs  []apiJobEntry `json:"jobs"`
	VMs   []apiVMEntry  `json:"vms"`
	Inbox string        `json:"inbox"`
}

func wsReadFrame(r io.Reader) (opcode byte, payload []byte, err error) {
	hdr := make([]byte, 2)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return
	}
	opcode = hdr[0] & 0x0f
	masked := hdr[1]&0x80 != 0
	n := int(hdr[1] & 0x7f)
	if n == 126 {
		ext := make([]byte, 2)
		if _, err = io.ReadFull(r, ext); err != nil {
			return
		}
		n = int(ext[0])<<8 | int(ext[1])
	} else if n == 127 {
		ext := make([]byte, 8)
		if _, err = io.ReadFull(r, ext); err != nil {
			return
		}
		n = int(ext[4])<<24 | int(ext[5])<<16 | int(ext[6])<<8 | int(ext[7])
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(r, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(r, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

func wsWriteFrame(w io.Writer, opcode byte, payload []byte) error {
	n := len(payload)
	hdr := make([]byte, 2, 10)
	hdr[0] = 0x80 | opcode
	switch {
	case n <= 125:
		hdr[1] = byte(n)
	case n <= 65535:
		hdr[1] = 126
		hdr = append(hdr, byte(n>>8), byte(n))
	default:
		hdr[1] = 127
		hdr = append(hdr, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	_, err := w.Write(append(hdr, payload...))
	return err
}

// describe renders a markdown block describing this orch instance: config
// surface plus current job snapshot. Drop into a CLAUDE.md so future Claude
// sessions know how to operate this instance without rediscovering it.
// Hostname is the SSH target users would put after `ssh root@`; pass an empty
// string to use os.Hostname.
func describe(cfg *Config, st *State, hostname string) string {
	if hostname == "" {
		h, err := os.Hostname()
		if err == nil {
			hostname = h
		} else {
			hostname = "<unknown-host>"
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## orchid: %s\n\n", hostname)
	fmt.Fprintf(&b, "- Inbox:        %s\n", cfg.GitHub.InboxRepo)
	fmt.Fprintf(&b, "- Bot:          %s", cfg.Orch.BotLogin)
	if cfg.Orch.BotEmail != "" {
		fmt.Fprintf(&b, " <%s>", cfg.Orch.BotEmail)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "- Branch:       %s<N>\n", cfg.Orch.BranchPrefix)
	fmt.Fprintf(&b, "- State:        %s\n", cfg.Orch.StateFile)
	fmt.Fprintf(&b, "- Workdir root: %s\n", cfg.Orch.WorkdirRoot)
	fmt.Fprintf(&b, "- Poll:         %s\n", cfg.Orch.PollInterval)
	if cfg.Orch.HTTPAddr != "" {
		fmt.Fprintf(&b, "- Dashboard:    http://%s%s/\n", hostname, cfg.Orch.HTTPAddr)
	}
	if cfg.Orch.NtfyTopic != "" {
		fmt.Fprintf(&b, "- ntfy topic:   %s\n", cfg.Orch.NtfyTopic)
	}
	b.WriteString("\nTargets (label → work repo):\n")
	for _, t := range cfg.Targets {
		fmt.Fprintf(&b, "- `%s` → %s\n", t.Label, t.Repo)
	}
	b.WriteString("\nVMs:\n")
	totalCap := 0
	for _, vm := range cfg.VMs {
		login, _ := vmBotIdentity(cfg.Orch, vm)
		extra := ""
		if login != cfg.Orch.BotLogin {
			extra = fmt.Sprintf(", bot=%s", login)
		}
		cap := "unlimited"
		if vm.Capacity > 0 {
			cap = fmt.Sprint(vm.Capacity)
			totalCap += vm.Capacity
		}
		fmt.Fprintf(&b, "- `%s`: %s (capacity %s%s)\n", vm.Name, vm.Host, cap, extra)
	}
	// Current job snapshot from the lock-free copy published by tick.
	var snap map[int]Job
	if v := st.httpSnap.Load(); v != nil {
		snap = v.(map[int]Job)
	}
	fmt.Fprintf(&b, "\nActive sessions: %d", len(snap))
	if totalCap > 0 {
		fmt.Fprintf(&b, " / %d", totalCap)
	}
	b.WriteString("\n")
	if len(snap) > 0 {
		nums := make([]int, 0, len(snap))
		for n := range snap {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			j := snap[n]
			pr := "no PR yet"
			if j.PR != 0 {
				pr = fmt.Sprintf("PR #%d", j.PR)
			}
			fmt.Fprintf(&b, "- issue #%d → %s: branch `%s`, %s, on %s/%s\n",
				n, j.TargetRepo, j.Branch, pr, j.VM, j.Tmux)
		}
	}
	return b.String()
}

func renderLogin(w http.ResponseWriter, next, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset=utf-8>
<title>orchid — sign in</title>
<style>
body{font-family:monospace;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#f6f8fa}
form{background:#fff;border:1px solid #d0d7de;border-radius:6px;padding:24px 28px;min-width:300px}
h2{margin:0 0 16px;font-size:15px}
input[type=password]{width:100%%;box-sizing:border-box;padding:6px 10px;border:1px solid #d0d7de;border-radius:4px;font-family:monospace;font-size:13px;margin-bottom:10px}
button{width:100%%;padding:7px;background:#0969da;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:13px}
button:hover{background:#0860ca}
.err{color:#cf222e;font-size:12px;margin-bottom:8px}
</style></head><body>
<form method=POST action=/login>
<h2>orchid</h2>
%s
<input type=hidden name=next value=%q>
<input type=password name=token placeholder="token" autofocus>
<button type=submit>Sign in</button>
</form></body></html>`,
		func() string {
			if errMsg != "" {
				return `<div class="err">` + errMsg + `</div>`
			}
			return ""
		}(),
		next)
}

func httpHandler(cfg *Config, st *State) http.Handler {
	secret := cfg.Orch.HTTPSecret

	const cookieName = "orchid_token"

	auth := func(next http.HandlerFunc) http.HandlerFunc {
		if secret == "" {
			return next
		}
		return func(w http.ResponseWriter, r *http.Request) {
			tok := r.URL.Query().Get("token")
			if tok == "" {
				if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
					tok = h[7:]
				}
			}
			if tok == "" {
				if c, err := r.Cookie(cookieName); err == nil {
					tok = c.Value
				}
			}
			if tok != secret {
				renderLogin(w, r.URL.RequestURI(), "")
				return
			}
			if r.URL.Query().Get("token") != "" {
				http.SetCookie(w, &http.Cookie{
					Name: cookieName, Value: secret,
					Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
				})
				q := r.URL.Query()
				q.Del("token")
				r.URL.RawQuery = q.Encode()
				http.Redirect(w, r, r.URL.RequestURI(), http.StatusSeeOther)
				return
			}
			next(w, r)
		}
	}

	mux := http.NewServeMux()

	if secret != "" {
		mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				renderLogin(w, "/", "")
				return
			}
			_ = r.ParseForm()
			if r.FormValue("token") == secret {
				http.SetCookie(w, &http.Cookie{
					Name: cookieName, Value: secret,
					Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
				})
				dest := r.FormValue("next")
				if dest == "" {
					dest = "/"
				}
				http.Redirect(w, r, dest, http.StatusSeeOther)
			} else {
				renderLogin(w, r.FormValue("next"), "invalid token")
			}
		})
	}

	// /api/state — JSON snapshot of jobs + VMs
	mux.HandleFunc("/api/state", auth(func(w http.ResponseWriter, r *http.Request) {
		var snap map[int]Job
		if v := st.httpSnap.Load(); v != nil {
			snap = v.(map[int]Job)
		}
		load := map[string]int{}
		jobs := make([]apiJobEntry, 0, len(snap))
		for issue, j := range snap {
			jobs = append(jobs, apiJobEntry{Issue: issue, Job: j})
			load[j.VM]++
		}
		sort.Slice(jobs, func(a, b int) bool { return jobs[a].Tmux < jobs[b].Tmux })

		vms := make([]apiVMEntry, 0, len(cfg.VMs))
		for _, vm := range cfg.VMs {
			vms = append(vms, apiVMEntry{
				Name:     vm.Name,
				Host:     vm.Host,
				Capacity: vm.Capacity,
				Used:     load[vm.Name],
			})
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(apiStateResp{
			Jobs:  jobs,
			VMs:   vms,
			Inbox: cfg.GitHub.InboxRepo,
		})
	}))

	// /ws?s=<session> — WebSocket streaming tmux pane output
	mux.HandleFunc("/ws", auth(func(w http.ResponseWriter, r *http.Request) {
		session := r.URL.Query().Get("s")
		for _, c := range session {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				http.Error(w, "invalid session", http.StatusBadRequest)
				return
			}
		}
		if session == "" {
			http.Error(w, "s required", http.StatusBadRequest)
			return
		}

		var vm *VMBlock
		if v := st.httpSnap.Load(); v != nil {
			for _, j := range v.(map[int]Job) {
				if j.Tmux == session {
					vm = vmByName(cfg, j.VM)
					break
				}
			}
		}
		if vm == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}

		key := r.Header.Get("Sec-Websocket-Key")
		if key == "" {
			http.Error(w, "not a websocket upgrade", http.StatusBadRequest)
			return
		}
		sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
		accept := base64.StdEncoding.EncodeToString(sum[:])

		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = buf

		resp := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Upgrade: websocket\r\n" +
			"Connection: Upgrade\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}

		done := make(chan struct{})
		go func() {
			defer close(done)
			for {
				_, payload, err := wsReadFrame(conn)
				if err != nil || len(payload) == 0 {
					return
				}
				b64 := base64.StdEncoding.EncodeToString(payload)
				sshExec(*vm, fmt.Sprintf("echo %s | base64 -d | tmux load-buffer -b orch-in - && tmux paste-buffer -b orch-in -t %s -d", b64, session))
			}
		}()

		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		var last string
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				out, _, err := sshExec(*vm, fmt.Sprintf("tmux capture-pane -p -t %s -e -S -200 2>&1", session))
				if err != nil {
					return
				}
				if out == last {
					continue
				}
				last = out
				conn.(net.Conn).SetWriteDeadline(time.Now().Add(5 * time.Second)) //nolint
				if err := wsWriteFrame(conn, 0x02, []byte(out)); err != nil {
					return
				}
			}
		}
	}))

	// SPA static files — all other routes serve www/dist with index.html fallback
	spaFS, _ := fs.Sub(wwwFS, "www/dist")
	fileServer := http.FileServerFS(spaFS)
	mux.HandleFunc("/", auth(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" {
			if _, err := fs.Stat(spaFS, path); err == nil {
				fileServer.ServeHTTP(w, r)
				return
			}
		}
		http.ServeFileFS(w, r, spaFS, "index.html")
	}))

	return mux
}

func main() {
	cfgPath := flag.String("config", "swarm.hcl", "path to HCL config")
	describeFlag := flag.Bool("describe", false, "print a CLAUDE.md-shaped description of this instance and exit")
	flag.Parse()

	var cfg Config
	if err := hclsimple.DecodeFile(*cfgPath, nil, &cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := loadState(cfg.Orch.StateFile)
	if err != nil {
		log.Fatalf("state: %v", err)
	}
	if *describeFlag {
		// Mirror the runtime publish so describe sees a non-nil snapshot.
		snap := make(map[int]Job, len(st.Jobs))
		for n, j := range st.Jobs {
			snap[n] = *j
		}
		st.httpSnap.Store(snap)
		fmt.Print(describe(&cfg, st, ""))
		return
	}
	interval, err := time.ParseDuration(cfg.Orch.PollInterval)
	if err != nil {
		log.Fatalf("poll_interval: %v", err)
	}
	for _, vm := range cfg.VMs {
		if login, _ := vmBotIdentity(cfg.Orch, vm); login == "" {
			log.Fatalf("vm %q: bot_login not set; configure orchestrator.bot_login or vm.%s.bot_login", vm.Name, vm.Name)
		}
	}
	tnames := make([]string, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		tnames = append(tnames, fmt.Sprintf("%s(%s→%s)", t.Name, t.Label, t.Repo))
	}
	log.Printf("orch up: inbox=%s targets=[%s] vms=%d interval=%s tracked=%d",
		cfg.GitHub.InboxRepo, strings.Join(tnames, ","), len(cfg.VMs), interval, len(st.Jobs))

	if cfg.Orch.HTTPAddr != "" {
		go func() {
			log.Printf("http ui on http://%s/", cfg.Orch.HTTPAddr)
			if err := http.ListenAndServe(cfg.Orch.HTTPAddr, httpHandler(&cfg, st)); err != nil {
				log.Printf("http server: %v", err)
			}
		}()
	}

	for i := range cfg.VMs {
		if err := bootstrapVM(cfg.VMs[i]); err != nil {
			log.Printf("vm %s: bootstrap FAILED: %v", cfg.VMs[i].Name, err)
		} else {
			log.Printf("vm %s: bootstrapped (github ssh ok)", cfg.VMs[i].Name)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	t := time.NewTicker(interval)
	defer t.Stop()
	tick(&cfg, st)
	for {
		select {
		case <-ctx.Done():
			log.Printf("shutdown")
			return
		case <-t.C:
			tick(&cfg, st)
		}
	}
}
