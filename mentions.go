package main

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Mention + assignment watchers. Polls every configured target repo for
// @-mentions of any configured bot login and for issues newly assigned
// to one. Dispatches:
//   - mention on a tracked PR: poke that worker session
//   - mention by an org member: open an inbox issue (with an LLM-summarised
//     action title) and 👀-react on the source
//   - mention by an external user: canned reply on the source comment
//   - assignment to one of our bots: open an inbox issue routed to the
//     matching target label
//
// Both pollers run from the same outer ticker in main; mentions need an
// explicit `mentions {}` config block, assignments only need a bot_login.

type Mention struct {
	Repo       string    // owner/repo where the mention lives
	IsPR       bool      // true if the host is a PR, false for an issue
	Number     int       // issue or PR number on Repo
	HostURL    string    // canonical URL of the host issue/PR
	HostAuthor string    // login that opened the host issue/PR (used to detect bot-self mentions)
	CommentID  string    // unique GitHub node id of the comment carrying the mention
	CommentURL string    // direct link to that specific comment
	Author     string    // login that wrote the comment
	Body       string    // raw comment body
	CreatedAt  time.Time // comment creation time (used to advance the cursor)
	Bot        string    // which configured bot was mentioned
}

// isBotLogin returns true if the given GitHub login looks like a bot.
// Heuristic order:
//
//  1. Any login configured as one of orchid's own bots (orch + per-VM
//     bot_login fields) is always a bot.
//  2. mentions.human_overrides force-classify a login as human even if
//     the heuristic below would mark it otherwise.
//  3. Otherwise, treat as bot if the login contains "bot"
//     (case-insensitive). Catches `crowlbot`, `denobot`, `nathanwhitbot`,
//     `avocet-bot`, etc. — including org-member bots that
//     `orgs/<org>/members` returns alongside humans.
//
// Caller is responsible for the consequences: dispatch skips both
// comment authors and host (issue/PR) authors classified as bots, so a
// false-positive here means a real human's mention gets ignored. Use
// human_overrides for any human whose login happens to contain "bot".
func isBotLogin(cfg *Config, login string) bool {
	for _, b := range botLogins(cfg) {
		if login == b {
			return true
		}
	}
	if cfg.Orch.Mentions != nil {
		for _, h := range cfg.Orch.Mentions.HumanOverrides {
			if login == h {
				return false
			}
		}
	}
	return strings.Contains(strings.ToLower(login), "bot")
}

// botLogins gathers all unique bot logins across the orchestrator default
// and per-VM overrides. These are the @-mentions we look for.
func botLogins(cfg *Config) []string {
	seen := map[string]bool{}
	if cfg.Orch.BotLogin != "" {
		seen[cfg.Orch.BotLogin] = true
	}
	for _, vm := range cfg.VMs {
		if vm.BotLogin != "" {
			seen[vm.BotLogin] = true
		}
	}
	out := make([]string, 0, len(seen))
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// fetchMaintainers pulls the full membership list of the configured org.
// Hard-fails (returns error) if the orch token lacks read:org or isn't in
// the org — there's no quiet fallback because misclassifying members as
// external would silently downgrade their requests to canned replies.
func fetchMaintainers(org string) ([]string, error) {
	out, errStr, err := run("gh", "api", "--paginate",
		fmt.Sprintf("orgs/%s/members", org),
		"--jq", ".[].login")
	if err != nil {
		return nil, fmt.Errorf("gh api orgs/%s/members: %v: %s", org, err, strings.TrimSpace(errStr))
	}
	var members []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			members = append(members, line)
		}
	}
	return members, nil
}

