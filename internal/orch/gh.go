package orch

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

func ghJSON[T any](args ...string) (T, error) {
	var zero T
	out, errStr, err := run("gh", args...)
	if err != nil {
		return zero, fmt.Errorf("gh %s: %v: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(errStr))
	}
	var v T
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return zero, err
	}
	return v, nil
}

func ghIssueList(repo, label string) ([]Issue, error) {
	args := []string{"issue", "list", "--repo", repo, "--state", "open",
		"--limit", "200", "--json", "number,title,body,state,labels"}
	if label != "" {
		args = append(args, "--label", label)
	}
	type rawIssue struct {
		Number int                     `json:"number"`
		Title  string                  `json:"title"`
		Body   string                  `json:"body"`
		State  string                  `json:"state"`
		Labels []struct{ Name string } `json:"labels"`
	}
	raw, err := ghJSON[[]rawIssue](args...)
	if err != nil {
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

func (is Issue) hasLabel(name string) bool {
	for _, l := range is.Labels {
		if l == name {
			return true
		}
	}
	return false
}

type PRSummary struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

func ghFindPRByBranch(repo, branch, author string) (*PRSummary, error) {
	args := []string{"pr", "list", "--repo", repo, "--head", branch, "--state", "all",
		"--limit", "5", "--json", "number,state"}
	if author != "" {
		args = append(args, "--author", author)
	}
	prs, err := ghJSON[[]PRSummary](args...)
	if err != nil {
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

// prTitleFromBranch returns the subject line of the branch's first commit
// against main, or "" if it can't be read. Agents write conventional-commit
// messages, so this is the natural, lint-passing PR title.
func prTitleFromBranch(repo, branch string) string {
	out, _, err := run("gh", "api",
		fmt.Sprintf("repos/%s/compare/main...%s", repo, branch),
		"--jq", ".commits[0].commit.message")
	if err != nil {
		return ""
	}
	subj := strings.TrimSpace(out)
	if i := strings.IndexByte(subj, '\n'); i >= 0 {
		subj = strings.TrimSpace(subj[:i])
	}
	return subj
}

// cleanIssueTitle strips a leading "[owner/repo#N] " bracket that orch inbox
// issues carry, so the fallback PR title isn't obviously non-conventional.
func cleanIssueTitle(t string) string {
	if strings.HasPrefix(t, "[") {
		if i := strings.Index(t, "] "); i >= 0 {
			return strings.TrimSpace(t[i+2:])
		}
	}
	return t
}

func ghAutoCreatePR(cfg *Config, n int, j *Job, is Issue) (int, error) {
	ahead, err := ghBranchAhead(j.TargetRepo, j.Branch)
	if err != nil || !ahead {
		return 0, err
	}
	// Title from the branch's commit subject — NOT the raw issue title, which
	// is often "[denoland/deno#NNNNN] <issue text>" and fails lint-title CI.
	title := prTitleFromBranch(j.TargetRepo, j.Branch)
	if title == "" {
		title = cleanIssueTitle(is.Title)
	}
	body := fmt.Sprintf("Closes %s#%d", cfg.GitHub.InboxRepo, n)
	out, errStr, err := run("gh", "pr", "create",
		"--repo", j.TargetRepo, "--head", j.Branch, "--base", "main",
		"--title", title, "--body", body)
	if err != nil {
		return 0, fmt.Errorf("gh pr create: %v: %s", err, errStr)
	}
	u := strings.TrimSpace(out)
	parts := strings.Split(u, "/")
	num, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0, fmt.Errorf("parse PR number from %q: %w", u, err)
	}
	log.Printf("issue #%d: auto-created PR #%d (%s)", n, num, u)
	return num, nil
}

type StatusCheck struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	CompletedAt string `json:"completedAt"`
}

type PRView struct {
	State          string                 `json:"state"`
	HeadRefOid     string                 `json:"headRefOid"`
	ReviewDecision string                 `json:"reviewDecision"` // "CHANGES_REQUESTED" | "APPROVED" | "REVIEW_REQUIRED" | ""
	Author         struct{ Login string } `json:"author"`
	Body           string                 `json:"body"`
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
	StatusCheckRollup []StatusCheck `json:"statusCheckRollup"`
	Mergeable         string        `json:"mergeable"`
	Commits           []struct {
		Oid     string `json:"oid"`
		Authors []struct {
			Login string `json:"login"`
		} `json:"authors"`
	} `json:"commits"`
}

const orchStatusOpen = "<!-- orchid-status -->"
const orchStatusClose = "<!-- /orchid-status -->"

// updatePRStatus writes (or replaces) a compact status line at the bottom of
// the PR description. Idempotent: skips the gh pr edit call when the status
// block is already up to date. Returns true if the body was changed.
func updatePRStatus(repo string, pr int, v *PRView, elapsed time.Duration) bool {
	// Build CI summary.
	pass, fail, pending := 0, 0, 0
	for _, c := range v.StatusCheckRollup {
		switch c.Conclusion {
		case "SUCCESS":
			pass++
		case "FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED":
			fail++
		default:
			pending++
		}
	}
	ciStr := "no CI"
	if pass+fail+pending > 0 {
		if fail > 0 {
			ciStr = fmt.Sprintf("❌ %d failing", fail)
		} else if pending > 0 {
			ciStr = fmt.Sprintf("🔄 %d/%d passing", pass, pass+pending)
		} else {
			ciStr = fmt.Sprintf("✅ %d passing", pass)
		}
	}
	reviewStr := ""
	switch v.ReviewDecision {
	case "APPROVED":
		reviewStr = " · ✅ approved"
	case "CHANGES_REQUESTED":
		reviewStr = " · 🔴 changes requested"
	case "REVIEW_REQUIRED":
		reviewStr = " · 👀 review requested"
	}
	elapsedStr := elapsed.Round(time.Minute).String()
	commits := len(v.Commits)
	statusLine := fmt.Sprintf("CI: %s · %d commit(s) · %s elapsed%s", ciStr, commits, elapsedStr, reviewStr)
	block := orchStatusOpen + "\n" + statusLine + "\n" + orchStatusClose

	body := v.Body
	if strings.Contains(body, orchStatusOpen) {
		// Already has block — replace it.
		start := strings.Index(body, orchStatusOpen)
		end := strings.Index(body, orchStatusClose)
		if end < 0 {
			end = len(body)
		} else {
			end += len(orchStatusClose)
		}
		existing := body[start:end]
		if existing == block {
			return false // no change
		}
		body = body[:start] + block + body[end:]
	} else {
		body = strings.TrimRight(body, "\n") + "\n\n---\n" + block
	}

	_, _, err := run("gh", "pr", "edit", fmt.Sprint(pr), "--repo", repo, "--body", body)
	return err == nil
}

func ghPRView(repo string, n int) (*PRView, error) {
	v, err := ghJSON[PRView]("pr", "view", fmt.Sprint(n), "--repo", repo,
		"--json", "state,headRefOid,reviewDecision,author,body,reviews,comments,statusCheckRollup,mergeable,commits")
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// orchPreflightPR creates the branch (with an empty placeholder commit) and
// opens a draft PR immediately — before the worker has written any code.
// This gives instant feedback: the PR exists within seconds of spawn so
// progress is visible on GitHub right away. The worker's real commits land
// on top of the placeholder commit; the draft PR auto-updates.
//
// Returns the PR number on success. Safe to retry: if the branch already
// exists it skips creation; if a PR already exists it returns its number.
func orchPreflightPR(repo, branch, inboxRepo string, issueN int, issueTitle, prDesc string) (int, error) {
	// 1. Default branch of the target repo.
	defaultBranch, _, err := run("gh", "api", fmt.Sprintf("repos/%s", repo), "--jq", ".default_branch")
	if err != nil {
		return 0, fmt.Errorf("get default branch: %v", err)
	}
	defaultBranch = strings.TrimSpace(defaultBranch)

	// 2. HEAD SHA of the default branch.
	baseSHA, _, err := run("gh", "api",
		fmt.Sprintf("repos/%s/branches/%s", repo, defaultBranch), "--jq", ".commit.sha")
	if err != nil {
		return 0, fmt.Errorf("get base sha: %v", err)
	}
	baseSHA = strings.TrimSpace(baseSHA)

	// 3. Tree SHA of that commit (re-use the same tree — zero diff).
	treeSHA, _, err := run("gh", "api",
		fmt.Sprintf("repos/%s/git/commits/%s", repo, baseSHA), "--jq", ".tree.sha")
	if err != nil {
		return 0, fmt.Errorf("get tree sha: %v", err)
	}
	treeSHA = strings.TrimSpace(treeSHA)

	// 4. Empty placeholder commit.
	commitJSON := fmt.Sprintf(`{"message":"wip: start %s#%d","tree":%q,"parents":[%q]}`,
		inboxRepo, issueN, treeSHA, baseSHA)
	commitSHA, _, err := runIn(commitJSON, "gh", "api", "--method", "POST",
		fmt.Sprintf("repos/%s/git/commits", repo), "--input", "-", "--jq", ".sha")
	if err != nil {
		return 0, fmt.Errorf("create placeholder commit: %v", err)
	}
	commitSHA = strings.TrimSpace(commitSHA)

	// 5. Create or update the branch ref.
	refJSON := fmt.Sprintf(`{"ref":"refs/heads/%s","sha":%q}`, branch, commitSHA)
	_, refErr, err := runIn(refJSON, "gh", "api", "--method", "POST",
		fmt.Sprintf("repos/%s/git/refs", repo), "--input", "-")
	if err != nil {
		// Branch may already exist (re-spawn). Try to fast-forward it.
		patchJSON := fmt.Sprintf(`{"sha":%q,"force":false}`, commitSHA)
		if _, _, e2 := runIn(patchJSON, "gh", "api", "--method", "PATCH",
			fmt.Sprintf("repos/%s/git/refs/heads/%s", repo, branch), "--input", "-"); e2 != nil {
			log.Printf("preflight: branch ref create failed (%s), patch also failed (%v)", strings.TrimSpace(refErr), e2)
			// Non-fatal: branch may exist from a previous spawn with real commits.
		}
	}

	// 6. Check if PR already exists.
	existing, _, _ := run("gh", "pr", "list", "--repo", repo, "--head", branch,
		"--state", "open", "--json", "number", "--jq", ".[0].number")
	if n, e := strconv.Atoi(strings.TrimSpace(existing)); e == nil && n > 0 {
		return n, nil
	}

	// 7. Open draft PR.
	prBody := prDesc
	if prBody == "" {
		prBody = issueTitle // bare fallback; triage description arrives later
	}
	prURL, _, err := run("gh", "pr", "create",
		"--repo", repo,
		"--head", branch,
		"--base", defaultBranch,
		"--draft",
		"--title", fmt.Sprintf("[WIP] %s", issueTitle),
		"--body", prBody)
	if err != nil {
		return 0, fmt.Errorf("gh pr create: %v", err)
	}
	// Parse PR number from URL: https://github.com/owner/repo/pull/N
	parts := strings.Split(strings.TrimSpace(prURL), "/")
	if len(parts) == 0 {
		return 0, fmt.Errorf("unexpected pr create output: %q", prURL)
	}
	n, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0, fmt.Errorf("parse pr number from %q: %v", prURL, err)
	}
	return n, nil
}
