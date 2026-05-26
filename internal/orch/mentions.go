package orch

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

type Mention struct {
	Repo       string
	IsPR       bool
	Number     int
	HostURL    string
	HostAuthor string
	CommentID  string
	CommentURL string
	Author     string
	Body       string
	CreatedAt  time.Time
	Bot        string
}

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

func inferMentionAction(cfg *Config, m Mention) string {
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
	if isBotLogin(cfg, m.Author) {
		return
	}
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

	summary := inferMentionAction(cfg, m)
	if summary == "" {
		log.Printf("mentions: skipping non-actionable mention from @%s in %s#%d", m.Author, m.Repo, m.Number)
		return
	}

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
			closeInstr := fmt.Sprintf(
				"\n\n---\n\n## When done\n\nThis is a review/response task — there is no PR to ship from this issue. After you've posted your response on the source comment/PR, close this inbox issue and exit:\n\n```sh\ngh issue close --repo %s %d --comment \"done\"\n```\n",
				cfg.GitHub.InboxRepo, newNum)
			if _, errStr, err := run("gh", "issue", "edit", fmt.Sprint(newNum),
				"--repo", cfg.GitHub.InboxRepo,
				"--body", body+closeInstr); err != nil {
				log.Printf("mentions: append close instructions to inbox #%d failed: %v: %s", newNum, err, strings.TrimSpace(errStr))
			}
			if cfg.Orch.Mentions.Acknowledge {
				_, _, err := run("gh", "api", "graphql", "-f",
					fmt.Sprintf("query=mutation { addReaction(input: {subjectId: %q, content: EYES}) { reaction { content } } }", m.CommentID))
				if err != nil {
					log.Printf("mentions: eyes reaction on %s failed: %v", m.CommentURL, err)
				}
			}
			return
		}
	}

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

func assignmentTick(cfg *Config, st *State) {
	st.mu.Lock()
	defer st.mu.Unlock()

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

	since := orchBootTime
	if st.MentionCursor != nil && st.MentionCursor.After(orchBootTime) {
		since = *st.MentionCursor
	}

	bots := botLogins(cfg)
	seen := map[string]bool{}
	maxSeen := since
	for _, t := range cfg.Targets {
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
					continue
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
