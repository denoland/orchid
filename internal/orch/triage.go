package orch

// Discovery agents: cheap one-shot `claude -p` passes that surface FACTS, not
// decisions. Two call sites:
//
//   - triage: every new inbox issue gets a pre-flight scout — existing PRs
//     covering the same work, sibling/duplicate inbox issues, likely source
//     pointers, a size read. Posted as an issue comment the worker reads at
//     bootstrap. ~18% of all rejected PRs were duplicates of work a 30-second
//     search would have found; this is that search.
//   - postmortem: when a job's PR reaches a terminal state, distill ONE line
//     (why it merged smoothly / why it was rejected) into lessons.md in the
//     shared memory repo, so the fleet compounds judgment instead of repeating
//     the same rejection class forever.
//
// Both run async off the tick loop, bounded by a small semaphore, and are
// best-effort: any failure logs and moves on. Neither blocks or delays a
// spawn — triage races the worker on purpose (instant PRs > perfect order).

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const triageMarker = "<!-- orchid-triage -->"

var (
	triageSem  = make(chan struct{}, 2)
	triageSeen sync.Map // issue number -> struct{}{}
)

// runAgentOneShot pipes prompt into the configured triage_cmd and returns
// trimmed stdout. The command is operator-defined in swarm.hcl (model, creds
// wrapper, flags all live there, not here).
func runAgentOneShot(cfg *Config, prompt string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-lc", cfg.Orch.TriageCmd)
	cmd.Stdin = strings.NewReader(prompt)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v: %s", err, oneLine(errb.String(), 300))
	}
	s := strings.TrimSpace(out.String())
	if len(s) > 16*1024 {
		s = s[:16*1024] + "\n… (truncated)"
	}
	return s, nil
}

// triageIssue is called from the tick for every open, routed, not-yet-jobbed
// inbox issue. Dedupe is two-layer: in-memory for the hot path, kv for
// restarts. Fire-and-forget.
func triageIssue(cfg *Config, st *State, is Issue, targetRepo string) {
	if cfg.Orch.TriageCmd == "" {
		return
	}
	if _, dup := triageSeen.LoadOrStore(is.Number, struct{}{}); dup {
		return
	}
	key := fmt.Sprintf("triaged_%d", is.Number)
	if st.store != nil {
		if v, _ := st.store.GetKV(key); v != nil {
			return
		}
		_ = st.store.PutKV(key, []byte(time.Now().UTC().Format(time.RFC3339)))
	}
	go func() {
		triageSem <- struct{}{}
		defer func() { <-triageSem }()
		body := is.Body
		if len(body) > 4000 {
			body = body[:4000] + "\n… (truncated)"
		}
		prompt := fmt.Sprintf(`You are orchid's triage scout. DISCOVERY ONLY: report facts and links; no recommendations to skip, close, or deprioritize, no opinions on whether the work is worthwhile. A worker agent and a human will read your report.

Inbox issue %s#%d
Title: %s
Target repo: %s
Body:
%s

Investigate with the gh CLI (read-only):
1. Existing work: open or merged PRs in %s that already cover this (gh pr list --repo %s --search '...' --state all --limit 10; if the title references an upstream issue like [owner/repo#N], also: gh pr list --repo %s --search 'N in:title,body' and check the upstream issue's linked PRs via gh api).
2. Duplicates: other inbox issues in %s referencing the same upstream issue or describing the same work (gh issue list --repo %s --search ... --state all).
3. Pointers: the most likely source files/dirs/tests in %s to start from (search the repo with gh search code or your knowledge of it).
4. Size: S (single sitting), M (a day), or L (multi-session port) with one line of rationale.

Output exactly this markdown, nothing else, max ~25 lines:
## Triage
**Existing work:** <PR links + one-line status each, or "none found">
**Duplicate inbox issues:** <links or "none found">
**Pointers:** <files/dirs, key functions>
**Size:** <S|M|L> — <rationale>`,
			cfg.GitHub.InboxRepo, is.Number, is.Title, targetRepo, body,
			targetRepo, targetRepo, targetRepo,
			cfg.GitHub.InboxRepo, cfg.GitHub.InboxRepo, targetRepo)
		out, err := runAgentOneShot(cfg, prompt, 5*time.Minute)
		if err != nil {
			log.Printf("issue #%d: triage failed: %v", is.Number, err)
			return
		}
		if out == "" || !strings.Contains(out, "## Triage") {
			log.Printf("issue #%d: triage produced no usable report, skipping comment", is.Number)
			return
		}
		comment := triageMarker + "\n" + out
		if _, errStr, err := run("gh", "issue", "comment", fmt.Sprint(is.Number),
			"--repo", cfg.GitHub.InboxRepo, "--body", comment); err != nil {
			log.Printf("issue #%d: triage comment failed: %v: %s", is.Number, err, strings.TrimSpace(errStr))
			return
		}
		log.Printf("issue #%d: triage report posted", is.Number)
	}()
}

