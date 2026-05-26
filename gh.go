package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
)

// Thin wrappers around the `gh` CLI. The CLI itself handles auth + the
// noise of GitHub's GraphQL/REST split — we just decode the JSON we asked
// for. Keep this file the only place that shells out to `gh` so the rest
// of the codebase has one place to look when a query needs adjusting.

// ghIssueList returns open issues in repo. If label is non-empty, restricts
// to that label; if empty, returns every open issue (used by tick to fetch
// the full inbox in one call instead of one-call-per-target).
func ghIssueList(repo, label string) ([]Issue, error) {
	args := []string{"issue", "list", "--repo", repo, "--state", "open",
		"--limit", "200", "--json", "number,title,body,state,labels"}
	if label != "" {
		args = append(args, "--label", label)
	}
	out, errStr, err := run("gh", args...)
	if err != nil {
		return nil, fmt.Errorf("gh issue list: %v: %s", err, errStr)
	}
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

// hasLabel reports whether the issue carries `name` in its labels list.
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

// ghAutoCreatePR opens a PR for the worker's branch when commits exist.
// Used when the worker's environment can't reach the API directly (e.g.
// clawpatrol's MITM blocks credential injection on api.github.com inside
// sessions) — central does it on the worker's behalf.
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
	CompletedAt string `json:"completedAt"` // RFC3339; latest run per name wins
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
	StatusCheckRollup []StatusCheck `json:"statusCheckRollup"`
	// Mergeable: GitHub returns "MERGEABLE", "CONFLICTING", or "UNKNOWN".
	// UNKNOWN means GitHub hasn't finished computing the merge — a few
	// seconds after the PR is viewed it usually settles.
	Mergeable string `json:"mergeable"`
	// Commits is the chronological commit list on the PR head branch.
	// Used to attribute new pushes so bot-authored pushes don't wake up
	// the session that just made them.
	Commits []struct {
		Oid     string `json:"oid"`
		Authors []struct {
			Login string `json:"login"`
		} `json:"authors"`
	} `json:"commits"`
}

func ghPRView(repo string, n int) (*PRView, error) {
	out, errStr, err := run("gh", "pr", "view", fmt.Sprint(n),
		"--repo", repo,
		"--json", "state,headRefOid,reviews,comments,statusCheckRollup,mergeable,commits")
	if err != nil {
		return nil, fmt.Errorf("gh pr view: %v: %s", err, errStr)
	}
	var v PRView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, err
	}
	return &v, nil
}