// refreshMaintainers replaces the cache if it's missing or older than ttl.
// The fetched member list is filtered through isBotLogin — bots that
// happen to be org members would otherwise be classified as
// "maintainers" and route their automated mentions to inbox-issue
// creation, causing bot-on-bot dispatch loops.
// Caller must hold st.mu.
func refreshMaintainers(cfg *Config, st *State, ttl time.Duration) error {
	if st.Maintainers != nil && time.Since(st.Maintainers.FetchedAt) < ttl {
		return nil
	}
	members, err := fetchMaintainers(cfg.Orch.Mentions.Org)
	if err != nil {
		return err
	}
	humans := make([]string, 0, len(members))
	for _, m := range members {
		if !isBotLogin(cfg, m) {
			humans = append(humans, m)
		}
	}
	st.Maintainers = &MaintainerCache{FetchedAt: time.Now(), Members: humans}
	log.Printf("mentions: refreshed maintainer cache for %s (%d humans, %d bots filtered)",
		cfg.Orch.Mentions.Org, len(humans), len(members)-len(humans))
	_ = saveState(st)
	return nil
}

// searchMentions returns mentions of `bot` in `repo` (issues + PRs) where
// the comment was created strictly after `since`. Two-stage: search the
// issue/PR set first, then fetch comments per item to find the specific
// commenting events.
func searchMentions(repo, bot string, since time.Time) ([]Mention, error) {
	type item struct {
		Number int                    `json:"number"`
		URL    string                 `json:"url"`
		Author struct{ Login string } `json:"author"`
		IsPR   bool
	}
	collect := func(kind string) ([]item, error) {
		out, errStr, err := run("gh", "search", kind,
			"mentions:"+bot,
			"repo:"+repo,
			"updated:>="+since.UTC().Format("2006-01-02"),
			"--limit", "100",
			"--json", "number,url,author")
		if err != nil {
			return nil, fmt.Errorf("gh search %s: %v: %s", kind, err, strings.TrimSpace(errStr))
		}
		var items []item
		if err := json.Unmarshal([]byte(out), &items); err != nil {
			return nil, err
		}
		for i := range items {
			items[i].IsPR = (kind == "prs")
		}
		return items, nil
	}
	issues, err := collect("issues")
	if err != nil {
		return nil, err
	}
	prs, err := collect("prs")
	if err != nil {
		return nil, err
	}
	all := append(issues, prs...)

	type ghComment struct {
		ID        string                 `json:"id"`
		URL       string                 `json:"url"`
		Body      string                 `json:"body"`
		CreatedAt time.Time              `json:"createdAt"`
		Author    struct{ Login string } `json:"author"`
	}
	mentionTag := "@" + bot
	var mentions []Mention
	for _, it := range all {
		viewKind := "issue"
		if it.IsPR {
			viewKind = "pr"
		}
		out, errStr, err := run("gh", viewKind, "view", fmt.Sprint(it.Number),
			"--repo", repo, "--json", "comments", "--jq", ".comments")
		if err != nil {
			log.Printf("mentions: %s view %s#%d: %v: %s", viewKind, repo, it.Number, err, strings.TrimSpace(errStr))
			continue
		}
		var comments []ghComment
		if err := json.Unmarshal([]byte(out), &comments); err != nil {
			log.Printf("mentions: parse comments for %s#%d: %v", repo, it.Number, err)
			continue
		}
		for _, c := range comments {
			if !c.CreatedAt.After(since) {
				continue
			}
			if !strings.Contains(c.Body, mentionTag) {
				continue
			}
			mentions = append(mentions, Mention{
				Repo: repo, IsPR: it.IsPR, Number: it.Number,
				HostURL: it.URL, HostAuthor: it.Author.Login,
				CommentID: c.ID, CommentURL: c.URL,
				Author: c.Author.Login, Body: c.Body,
				CreatedAt: c.CreatedAt, Bot: bot,
			})
		}
	}
	return mentions, nil
}

