package orch

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
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

func ghPRView(repo string, n int) (*PRView, error) {
	v, err := ghJSON[PRView]("pr", "view", fmt.Sprint(n), "--repo", repo,
		"--json", "state,headRefOid,reviewDecision,author,body,reviews,comments,statusCheckRollup,mergeable,commits")
	if err != nil {
		return nil, err
	}
	return &v, nil
}
