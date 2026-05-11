package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

type Config struct {
	GitHub          GitHubBlock   `hcl:"github,block"`
	Orch            OrchBlock     `hcl:"orchestrator,block"`
	BootstrapPrompt string        `hcl:"bootstrap_prompt"`
	Targets         []TargetBlock `hcl:"target,block"`
	VMs             []VMBlock     `hcl:"vm,block"`
}

type GitHubBlock struct {
	InboxRepo string `hcl:"inbox_repo"`
	TokenEnv  string `hcl:"token_env,optional"`
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
	BotLogin     string `hcl:"bot_login,optional"` // shown in the UI bot column
}

type VMBlock struct {
	Name       string `hcl:",label"`
	Host       string `hcl:"host"`
	User       string `hcl:"user,optional"`
	Key        string `hcl:"key,optional"`        // not needed for localhost
	Capacity   int    `hcl:"capacity,optional"`   // 0 = unlimited
	Sccache    bool   `hcl:"sccache,optional"`
	SccacheDir string `hcl:"sccache_dir,optional"` // default ~/.cache/sccache
}

type Job struct {
	VM                   string            `json:"vm"`
	Tmux                 string            `json:"tmux"`
	Target               string            `json:"target"`      // target block name
	TargetRepo           string            `json:"target_repo"` // resolved (e.g. denoland/deno)
	Branch               string            `json:"branch"`
	PR                   int               `json:"pr,omitempty"`
	SeenReviewIDs        []string          `json:"seen_review_ids,omitempty"`
	SeenThreadCommentIDs []string          `json:"seen_thread_comment_ids,omitempty"`
	SeenIssueCommentIDs  []string          `json:"seen_issue_comment_ids,omitempty"`
	LastHeadOID          string            `json:"last_head_oid,omitempty"`
	LastCheckConclusions map[string]string `json:"last_check_conclusions,omitempty"`
}

type State struct {
	mu   sync.Mutex
	Jobs map[int]*Job `json:"jobs"`
}

// retry wraps an exec.Command-style call with bounded retries on non-zero
// exit. clawpatrol's MITM proxy is known to drop connections sporadically
// (gh: "error connecting to api.github.com", ssh: exit 255); this hides
// those blips so a single tick doesn't lose work. Backoff: 1s, 2s, 4s.
const runAttempts = 4

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
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
}

func ghIssueList(repo, label string) ([]Issue, error) {
	out, errStr, err := run("gh", "issue", "list",
		"--repo", repo, "--label", label, "--state", "open",
		"--limit", "200", "--json", "number,title,body,state")
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %v: %s", err, errStr)
	}
	var issues []Issue
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		return nil, err
	}
	return issues, nil
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