// inferMentionAction asks an LLM (run on the orch host) to either (a)
// summarize what action the comment is requesting, in ≤15 words, or
// (b) return the literal "NOOP" if the comment is purely informational
// (status update, FYI, automated bot chatter, etc.). Defaults to
// `claude -p`; configurable via mentions.llm_command (e.g. ["codex","exec"]
// to keep claude budget for workers).
//
// Returns the summary if the comment is actionable, or "" if NOOP /
// the model call failed. Caller treats "" as "skip dispatch" — better
// to silently ignore an ambiguous mention than to spam the inbox with
// no-op tracking issues.
func inferMentionAction(cfg *Config, m Mention) string {
	// Cheap pre-filter for obviously-NOOP comments: short acks like "thanks",
	// "lgtm", emoji-only, +1, etc. Skips the ~5s LLM call entirely for the
	// common-case noise. We only match SHORT bodies so a long "thanks for
	// fixing X, the symptom was Y" still falls through to the LLM which can
	// pick out an embedded request.
	if isShortNoop(m.Body) {
		return ""
	}
	prompt := fmt.Sprintf(`Read the GitHub comment below. Decide whether it is asking the bot to perform a specific, concrete action (review, rebase, fix, investigate, address feedback, retry, look at, etc.).

Reply with EXACTLY one line:
  - If it IS an actionable request, reply with the action in 15 words or fewer. No preamble, no quoting.
  - If it is informational only (status update, FYI, thanks, summary of work the commenter already did, automated bot output, etc.), reply with the literal word: NOOP

Comment from @%s in %s#%d:
%s`, m.Author, m.Repo, m.Number, m.Body)
	llmCmd := []string{"claude", "-p"}
	if cfg.Orch.Mentions != nil && len(cfg.Orch.Mentions.LLMCommand) > 0 {
		llmCmd = cfg.Orch.Mentions.LLMCommand
	}
	out, _, err := runIn(prompt, llmCmd[0], llmCmd[1:]...)
	if err != nil {
		log.Printf("mentions: %s failed for %s: %v (treating as NOOP to avoid spam)", strings.Join(llmCmd, " "), m.CommentURL, err)
		return ""
	}
	summary := strings.TrimSpace(out)
	if i := strings.IndexByte(summary, '\n'); i >= 0 {
		summary = strings.TrimSpace(summary[:i])
	}
	if summary == "" || strings.EqualFold(summary, "NOOP") {
		return ""
	}
	return summary
}

// shortNoopPattern matches very-short ack/thanks/FYI bodies that are almost
// never actionable. Anchored, case-insensitive, allows leading @-mention and
// trailing punctuation/emoji. Tested in orch_test.go.
var shortNoopPattern = regexp.MustCompile(
	`(?i)^\s*(@\S+\s+)?(thanks?|thx|ty|tysm|cheers|nice|cool|lgtm|ok(ay)?|sgtm|got\s?it|fyi|np|nm|\+1|sounds?\s+good|will\s+do|done|ack(nowledged)?)\b[\s!.,?\W]*$`,
)

// isShortNoop returns true if the comment body is short enough and matches
// an obviously-noop pattern. Used by inferMentionAction to skip the LLM
// call for the common noise case.
func isShortNoop(body string) bool {
	t := strings.TrimSpace(body)
	if len(t) > 80 {
		return false
	}
	return shortNoopPattern.MatchString(t)
}

// targetLabelFor returns the routing label whose target.repo matches repo.
// Empty string if no target points at that repo.
func targetLabelFor(cfg *Config, repo string) string {
	for _, t := range cfg.Targets {
		if t.Repo == repo {
			return t.Label
		}
	}
	return ""
}

