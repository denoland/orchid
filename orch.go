package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

type Config struct {
	GitHub          GitHubBlock `hcl:"github,block"`
	Orch            OrchBlock   `hcl:"orchestrator,block"`
	BootstrapPrompt string      `hcl:"bootstrap_prompt"`
	VMs             []VMBlock   `hcl:"vm,block"`
}

type GitHubBlock struct {
	Repo     string `hcl:"repo"`
	Label    string `hcl:"label"`
	TokenEnv string `hcl:"token_env,optional"`
}

type OrchBlock struct {
	PollInterval string `hcl:"poll_interval"`
	StateFile    string `hcl:"state_file"`
	BranchPrefix string `hcl:"branch_prefix"`
}

type VMBlock struct {
	Name string `hcl:",label"`
	Host string `hcl:"host"`
	User string `hcl:"user"`
	Key  string `hcl:"key"`
}

type Job struct {
	VM                   string            `json:"vm"`
	Tmux                 string            `json:"tmux"`
	Branch               string            `json:"branch"`
	PR                   int               `json:"pr,omitempty"`
	SeenReviewIDs        []int64           `json:"seen_review_ids,omitempty"`
	SeenThreadCommentIDs []int64           `json:"seen_thread_comment_ids,omitempty"`
	SeenIssueCommentIDs  []int64           `json:"seen_issue_comment_ids,omitempty"`
	LastHeadOID          string            `json:"last_head_oid,omitempty"`
	LastCheckConclusions map[string]string `json:"last_check_conclusions,omitempty"`
}

type State struct {
	Jobs map[int]*Job `json:"jobs"`
}