func ghFindPRByBranch(repo, branch string) (*PRSummary, error) {
	out, errStr, err := run("gh", "pr", "list",
		"--repo", repo, "--head", branch, "--state", "all",
		"--limit", "5", "--json", "number,state")
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
  jq '. + {skipDangerousModePermissionPrompt: true}' ~/.claude/settings.json > ~/.claude/settings.json.tmp && mv ~/.claude/settings.json.tmp ~/.claude/settings.json
else
  echo '{"theme":"dark","skipDangerousModePermissionPrompt":true}' > ~/.claude/settings.json
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
		remoteScript := fmt.Sprintf(`set -e
umask 077
mkdir -m 700 -p ~/.ssh
echo %s | base64 -d > ~/.ssh/id_ed25519
chmod 600 ~/.ssh/id_ed25519
echo %s | base64 -d > ~/.ssh/id_ed25519.pub
chmod 644 ~/.ssh/id_ed25519.pub
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
func tmuxStart(vm VMBlock, session, workdir, sharedDir, repo, branch string) error {
	script := fmt.Sprintf(`set -e
SHARED=%q
REPO=%q
WORKDIR=%q
BRANCH=%q
SESSION=%q

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
git -C "$WORKDIR" config user.name divybot
git -C "$WORKDIR" config user.email divybot@users.noreply.github.com

# 4) pre-stamp claude's per-folder trust flag so the TUI doesn't prompt
jq --arg d "$WORKDIR" '.projects[$d].hasTrustDialogAccepted = true' ~/.claude.json > ~/.claude.json.tmp && mv ~/.claude.json.tmp ~/.claude.json

# 5) launch the pane
tmux kill-session -t "$SESSION" 2>/dev/null || true
tmux new-session -d -c "$WORKDIR" -s "$SESSION" 'bash -lc "clawpatrol run -- claude --dangerously-skip-permissions"'
`, sharedDir, repo, workdir, branch, session)

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
func tmuxIdle(vm VMBlock, session string) (bool, error) {
	out, _, err := sshExec(vm, fmt.Sprintf("tmux capture-pane -p -t %s | tail -8", session))
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

func renderBootstrap(tmpl string, is Issue, branch, targetName, targetRepo, inboxRepo, workdir string) string {
	return strings.NewReplacer(
		"{{issue.number}}", fmt.Sprint(is.Number),
		"{{issue.title}}", is.Title,
		"{{issue.body}}", is.Body,
		"{{branch}}", branch,
		"{{target.name}}", targetName,
		"{{target.repo}}", targetRepo,
		"{{inbox.repo}}", inboxRepo,
		"{{workdir}}", workdir,
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
	return os.Rename(tmp, path)
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

func spawn(cfg *Config, st *State, vm *VMBlock, is Issue, target TargetBlock) error {
	session := sessionName(is.Number)
	branch := cfg.Orch.BranchPrefix + fmt.Sprint(is.Number)
	root := strings.TrimRight(cfg.Orch.WorkdirRoot, "/")
	workdir := fmt.Sprintf("%s/issue-%d", root, is.Number)
	sharedDir := fmt.Sprintf("%s/repos/%s", root, strings.ReplaceAll(target.Repo, "/", "-"))
	if err := tmuxStart(*vm, session, workdir, sharedDir, target.Repo, branch); err != nil {
		return err
	}
	// Defensive: dismiss claude's per-folder trust dialog if it appears.
	// Default is "Yes, I trust this folder" so plain Enter accepts.
	// Settings.json provisioned by bootstrapVM kills the dangerous-mode
	// warnings, so trust is the only dialog we should see at first launch.
	time.Sleep(3 * time.Second)
	_, _, _ = sshExec(*vm, fmt.Sprintf("tmux send-keys -t %s Enter", session))
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if idle, err := tmuxIdle(*vm, session); err == nil && idle {
			break
		}
		time.Sleep(2 * time.Second)
	}
	msg := renderBootstrap(cfg.BootstrapPrompt, is, branch, target.Name, target.Repo, cfg.GitHub.InboxRepo, workdir)
	if err := tmuxPaste(*vm, session, msg); err != nil {
		tmuxKill(*vm, session)
		return fmt.Errorf("bootstrap paste: %w", err)
	}
	st.Jobs[is.Number] = &Job{
		VM: vm.Name, Tmux: session,
		Target: target.Name, TargetRepo: target.Repo,
		Branch: branch, LastCheckConclusions: map[string]string{},
	}
	log.Printf("issue #%d: spawned on %s/%s, target=%s (%s), branch=%s",
		is.Number, vm.Name, session, target.Name, target.Repo, branch)
	return nil
}

func tick(cfg *Config, st *State) {
	st.mu.Lock()
	defer st.mu.Unlock()
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

	for n, j := range st.Jobs {
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
			log.Printf("issue #%d: tmux session %q gone, tearing down", n, j.Tmux)
			tearDown(cfg, st, n)
			_ = saveState(cfg.Orch.StateFile, st)
			continue
		}
		if j.PR == 0 {
			pr, err := ghFindPRByBranch(j.TargetRepo, j.Branch)
			if err != nil {
				log.Printf("issue #%d: find PR failed: %v", n, err)
				continue
			}
			if pr == nil {
				continue
			}
			j.PR = pr.Number
			log.Printf("issue #%d: found PR #%d in %s", n, j.PR, j.TargetRepo)
			_ = saveState(cfg.Orch.StateFile, st)
		}
		v, err := ghPRView(j.TargetRepo, j.PR)
		if err != nil {
			log.Printf("issue #%d: pr view failed: %v", n, err)
			continue
		}
		if v.State == "MERGED" || v.State == "CLOSED" {
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

const indexTmpl = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>orchid</title>
<meta http-equiv="refresh" content="5">
<style>
  body { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; margin: 24px; color: #222; }
  h1 { font-size: 16px; margin: 0 0 4px 0; }
  .meta { color: #666; font-size: 12px; margin-bottom: 18px; }
  table { border-collapse: collapse; width: 100%; font-size: 13px; }
  th, td { padding: 8px 12px; border-bottom: 1px solid #e5e5e5; text-align: left; vertical-align: top; }
  th { background: #f6f6f6; font-weight: 600; }
  tr.busy td { background: #fafffa; }
  tr.free td { color: #999; }
  .pill { display: inline-block; padding: 1px 8px; border-radius: 8px; font-size: 11px; }
  .pill.busy { background: #d4edda; color: #155724; }
  .pill.free { background: #eee; color: #666; }
  a { color: #0366d6; text-decoration: none; }
  a:hover { text-decoration: underline; }
  .term { font-weight: 600; }
</style>
</head>
<body>
<h1>orchid swarm</h1>
<div class="meta">
  inbox: <a href="https://github.com/{{.Inbox}}/issues">{{.Inbox}}</a> ·
  targets: {{range $i, $t := .Targets}}{{if $i}}, {{end}}<code>{{$t.Label}}</code>→<a href="https://github.com/{{$t.Repo}}">{{$t.Repo}}</a>{{end}} ·
  refresh 5s · updated {{.Updated}}
</div>
<table>
<thead><tr>
  <th>VM</th><th>Status</th><th>Issue</th><th>Repo</th><th>Bot</th><th>Session</th><th>PR</th><th>Terminal</th>
</tr></thead>
<tbody>
{{range .Rows}}
<tr class="{{if .Busy}}busy{{else}}free{{end}}">
  <td>{{.VM}}</td>
  <td>{{if .Busy}}<span class="pill busy">busy</span>{{else}}<span class="pill free">free</span>{{end}}</td>
  <td>{{if .Issue}}<a href="https://github.com/{{$.Inbox}}/issues/{{.Issue}}">#{{.Issue}}</a>{{end}}</td>
  <td>{{if .Repo}}<a href="https://github.com/{{.Repo}}">{{.Repo}}</a>{{end}}</td>
  <td>{{.Bot}}</td>
  <td><code>{{.Session}}</code></td>
  <td>{{if .PR}}<a href="https://github.com/{{.Repo}}/pull/{{.PR}}">#{{.PR}}</a>{{end}}</td>
  <td>{{if .TermURL}}<a class="term" target="_blank" href="{{.TermURL}}">open ↗</a>{{else}}local{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
</body>
</html>`

type uiRow struct {
	VM      string
	Busy    bool
	Issue   int
	Repo    string
	Bot     string
	Session string
	PR      int
	TermURL string
}

type uiData struct {
	Inbox   string
	Targets []TargetBlock
	Rows    []uiRow
	Updated string
}

// xtermURL builds the exe.dev xterm URL for a VM host. With a session name
// we add ?cmd=tmux+attach+-t+SESSION so the xterm auto-attaches if exe.dev
// honors the param; falls back to a plain shell if not.
func xtermURL(host, session string) string {
	parts := strings.SplitN(host, ".", 2)
	if len(parts) != 2 {
		return ""
	}
	u := fmt.Sprintf("https://%s.xterm.%s/", parts[0], parts[1])
	if session != "" {
		u += "?cmd=" + strings.ReplaceAll("tmux attach -t "+session, " ", "+")
	}
	return u
}

func httpHandler(cfg *Config, st *State) http.Handler {
	t := template.Must(template.New("ix").Parse(indexTmpl))
	bot := cfg.Orch.BotLogin
	if bot == "" {
		bot = "divybot"
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		st.mu.Lock()
		used := map[string]int{}
		jobs := map[int]Job{}
		for n, j := range st.Jobs {
			used[j.VM] = n
			jobs[n] = *j
		}
		st.mu.Unlock()

		rows := make([]uiRow, 0, len(cfg.VMs))
		for _, vm := range cfg.VMs {
			r := uiRow{VM: vm.Name, Bot: bot}
			if n, ok := used[vm.Name]; ok {
				j := jobs[n]
				r.Busy = true
				r.Issue = n
				r.Repo = j.TargetRepo
				r.Session = j.Tmux
				r.PR = j.PR
			}
			r.TermURL = xtermURL(vm.Host, r.Session)
			rows = append(rows, r)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].VM < rows[j].VM })

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = t.Execute(w, uiData{
			Inbox:   cfg.GitHub.InboxRepo,
			Targets: cfg.Targets,
			Rows:    rows,
			Updated: time.Now().UTC().Format("15:04:05Z"),
		})
	})
}

func main() {
	cfgPath := flag.String("config", "swarm.hcl", "path to HCL config")
	flag.Parse()

	var cfg Config
	if err := hclsimple.DecodeFile(*cfgPath, nil, &cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	interval, err := time.ParseDuration(cfg.Orch.PollInterval)
	if err != nil {
		log.Fatalf("poll_interval: %v", err)
	}
	st, err := loadState(cfg.Orch.StateFile)
	if err != nil {
		log.Fatalf("state: %v", err)
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