// dispatchMention is the policy split: classify the mention into one of
// three buckets and act on it. Caller must hold st.mu.
func dispatchMention(cfg *Config, st *State, m Mention) {
	// Bot filters first — drop anything bot-authored.
	if isBotLogin(cfg, m.Author) {
		return
	}
	// Host-author bot filter: skip if the issue/PR was opened by a
	// THIRD-PARTY bot (e.g. dependabot, crowlbot). PRs opened by OUR
	// own bots are exempt — a human pinging us on one of our own PRs
	// is exactly the case we want to handle (tracked-PR poke, or
	// maintainer-routed inbox issue if the PR is no longer tracked).
	if isBotLogin(cfg, m.HostAuthor) {
		ourBot := false
		for _, b := range botLogins(cfg) {
			if m.HostAuthor == b {
				ourBot = true
				break
			}
		}
		if !ourBot {
			return
		}
	}

	// Mention on a PR orch already tracks → poke that worker session.
	// Skip the LLM gate here; the worker decides whether the comment is
	// actionable as part of its existing review-handling.
	if m.IsPR {
		for n, j := range st.Jobs {
			if j.PR != m.Number || j.TargetRepo != m.Repo {
				continue
			}
			vm := vmByName(cfg, j.VM)
			if vm == nil {
				return
			}
			msg := fmt.Sprintf("New @-mention by @%s on PR #%d (%s):\n\n%s", m.Author, m.Number, m.CommentURL, oneLine(m.Body, 400))
			if err := tmuxPaste(*vm, j.Tmux, msg); err != nil {
				log.Printf("mentions: poke #%d failed: %v", n, err)
				return
			}
			log.Printf("mentions: poked tracked PR session for issue #%d (PR #%d, by @%s)", n, m.Number, m.Author)
			return
		}
	}

	// LLM gate: only continue if the comment is actually requesting work.
	// Returns "" for NOOP (informational, status, FYI) — silently skip.
	summary := inferMentionAction(cfg, m)
	if summary == "" {
		log.Printf("mentions: skipping non-actionable mention from @%s in %s#%d", m.Author, m.Repo, m.Number)
		return
	}

	// Maintainer (org-member, post-bot-filter) → open inbox issue with the
	// LLM-summarized action as the title.
	if st.Maintainers.has(m.Author) {
		label := targetLabelFor(cfg, m.Repo)
		if label == "" {
			log.Printf("mentions: maintainer mention in %s but no matching target label, falling back to external reply", m.Repo)
		} else {
			title := fmt.Sprintf("@%s: %s", m.Author, summary)
			body := fmt.Sprintf("Triggered by mention in %s.\n\n@%s wrote:\n\n> %s\n\nInferred action: %s",
				m.CommentURL, m.Author, oneLine(m.Body, 800), summary)
			out, errStr, err := run("gh", "issue", "create",
				"--repo", cfg.GitHub.InboxRepo,
				"--label", label,
				"--title", title,
				"--body", body)
			if err != nil {
				log.Printf("mentions: gh issue create failed for %s: %v: %s", m.CommentURL, err, strings.TrimSpace(errStr))
				return
			}
			newURL := strings.TrimSpace(out)
			parts := strings.Split(newURL, "/")
			newNum, _ := strconv.Atoi(parts[len(parts)-1])
			log.Printf("mentions: opened inbox issue #%d for maintainer @%s mention in %s#%d",
				newNum, m.Author, m.Repo, m.Number)
			// Append explicit close instructions now that we know the new
			// issue number. Mention-routed inbox issues are review/response
			// tasks — there is no PR to ship, so nothing closes the inbox
			// issue automatically and the worker session would otherwise
			// linger forever waiting for follow-up that never comes.
			closeInstr := fmt.Sprintf(
				"\n\n---\n\n## When done\n\nThis is a review/response task — there is no PR to ship from this issue. After you've posted your response on the source comment/PR, close this inbox issue and exit:\n\n```sh\ngh issue close --repo %s %d --comment \"done\"\n```\n",
				cfg.GitHub.InboxRepo, newNum)
			if _, errStr, err := run("gh", "issue", "edit", fmt.Sprint(newNum),
				"--repo", cfg.GitHub.InboxRepo,
				"--body", body+closeInstr); err != nil {
				log.Printf("mentions: append close instructions to inbox #%d failed: %v: %s", newNum, err, strings.TrimSpace(errStr))
			}
			if cfg.Orch.Mentions.Acknowledge {
				// React to the mentioning comment with 👀 instead of posting
				// a "Tracking in inbox#N" reply — GitHub already auto-creates
				// a "mentioned in" backlink on the source comment when we
				// reference its URL in the new inbox issue body, so the
				// comment is needless noise. The reaction is a quieter ack.
				_, _, err := run("gh", "api", "graphql", "-f",
					fmt.Sprintf("query=mutation { addReaction(input: {subjectId: %q, content: EYES}) { reaction { content } } }", m.CommentID))
				if err != nil {
					log.Printf("mentions: eyes reaction on %s failed: %v", m.CommentURL, err)
				}
			}
			return
		}
	}

	// External user → canned reply on source.
	reply := fmt.Sprintf("Hi @%s — I'm an automated bot. @bartlomieju has been notified and will follow up.", m.Author)
	kind := "issue"
	if m.IsPR {
		kind = "pr"
	}
	if _, _, err := run("gh", kind, "comment", fmt.Sprint(m.Number), "--repo", m.Repo, "--body", reply); err != nil {
		log.Printf("mentions: external reply on %s#%d failed: %v", m.Repo, m.Number, err)
		return
	}
	log.Printf("mentions: replied to external @%s in %s#%d", m.Author, m.Repo, m.Number)
}