func run(name string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func runIn(stdin string, name string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
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

func sshExec(vm VMBlock, remote string) (string, string, error) {
	return run("ssh", append(sshArgs(vm), remote)...)
}

func sshExecIn(vm VMBlock, stdin, remote string) (string, string, error) {
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
		ID     int64                  `json:"id"`
		Author struct{ Login string } `json:"author"`
		State  string                 `json:"state"`
		Body   string                 `json:"body"`
	} `json:"reviews"`
	ReviewThreads []struct {
		Path     string `json:"path"`
		Line     int    `json:"line"`
		Comments []struct {
			ID     int64                  `json:"id"`
			Author struct{ Login string } `json:"author"`
			Body   string                 `json:"body"`
		} `json:"comments"`
	} `json:"reviewThreads"`
	Comments []struct {
		ID     int64                  `json:"id"`
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
		"--json", "state,headRefOid,reviews,reviewThreads,comments,statusCheckRollup")
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

func tmuxStart(vm VMBlock, session string) error {
	// bash -lc so the user's login PATH (e.g. ~/.local/bin for clawpatrol) is in scope.
	cmd := fmt.Sprintf(`tmux new-session -d -s %s 'bash -lc "clawpatrol run -- claude"'`, session)
	_, errStr, err := sshExec(vm, cmd)
	if err != nil {
		return fmt.Errorf("tmux new-session: %v: %s", err, errStr)
	}
	return nil
}

func tmuxKill(vm VMBlock, session string) {
	_, _, _ = sshExec(vm, fmt.Sprintf("tmux kill-session -t %s 2>/dev/null", session))
}

// tmuxIdle is a heuristic for "claude TUI is at its input prompt and not
// processing". It looks for a prompt marker and the absence of the
// in-progress hint claude prints while it works. False negatives (we think
// it's busy when it isn't) just defer the poke by one tick — safe.
func tmuxIdle(vm VMBlock, session string) (bool, error) {
	out, _, err := sshExec(vm, fmt.Sprintf("tmux capture-pane -p -t %s | tail -8", session))
	if err != nil {
		return false, err
	}
	if strings.Contains(out, "esc to interrupt") {
		return false, nil
	}
	return strings.Contains(out, "> "), nil
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
	used := map[string]bool{}
	for _, j := range st.Jobs {
		used[j.VM] = true
	}
	idx := make([]int, 0, len(cfg.VMs))
	for i := range cfg.VMs {
		idx = append(idx, i)
	}
	sort.Slice(idx, func(a, b int) bool { return cfg.VMs[idx[a]].Name < cfg.VMs[idx[b]].Name })
	for _, i := range idx {
		if !used[cfg.VMs[i].Name] {
			return &cfg.VMs[i]
		}
	}
	return nil
}

func vmByName(cfg *Config, name string) *VMBlock {
	for i := range cfg.VMs {
		if cfg.VMs[i].Name == name {
			return &cfg.VMs[i]
		}
	}
	return nil
}

func renderBootstrap(tmpl string, is Issue, branch string) string {
	return strings.NewReplacer(
		"{{issue.number}}", fmt.Sprint(is.Number),
		"{{issue.title}}", is.Title,
		"{{issue.body}}", is.Body,
		"{{branch}}", branch,
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

func diffPR(j *Job, v *PRView) (newReviews, newThreadComments, newIssueComments []int64, pushed bool, checkChanges []string) {
	seen := func(ids []int64) map[int64]bool {
		m := map[int64]bool{}
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

func summarize(v *PRView, nr, ntc, nic []int64, pushed bool, checks []string) string {
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

func spawn(cfg *Config, st *State, vm *VMBlock, is Issue) error {
	session := sessionName(is.Number)
	branch := cfg.Orch.BranchPrefix + fmt.Sprint(is.Number)
	if err := tmuxStart(*vm, session); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if idle, err := tmuxIdle(*vm, session); err == nil && idle {
			break
		}
		time.Sleep(2 * time.Second)
	}
	msg := renderBootstrap(cfg.BootstrapPrompt, is, branch)
	if err := tmuxPaste(*vm, session, msg); err != nil {
		tmuxKill(*vm, session)
		return fmt.Errorf("bootstrap paste: %w", err)
	}
	st.Jobs[is.Number] = &Job{VM: vm.Name, Tmux: session, Branch: branch, LastCheckConclusions: map[string]string{}}
	log.Printf("issue #%d: spawned on %s/%s, branch=%s", is.Number, vm.Name, session, branch)
	return nil
}

func tick(cfg *Config, st *State) {
	issues, err := ghIssueList(cfg.GitHub.Repo, cfg.GitHub.Label)
	if err != nil {
		log.Printf("list issues: %v", err)
		return
	}
	open := map[int]Issue{}
	for _, is := range issues {
		open[is.Number] = is
	}

	for n, is := range open {
		if _, exists := st.Jobs[n]; exists {
			continue
		}
		vm := freeVM(cfg, st)
		if vm == nil {
			log.Printf("issue #%d: no free VM, skipping", n)
			continue
		}
		if err := spawn(cfg, st, vm, is); err != nil {
			log.Printf("issue #%d: spawn failed on %s: %v", n, vm.Name, err)
			continue
		}
		_ = saveState(cfg.Orch.StateFile, st)
	}

	for n, j := range st.Jobs {
		if _, stillOpen := open[n]; !stillOpen {
			isOpen, err := ghIssueIsOpen(cfg.GitHub.Repo, n)
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
			pr, err := ghFindPRByBranch(cfg.GitHub.Repo, j.Branch)
			if err != nil {
				log.Printf("issue #%d: find PR failed: %v", n, err)
				continue
			}
			if pr == nil {
				continue
			}
			j.PR = pr.Number
			log.Printf("issue #%d: found PR #%d", n, j.PR)
			_ = saveState(cfg.Orch.StateFile, st)
		}
		v, err := ghPRView(cfg.GitHub.Repo, j.PR)
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
	log.Printf("orch up: repo=%s label=%s vms=%d interval=%s tracked=%d",
		cfg.GitHub.Repo, cfg.GitHub.Label, len(cfg.VMs), interval, len(st.Jobs))

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