// runPostmortem distills the outcome of a finished PR into one lesson line in
// the shared memory repo (lessons.md). Called async from closeInboxIssue.
func runPostmortem(cfg *Config, issue int, prState, repo string, pr int) {
	if cfg.Orch.TriageCmd == "" {
		return
	}
	triageSem <- struct{}{}
	defer func() { <-triageSem }()
	view, _, err := run("gh", "pr", "view", fmt.Sprint(pr), "--repo", repo,
		"--json", "title,state,additions,deletions,reviews,comments",
		"--jq", `{title,state,additions,deletions,reviews:[.reviews[-5:][]|{author:.author.login,state,body:(.body|.[0:400])}],comments:[.comments[-5:][]|{author:.author.login,body:(.body|.[0:400])}]}`)
	if err != nil {
		log.Printf("issue #%d: postmortem pr view failed: %v", issue, err)
		return
	}
	prompt := fmt.Sprintf(`You are orchid's postmortem scribe. A swarm worker's PR just reached a terminal state. Distill ONE lesson line for future workers in the same repos. Facts only — base it strictly on the data below; if the data doesn't show a clear reason, say "no clear signal". No speculation, no advice beyond what the evidence supports.

PR %s#%d — %s
%s

Output EXACTLY one line, <=200 chars, this format:
- %s %s#%d: <why it merged smoothly | why it was rejected | no clear signal>`,
		repo, pr, strings.ToLower(prState), view,
		strings.ToLower(prState), repo, pr)
	out, err := runAgentOneShot(cfg, prompt, 3*time.Minute)
	if err != nil {
		log.Printf("issue #%d: postmortem failed: %v", issue, err)
		return
	}
	line := ""
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "- ") {
			line = l
			break
		}
	}
	if line == "" {
		log.Printf("issue #%d: postmortem produced no lesson line", issue)
		return
	}
	appendLesson(cfg, line)
}

// appendLesson appends one line to lessons.md in the memory store; the memory
// sync loop commits and pushes it to the shared repo on its normal cadence.
func appendLesson(cfg *Config, line string) {
	vm, _ := memoryStore(cfg)
	if vm == nil {
		return
	}
	dir := memoryStoreDir(cfg, vm)
	script := fmt.Sprintf(`DIR="%s"
mkdir -p "$DIR"
[ -f "$DIR/lessons.md" ] || printf '# Lessons — auto-distilled PR postmortems\n\nOne line per finished PR: why it merged smoothly or why it was rejected. Newest last.\n\n' > "$DIR/lessons.md"
printf '%%s\n' %s >> "$DIR/lessons.md"`, dir, shellSingleQuote(line))
	if _, errStr, err := sshExecIn(*vm, script, "bash -s"); err != nil {
		log.Printf("append lesson: %v: %s", err, strings.TrimSpace(errStr))
		return
	}
	log.Printf("lesson recorded: %s", oneLine(line, 160))
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