// assignedIssue is one row from `gh search issues assignee:<bot>`. The
// node_id is what we dedupe against — GitHub recycles issue numbers per
// repo, but node ids are globally unique and stable across renames.
type assignedIssue struct {
	NodeID    string
	Repo      string
	Number    int
	Title     string
	Body      string
	URL       string
	Author    string
	UpdatedAt time.Time
}

// searchAssignments returns open issues in `repo` currently assigned to
// `bot`. Same gh search surface mentions uses, just filtered on
// `assignee:` instead of comment-mention. Caller dedupes via node id
// so a re-poll never re-creates the inbox issue.
func searchAssignments(repo, bot string) ([]assignedIssue, error) {
	out, errStr, err := run("gh", "api",
		"-H", "Accept: application/vnd.github+json",
		"--paginate",
		fmt.Sprintf("/search/issues?q=repo:%s+assignee:%s+is:issue+is:open&per_page=50", repo, bot),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(errStr))
	}
	var resp struct {
		Items []struct {
			NodeID    string    `json:"node_id"`
			Number    int       `json:"number"`
			Title     string    `json:"title"`
			Body      string    `json:"body"`
			HTMLURL   string    `json:"html_url"`
			User      struct{ Login string } `json:"user"`
			UpdatedAt time.Time `json:"updated_at"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, err
	}
	res := make([]assignedIssue, 0, len(resp.Items))
	for _, it := range resp.Items {
		res = append(res, assignedIssue{
			NodeID:    it.NodeID,
			Repo:      repo,
			Number:    it.Number,
			Title:     it.Title,
			Body:      it.Body,
			URL:       it.HTMLURL,
			Author:    it.User.Login,
			UpdatedAt: it.UpdatedAt,
		})
	}
	return res, nil
}

// assignmentTick scans every target repo for issues newly assigned to
// one of orchid's bots and opens a corresponding inbox issue so the
// swarm picks them up. Dedupes against a seen-node-id set stored in
// the kv table — a re-poll, a restart, or an unassign+reassign never
// double-files. Same cadence as the mention poller; both run from the
// same outer loop in main.
func assignmentTick(cfg *Config, st *State) {
	st.mu.Lock()
	defer st.mu.Unlock()

	// Load the persisted seen-set. Cap to ~5000 entries so a long-lived
	// orch doesn't bloat the kv row indefinitely.
	seen := map[string]bool{}
	if b, err := st.store.GetKV("assignments_seen"); err == nil && len(b) > 0 {
		var ids []string
		if json.Unmarshal(b, &ids) == nil {
			for _, id := range ids {
				seen[id] = true
			}
		}
	}

	bots := botLogins(cfg)
	added := false
	for _, t := range cfg.Targets {
		if t.Repo == cfg.GitHub.InboxRepo {
			continue
		}
		label := targetLabelFor(cfg, t.Repo)
		if label == "" {
			continue
		}
		for _, bot := range bots {
			issues, err := searchAssignments(t.Repo, bot)
			if err != nil {
				log.Printf("assignments: search %s × %s failed: %v", t.Repo, bot, err)
				continue
			}
			for _, it := range issues {
				if seen[it.NodeID] {
					continue
				}
				title := fmt.Sprintf("[%s#%d] %s", it.Repo, it.Number, it.Title)
				body := fmt.Sprintf(
					"Triggered by assignment of [%s#%d](%s) to @%s.\n\nOpened by @%s.\n\n---\n\n%s",
					it.Repo, it.Number, it.URL, bot, it.Author, oneLine(it.Body, 2000),
				)
				out, errStr, err := run("gh", "issue", "create",
					"--repo", cfg.GitHub.InboxRepo,
					"--label", label,
					"--title", title,
					"--body", body)
				if err != nil {
					log.Printf("assignments: gh issue create failed for %s#%d: %v: %s",
						it.Repo, it.Number, err, strings.TrimSpace(errStr))
					continue
				}
				seen[it.NodeID] = true
				added = true
				log.Printf("assignments: opened inbox issue for %s#%d (assignee=%s) → %s",
					it.Repo, it.Number, bot, strings.TrimSpace(out))
			}
		}
	}

	if added {
		ids := make([]string, 0, len(seen))
		for id := range seen {
			ids = append(ids, id)
		}
		// Trim oldest if we ever grow beyond the cap — simple slice cut,
		// no need for an ordered structure for what's effectively a
		// bloom filter at this scale.
		if len(ids) > 5000 {
			ids = ids[len(ids)-5000:]
		}
		if b, err := json.Marshal(ids); err == nil {
			_ = st.store.PutKV("assignments_seen", b)
		}
	}
}

// mentionTick runs one polling cycle: refresh the cache if stale, then
// search each (target.repo × bot) pair for new mentions and dispatch.
func mentionTick(cfg *Config, st *State) {
	st.mu.Lock()
	defer st.mu.Unlock()

	ttl, _ := time.ParseDuration(cfg.Orch.Mentions.MaintainerTTL)
	if ttl == 0 {
		ttl = 1 * time.Hour
	}
	if err := refreshMaintainers(cfg, st, ttl); err != nil {
		log.Printf("mentions: maintainer refresh failed (skipping cycle): %v", err)
		return
	}

	// Cursor: floor at orch boot time. On a fresh process (cursor nil)
	// we start at boot time; on a restart with a cursor that predates
	// boot (long downtime, stale state) we still floor at boot. This
	// is the deliberate trade: missing pre-boot mentions is acceptable
	// — silently re-dispatching them on every restart is not.
	since := orchBootTime
	if st.MentionCursor != nil && st.MentionCursor.After(orchBootTime) {
		since = *st.MentionCursor
	}

	bots := botLogins(cfg)
	seen := map[string]bool{}
	maxSeen := since
	for _, t := range cfg.Targets {
		// Skip the inbox repo itself — mentions there are noise
		// (operator chatter on tracking issues we ourselves created),
		// and the search counts against the same rate limit.
		if t.Repo == cfg.GitHub.InboxRepo {
			continue
		}
		for _, b := range bots {
			ms, err := searchMentions(t.Repo, b, since)
			if err != nil {
				log.Printf("mentions: search %s × %s failed: %v", t.Repo, b, err)
				continue
			}
			for _, m := range ms {
				if seen[m.CommentID] {
					continue // same comment found via multiple bot searches
				}
				seen[m.CommentID] = true
				dispatchMention(cfg, st, m)
				if m.CreatedAt.After(maxSeen) {
					maxSeen = m.CreatedAt
				}
			}
		}
	}
	if maxSeen.After(since) {
		st.MentionCursor = &maxSeen
		_ = saveState(st)
	}
}
