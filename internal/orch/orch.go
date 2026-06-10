package orch

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	GitHub              GitHubBlock    `hcl:"github,block" json:"github"`
	Orch                OrchBlock      `hcl:"orchestrator,block" json:"orchestrator"`
	BootstrapPrompt     string         `hcl:"bootstrap_prompt" json:"bootstrap_prompt"`
	CronBootstrapPrompt string         `hcl:"cron_bootstrap_prompt,optional" json:"cron_bootstrap_prompt,omitempty"`
	Targets             []TargetBlock  `hcl:"target,block" json:"targets"`
	VMs                 []VMBlock      `hcl:"vm,block" json:"vms"`
	Machines            []MachineBlock `hcl:"machine,block" json:"machines,omitempty"`
}

// MachineBlock is a physical host that runs one or more agent "slots". It's
// sugar over VMBlock: a machine with N `agent` blocks expands (at load, via
// expandMachines) into N VMBlocks sharing the host/ssh/home, so you define a box
// once instead of one `vm` block per agent+account. Plain `vm` blocks still work.
type MachineBlock struct {
	Name        string         `hcl:",label" json:"name"`
	Host        string         `hcl:"host" json:"host"`
	User        string         `hcl:"user,optional" json:"user,omitempty"`
	Key         string         `hcl:"key,optional" json:"key,omitempty"`
	SessionHome string         `hcl:"session_home,optional" json:"session_home,omitempty"`
	WorkdirRoot string         `hcl:"workdir_root,optional" json:"workdir_root,omitempty"`
	BotLogin    string         `hcl:"bot_login,optional" json:"bot_login,omitempty"`
	BotEmail    string         `hcl:"bot_email,optional" json:"bot_email,omitempty"`
	Sccache     bool           `hcl:"sccache,optional" json:"sccache,omitempty"`
	SccacheDir  string         `hcl:"sccache_dir,optional" json:"sccache_dir,omitempty"`
	JoinManaged bool           `hcl:"join_managed,optional" json:"join_managed,omitempty"`
	Agents      []MachineAgent `hcl:"agent,block" json:"agents"`
}

// MachineAgent is one agent slot on a machine. The label is the agent type
// (claude|codex); account defaults to the agent (set it to run a second account
// of the same agent, e.g. codex + codex-mini).
type MachineAgent struct {
	Agent      string `hcl:",label" json:"agent"`
	Name       string `hcl:"name,optional" json:"name,omitempty"` // override expanded VM name (default <machine>-<account>); set it to keep a name stable across a migration
	Account    string `hcl:"account,optional" json:"account,omitempty"`
	Capacity   int    `hcl:"capacity,optional" json:"capacity,omitempty"`
	CodexHome  string `hcl:"codex_home,optional" json:"codex_home,omitempty"`
	SessionCmd string `hcl:"session_cmd,optional" json:"session_cmd,omitempty"`
}

// expandMachines desugars `machine` blocks into VMBlocks appended to cfg.VMs.
// Each agent slot becomes a VM named "<machine>-<account>" (account defaults to
// the agent type). Idempotent-ish: call once after each config decode.
func expandMachines(cfg *Config) {
	for _, m := range cfg.Machines {
		for _, a := range m.Agents {
			acct := a.Account
			if acct == "" {
				acct = a.Agent
			}
			name := a.Name
			if name == "" {
				name = m.Name + "-" + acct
			}
			cfg.VMs = append(cfg.VMs, VMBlock{
				Name:        name,
				Host:        m.Host,
				User:        m.User,
				Key:         m.Key,
				Capacity:    a.Capacity,
				Sccache:     m.Sccache,
				SccacheDir:  m.SccacheDir,
				SessionCmd:  a.SessionCmd,
				SessionHome: m.SessionHome,
				WorkdirRoot: m.WorkdirRoot,
				BotLogin:    m.BotLogin,
				BotEmail:    m.BotEmail,
				Agent:       a.Agent,
				Account:     a.Account,
				CodexHome:   a.CodexHome,
				JoinManaged: m.JoinManaged,
			})
		}
	}
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
	BotGithubKey  string         `hcl:"bot_github_key,optional" json:"bot_github_key,omitempty"`
	Mentions      *MentionsBlock `hcl:"mentions,block" json:"mentions,omitempty"`
	Throttle      *ThrottleBlock `hcl:"throttle,block" json:"throttle,omitempty"`
	Memory        *MemoryBlock   `hcl:"memory,block" json:"memory,omitempty"`
	// TriageCmd runs a one-shot agent (e.g. `claude -p` behind clawpatrol) for
	// the discovery passes: issue triage comments and PR postmortem lessons.
	// The prompt arrives on stdin; stdout is the report. Empty disables both.
	TriageCmd string `hcl:"triage_cmd,optional" json:"triage_cmd,omitempty"`
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
	Name        string `hcl:",label" json:"name"`
	Host        string `hcl:"host" json:"host"`
	User        string `hcl:"user,optional" json:"user,omitempty"`
	Key         string `hcl:"key,optional" json:"key,omitempty"`
	Capacity    int    `hcl:"capacity,optional" json:"capacity,omitempty"`
	Sccache     bool   `hcl:"sccache,optional" json:"sccache,omitempty"`
	SccacheDir  string `hcl:"sccache_dir,optional" json:"sccache_dir,omitempty"`
	SessionCmd  string `hcl:"session_cmd,optional" json:"session_cmd,omitempty"`
	SessionHome string `hcl:"session_home,optional" json:"session_home,omitempty"`
	WorkdirRoot string `hcl:"workdir_root,optional" json:"workdir_root,omitempty"`
	BotLogin    string `hcl:"bot_login,optional" json:"bot_login,omitempty"`
	BotEmail    string `hcl:"bot_email,optional" json:"bot_email,omitempty"`
	Agent       string `hcl:"agent,optional" json:"agent,omitempty"`
	// Account is the billing/metering identity this VM's sessions count against
	// — the key the quota sampler + governor pace independently. Defaults to the
	// agent name. Set it to run TWO accounts of the same agent (e.g. two codex
	// logins on one host: agent="codex" for both, account="codex"/"codex-mini",
	// each with its own CodexHome). Surfaced per-account on the dashboard.
	Account string `hcl:"account,optional" json:"account,omitempty"`
	// CodexHome overrides the codex CLI's home dir (the CODEX_HOME env var:
	// auth.json + config.toml + sessions/ rollouts) for this VM, so a second
	// codex account on the same host-user has isolated auth + usage telemetry.
	// Empty => the agent's default (~/.codex). Honored in the session_cmd AND
	// the trust-stamp/usage-tailer. May contain $HOME (expanded by the shell).
	CodexHome       string `hcl:"codex_home,optional" json:"codex_home,omitempty"`
	IdleMarker      string `hcl:"idle_marker,optional" json:"idle_marker,omitempty"`
	BusyMarker      string `hcl:"busy_marker,optional" json:"busy_marker,omitempty"`
	BootstrapPrompt string `hcl:"bootstrap_prompt,optional" json:"bootstrap_prompt,omitempty"`
	JoinManaged     bool   `hcl:"join_managed,optional" json:"join_managed,omitempty"`
}

// Job lifecycle: "oneshot" (default) — issue → session → PR → teardown.
// "cron" — issue stays open, ephemeral session fires every Schedule, no PR.
// prTracker holds the per-PR "what have we already relayed to the worker"
// state used by diffPR. Shared by a job's primary PR and each upstream PR it
// tracks. Embedded into Job anonymously so its fields stay top-level in the
// persisted JSON (backward compatible with pre-ExtraPRs state.db rows).
type prTracker struct {
	SeenReviewIDs        []string          `json:"seen_review_ids,omitempty"`
	SeenThreadCommentIDs []string          `json:"seen_thread_comment_ids,omitempty"`
	SeenIssueCommentIDs  []string          `json:"seen_issue_comment_ids,omitempty"`
	LastHeadOID          string            `json:"last_head_oid,omitempty"`
	LastCheckConclusions map[string]string `json:"last_check_conclusions,omitempty"`
	LastMergeable        string            `json:"last_mergeable,omitempty"`
}

// ExtraPR is an upstream / dependency PR (in a repo other than the job's
// target) that a session opened as part of its work. orch watches it like the
// primary PR — relaying new reviews/comments/CI back — but only after
// confirming our bot authored it. Discovered from the primary PR body's
// cross-repo links.
type ExtraPR struct {
	Repo      string `json:"repo"`   // owner/repo
	Number    int    `json:"number"` // PR number in Repo
	Validated bool   `json:"validated,omitempty"`
	prTracker
}

type Job struct {
	VM            string    `json:"vm"`
	Tmux          string    `json:"tmux"`
	Target        string    `json:"target"`
	TargetRepo    string    `json:"target_repo"`
	Branch        string    `json:"branch"`
	IssueTitle    string    `json:"issue_title,omitempty"`
	IssueGoal     string    `json:"issue_goal,omitempty"` // issue body excerpt, re-injected on every poke
	Lifecycle     string    `json:"lifecycle,omitempty"`
	Schedule      string    `json:"schedule,omitempty"`
	Timeout       string    `json:"timeout,omitempty"`
	NextFireAt    time.Time `json:"next_fire_at,omitempty"`
	FireStartedAt time.Time `json:"fire_started_at,omitempty"`
	PR            int       `json:"pr,omitempty"`
	prTracker               // seen review/comment IDs, last head OID, check conclusions, mergeable — flattened into Job's JSON, so existing state.db jobs deserialize unchanged
	// ExtraPRs are upstream / dependency PRs this session opened in OTHER repos
	// (e.g. a dprint PR backing a deno fix). orch watches them too and feeds
	// their reviews/comments/CI back to the worker — see issue #1.
	ExtraPRs []ExtraPR `json:"extra_prs,omitempty"`
	// IgnoredPRs are cross-repo refs we discovered but stopped tracking (not
	// authored by our bot, or already merged/closed). Kept so discoverExtraPRs
	// doesn't re-add a ref that's still sitting in the PR body each tick.
	IgnoredPRs []string `json:"ignored_prs,omitempty"`

	// Proactive pacing governor fields (see governor.go). All persist for free
	// via SaveState's json blob. Priority is parsed from the issue body's toml
	// frontmatter ("priority = N", higher = more important; default 0 / the
	// configured DefaultPriority). Paused/PausedAt track a duty-cycle SIGSTOP:
	// when the governor freezes a session's token burn it sets Paused=true and
	// stamps PausedAt; the reconcile pass clears them on resume or when the
	// pane is gone, so a stopped session is never stranded across a restart.
	Priority int       `json:"priority,omitempty"`
	Paused   bool      `json:"paused,omitempty"`
	PausedAt time.Time `json:"paused_at,omitempty"`

	// Token-saving bookkeeping (see tick.go). SpawnedAt anchors the session-age
	// cap (zero is grandfathered to now on first sighting so existing sessions
	// aren't all cycled at once). LastPokeAt debounces review/CI pokes.
	// LastClearAt is the cooldown so a freshly /clear'd session isn't cycled
	// again immediately.
	SpawnedAt   time.Time `json:"spawned_at,omitempty"`
	LastPokeAt  time.Time `json:"last_poke_at,omitempty"`
	LastClearAt time.Time `json:"last_clear_at,omitempty"`

	// Review/CI resilience (see tick.go). An outstanding CHANGES_REQUESTED review
	// or failing CI is "addressed" only when a new commit is pushed — until then
	// orch keeps re-surfacing it (so a poke that landed in a stuck/dead pane is
	// retried, not silently dropped). RelayHead is the PR head OID when we last
	// re-surfaced an outstanding item; RelayPokes counts those re-surfaces so we
	// escalate to a human instead of looping forever.
	RelayHead  string `json:"relay_head,omitempty"`
	RelayPokes int    `json:"relay_pokes,omitempty"`
}

type State struct {
	mu            sync.Mutex
	Jobs          map[int]*Job
	MentionCursor *time.Time
	Maintainers   *MaintainerCache
	store         *Store
	httpSnap      atomic.Value
	Bcast         chan struct{} `json:"-"`

	// Monotonic counter for synthetic ids on adhoc jobs (orch run).
	// Decrements without ever colliding even when concurrent calls
	// happen while tick() holds st.mu. Initialised from min(Jobs key)
	// at startup so restarts don't reuse ids.
	adhocSeq atomic.Int64

	// VM reachability snapshot, updated by the SSH probe loop. Not
	// persisted — a fresh process starts with everything unknown and
	// fills in within one probe interval.
	healthMu sync.RWMutex
	health   map[string]VMHealth

	// lastThrottleModeByAgent is each agent's throttle mode observed on the
	// previous tick, used to log only on mode transitions (not every tick).
	// Guarded by st.mu (only read/written inside tick()). Not persisted —
	// defaults to ModeAllow on a fresh process.
	lastThrottleModeByAgent map[string]ThrottleMode

	// Proactive pacing governor in-memory state (see governor.go). Guarded by
	// st.mu (read/written inside tick(), snapshotted under st.mu for /api/state).
	// Not persisted in the json blob — govCap is mirrored to the kv table
	// ("gov_cap") so the slew anchor survives a restart; lastGovDecision is
	// re-derived every tick. On a fresh process govCap is 0 (=> GovernorDecide
	// treats it as "start at maxActive") and lastGovDecision is the zero
	// (fail-open) value.
	// Per-agent (claude/codex): each agent paces against its own account quota.
	// govCapByAgent is the slew anchor per agent, mirrored to the kv table
	// ("gov_cap_<agent>") so it survives a restart. Written under st.mu (tick).
	govCapByAgent map[string]int
	// lastGovByAgent is surfaced on /api/state from the HTTP goroutine, so it
	// is guarded by its OWN mutex (govMu), NEVER st.mu — tick() holds st.mu for
	// a full multi-second scheduler pass and /api/state must not block on it.
	govMu          sync.RWMutex
	lastGovByAgent map[string]GovernorDecision
	// lastGovBinding/lastGovCap (per agent) let tick() log only on transitions.
	lastGovBindingByAgent map[string]string
	lastGovCapByAgent     map[string]int
}

// VMHealth is the in-memory liveness record for one configured VM.
type VMHealth struct {
	Online  bool
	LastOK  time.Time
	LastErr string
	OS      string // "Darwin" / "Linux" (from `uname -s`); for the dashboard machine icon
}

func (s *State) SetVMHealth(name string, h VMHealth) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	if s.health == nil {
		s.health = map[string]VMHealth{}
	}
	s.health[name] = h
}

func (s *State) VMHealth(name string) VMHealth {
	s.healthMu.RLock()
	defer s.healthMu.RUnlock()
	return s.health[name]
}

// SetGovernorState records one agent's latest governor verdict for /api/state.
// Called from tick() (which already holds st.mu); takes only govMu, so the HTTP
// reader never contends with the scheduler pass. Lock order is always st.mu →
// govMu (never the reverse), so no deadlock.
func (s *State) SetGovernorState(agent string, d GovernorDecision) {
	s.govMu.Lock()
	if s.lastGovByAgent == nil {
		s.lastGovByAgent = map[string]GovernorDecision{}
	}
	s.lastGovByAgent[agent] = d
	s.govMu.Unlock()
}

// GovernorStates returns a copy of the per-agent governor decisions for the
// lock-free /api/state builder. Guarded by govMu (NOT st.mu) so it never blocks
// behind a long tick().
func (s *State) GovernorStates() map[string]GovernorDecision {
	s.govMu.RLock()
	defer s.govMu.RUnlock()
	out := make(map[string]GovernorDecision, len(s.lastGovByAgent))
	for a, d := range s.lastGovByAgent {
		out[a] = d
	}
	return out
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

const vmHealthProbeInterval = 15 * time.Second

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
		// Multiplex: every idle-check / poke / tail to a host shares ONE TCP+ssh
		// master connection instead of opening a fresh handshake. Without this,
		// ~10 sessions on a box fire ~10 simultaneous handshakes per poll tick →
		// the box's sshd MaxStartups resets new connections ("kex_exchange_
		// identification: Connection reset") and the worker goes unreachable while
		// its already-open connections keep showing live. ControlPersist keeps the
		// master warm between calls; %C hashes user/host/port into the socket name.
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=/tmp/orch-ssh-%C",
		"-o", "ControlPersist=120s",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
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
	Priority    int // governor priority parsed in the same toml pass; 0 if absent
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
		case "priority":
			if n, err := strconv.Atoi(val); err == nil {
				cfg.Priority = n
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

// parsePriorityFrontmatter scans the issue body's leading ```toml block for a
// "priority = N" line and returns N. It is a standalone sibling of
// parseCronFrontmatter so non-cron oneshots (which never go through the full
// cron parser) still pick up a priority. Returns 0 when the frontmatter is
// absent, has no priority key, or the value is malformed — never fatal,
// mirroring parseCronFrontmatter's tolerance. The caller substitutes the
// configured DefaultPriority when this returns 0.
func parsePriorityFrontmatter(body string) int {
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "```toml" {
		return 0
	}
	i++
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
		if key != "priority" {
			continue
		}
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), "\"'")
		if n, err := strconv.Atoi(val); err == nil {
			return n
		}
		return 0
	}
	return 0
}

// validFrontmatterModels / validFrontmatterEfforts gate the claude --model and
// --effort values an issue may request via frontmatter, so a stray value can't
// be passed through to the CLI. Full model names (claude-*) are allowed for model.
var validFrontmatterModels = map[string]bool{"opus": true, "sonnet": true, "haiku": true}
var validFrontmatterEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// frontmatterString returns the quote-stripped value of a `key = value` line in
// the issue body's leading ```toml frontmatter block — the exact same block
// parsePriorityFrontmatter reads (priority lives there too). `want` must be
// lower-case. Returns "" when there is no toml block or the key is absent.
func frontmatterString(body, want string) string {
	lines := strings.Split(body, "\n")
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i >= len(lines) || strings.TrimSpace(lines[i]) != "```toml" {
		return ""
	}
	i++
	for ; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "```" {
			break
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		if strings.TrimSpace(strings.ToLower(line[:eq])) != want {
			continue
		}
		return strings.Trim(strings.TrimSpace(line[eq+1:]), "\"'")
	}
	return ""
}

// claudeFlagsFromFrontmatter returns the " --model X --effort Y" suffix an issue
// requests via toml frontmatter (model = "sonnet", effort = "medium"). Values
// are validated against a fixed allowlist so only safe tokens reach the claude
// CLI; absent or invalid values are dropped. Empty string when nothing is set.
func claudeFlagsFromFrontmatter(body string) string {
	out := ""
	m := strings.ToLower(frontmatterString(body, "model"))
	if validFrontmatterModels[m] || (m != "" && strings.HasPrefix(m, "claude-")) {
		out += " --model " + m
	}
	e := strings.ToLower(frontmatterString(body, "effort"))
	if validFrontmatterEfforts[e] {
		out += " --effort " + e
	}
	return out
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

// orchidAskHookScript is a Claude Code PreToolUse hook installed on each worker
// (see stamp_hooks). The swarm is autonomous — no human answers interactive
// prompts, so AskUserQuestion / ExitPlanMode would strand a session forever.
// This intercepts them BEFORE the blocking menu renders:
//   - AskUserQuestion → post the question + options to the inbox issue (so the
//     decision is visible / a human can redirect later via the PR), then DENY
//     with a reason that tells the agent to pick the best option itself and keep
//     going. (We deny rather than auto-answer because the allow+answers path is
//     unreliable — Claude Code strips the injected answer, anthropics/claude-code#12031.)
//   - ExitPlanMode → ALLOW, so the agent leaves plan mode and starts editing.
// __INBOX_REPO__ is replaced with the configured inbox repo in Go. Always emits
// a decision even if jq/gh fail, so a worker never hangs waiting on this hook.
const orchidAskHookScript = `#!/usr/bin/env bash
input=$(cat)
tool=$(printf '%s' "$input" | jq -r '.tool_name // ""' 2>/dev/null)

if [ "$tool" = "ExitPlanMode" ]; then
  printf '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}\n'
  exit 0
fi
if [ "$tool" != "AskUserQuestion" ]; then
  exit 0
fi

cwd=$(printf '%s' "$input" | jq -r '.cwd // ""' 2>/dev/null)
issue=$(printf '%s' "$cwd" | sed -n 's#.*/issue-\([0-9][0-9]*\).*#\1#p')
body=$(printf '%s' "$input" | jq -r '
  "An autonomous orchid worker tried to ask a question. No human is watching, so it was told to choose the best option and continue — recorded here for visibility:\n\n"
  + ([.tool_input.questions[]?
       | "**" + ((.question // .header // .text) // "Question") + "**\n"
         + ([.options[]? | "- " + ((.label // .) | tostring)
              + (if (.description // "") != "" then " — " + .description else "" end)] | join("\n"))
     ] | join("\n\n"))
' 2>/dev/null)
if [ -n "$issue" ] && [ -n "$body" ]; then
  gh issue comment "$issue" --repo "__INBOX_REPO__" --body "$body" >/dev/null 2>&1 || true
fi

reason="You are running fully autonomously in the orchid swarm — there is NO human available to answer, and the interactive menu you just opened would strand this session forever. Your question and options have been posted to the tracking issue for visibility. Decide the best option YOURSELF from the issue goal and keep working. Do not call AskUserQuestion or ExitPlanMode again — just proceed until you can open a PR."
jq -cn --arg r "$reason" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
exit 0
`

func tmuxStart(vm VMBlock, session, workdir, sharedDir, repo, branch, sessionCmdOverride, botLogin, botEmail, memOverrideDir, inboxRepo string) error {
	sessionCmd := sessionCmdOverride
	if sessionCmd == "" {
		sessionCmd = vm.SessionCmd
	}
	if sessionCmd == "" {
		sessionCmd = "clawpatrol run -- claude --dangerously-skip-permissions"
	}
	sessionHome := vm.SessionHome
	agent := vmAgent(vm).name
	codexHome := vm.CodexHome
	// PreToolUse hook that keeps an autonomous worker from stranding on an
	// interactive prompt: AskUserQuestion → post the question to the inbox issue
	// and deny so the agent decides itself; ExitPlanMode → allow (proceed). Shipped
	// as base64 so the inbox repo is injected in Go, with no shell-escaping in the
	// embedded provisioning template. Empty when unset (stamp_hooks then skips it).
	askHookB64 := ""
	if inboxRepo != "" {
		askHookB64 = base64.StdEncoding.EncodeToString(
			[]byte(strings.ReplaceAll(orchidAskHookScript, "__INBOX_REPO__", inboxRepo)))
	}
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
CODEX_HOME=%q
MEM_STORE=%q
ASK_HOOK_B64=%q

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
  if [ "$(uname)" = "Darwin" ]; then
    SESSION_USER=$(stat -f '%%Su' "$SESSION_HOME")
  else
    SESSION_USER=$(stat -c '%%U' "$SESSION_HOME")
  fi
  chown -R "$SESSION_USER:staff" "$WORKDIR" "$SHARED" 2>/dev/null || \
    chown -R "$SESSION_USER:$SESSION_USER" "$WORKDIR" "$SHARED" 2>/dev/null || true
fi

# 4) agent-specific pre-warm: pre-stamp the agent's directory-trust so the TUI
# launches straight into the task instead of blocking on a trust prompt. Claude
# stores per-folder trust in ~/.claude.json; codex stores per-project trust in
# ~/.codex/config.toml keyed by the git repo root (and we add the worktree too).
# --dangerously-bypass-approvals-and-sandbox skips codex APPROVALS but NOT the
# trust dialog, so this stamp is required for unattended codex. Stamp $HOME (the
# user running this script) and SESSION_HOME if set to a different user.
if [ "$AGENT" = "claude" ]; then
  stamp_trust() {
    local CHOME="$1"
    [ -z "$CHOME" ] && return
    local CJSON="$CHOME/.claude.json"
    [ -f "$CJSON" ] || echo '{}' > "$CJSON"
    jq --arg d "$WORKDIR" '.projects[$d].hasTrustDialogAccepted = true' "$CJSON" > "$CJSON.tmp" && mv "$CJSON.tmp" "$CJSON"
  }
  # Ensure the Notification + UserPromptSubmit hooks append to notify.jsonl, so
  # orch gets a RELIABLE "waiting for input" signal (claude emits it) instead of
  # scraping the pane. $HOME stays literal in the stored command (claude expands
  # it at hook runtime, like the statusLine hook).
  #
  # Also pin autoMemoryDirectory to ONE shared, persistent store per home. Claude
  # auto-memory is otherwise keyed by cwd path, so every ephemeral issue-N
  # worktree gets an isolated island and nothing accumulates across the swarm.
  # Redirecting all sessions to "$CHOME/.claude/auto-memory" makes the swarm's
  # discovered build env / maintainer prefs / ops gotchas compound into one wiki
  # that every later session reads at startup. autoMemoryDirectory resolves from
  # user settings (this file), and an absolute path avoids ~-expansion ambiguity.
  stamp_hooks() {
    local CHOME="$1"
    [ -z "$CHOME" ] && return
    mkdir -p "$CHOME/.claude/auto-memory"
    local SJ="$CHOME/.claude/settings.json"
    [ -f "$SJ" ] || echo '{}' > "$SJ"
    # PreToolUse auto-handler for interactive prompts (no human watches the
    # swarm): decode the hook script and register it for AskUserQuestion /
    # ExitPlanMode. jq only touches the keys it names, so a hand-set hook key
    # survives; we (re)write all of them each spawn so updates roll out.
    local HOOKARG='.'
    if [ -n "$ASK_HOOK_B64" ]; then
      local HOOKSH="$CHOME/.claude/orchid-ask-hook.sh"
      printf '%%s' "$ASK_HOOK_B64" | base64 -d > "$HOOKSH" && chmod +x "$HOOKSH"
      HOOKARG='.hooks.PreToolUse = [{"matcher":"AskUserQuestion|ExitPlanMode","hooks":[{"type":"command","command":$hook}]}]'
    fi
    # Lifecycle hooks (SessionStart/Stop/SessionEnd) feed the same notify.jsonl
    # the orchestrator already tails: claude self-reports alive/idle/exited, so
    # orch stops inferring state from pane pixels (blind 6-minute spawn waits,
    # prompt-marker idle scraping).
    local NTEE='[{"hooks":[{"type":"command","command":"tee -a $HOME/.claude/notify.jsonl >/dev/null"}]}]'
    jq --arg amd "$CHOME/.claude/auto-memory" --arg hook "$CHOME/.claude/orchid-ask-hook.sh" --argjson ntee "$NTEE" ".hooks.Notification = \$ntee | .hooks.UserPromptSubmit = \$ntee | .hooks.SessionStart = \$ntee | .hooks.Stop = \$ntee | .hooks.SessionEnd = \$ntee | .autoMemoryEnabled = true | .autoMemoryDirectory = \$amd | $HOOKARG" "$SJ" > "$SJ.tmp" && mv "$SJ.tmp" "$SJ"
  }
  stamp_trust "$HOME"
  stamp_hooks "$HOME"
  if [ -n "$SESSION_HOME" ] && [ "$SESSION_HOME" != "~" ] && [ "$SESSION_HOME" != "$HOME" ]; then
    stamp_trust "$SESSION_HOME"
    stamp_hooks "$SESSION_HOME"
  fi
elif [ "$AGENT" = "codex" ]; then
  # The shared swarm knowledge base is the claude auto-memory store (one dir,
  # maintained by every session). Codex runs as the session user too, so it can
  # READ the same store — we just point it there via AGENTS.md (codex's native,
  # always-loaded global instructions). One brain across both agents, no copy.
  STORE="$MEM_STORE"
  if [ -z "$STORE" ]; then
    STORE="$HOME/.claude/auto-memory"
    if [ -n "$SESSION_HOME" ] && [ "$SESSION_HOME" != "~" ]; then
      STORE="$SESSION_HOME/.claude/auto-memory"
    fi
  fi
  # Trust the repo root + worktree in the codex home's config.toml. CDIR is the
  # codex home directory itself (CODEX_HOME, or ~/.codex by default).
  trust_codex() {
    local CDIR="$1"
    [ -z "$CDIR" ] && return
    mkdir -p "$CDIR"
    local CFG="$CDIR/config.toml"
    touch "$CFG"
    local P
    for P in "$SHARED" "$WORKDIR"; do
      if ! grep -qF "[projects.\"$P\"]" "$CFG" 2>/dev/null; then
        printf '\n[projects."%%s"]\ntrust_level = "trusted"\n' "$P" >> "$CFG"
      fi
    done
  }
  # Suppress codex's "Update now / Skip" launch interstitial. It fires whenever
  # version.json's dismissed_version != latest_version; on a version bump the
  # default-highlighted "Update now" eats orch's wake-keystroke and the session
  # never reaches the idle prompt (3m spawn timeout). Re-stamp dismissed_version
  # to the latest codex has seen so the prompt stays dismissed across bumps.
  dismiss_codex_update() {
    local CDIR="$1"
    [ -z "$CDIR" ] && return
    local VJ="$CDIR/version.json"
    [ -f "$VJ" ] || return
    python3 -c 'import json,sys
p=sys.argv[1]
try: d=json.load(open(p))
except Exception: sys.exit(0)
lv=d.get("latest_version")
if lv: d["dismissed_version"]=lv; json.dump(d,open(p,"w"))' "$VJ" 2>/dev/null || true
  }
  # Stamp AGENTS.md pointing codex at the shared store + inline its current index
  # for cold-start value. Overwritten each spawn so the inlined index stays fresh.
  stamp_codex_agents() {
    local CDIR="$1"
    [ -z "$CDIR" ] && return
    mkdir -p "$CDIR"
    {
      printf '# Swarm agent notes (codex)\n\n'
      printf 'The orchid swarm keeps a shared, accumulating knowledge base that every\n'
      printf 'past claude/codex session contributed to (build-env incantations, test\n'
      printf 'recipes, maintainer preferences learned the hard way). Notes are kept\n'
      printf 'per target repo under %%s/<owner>/<repo>/.\n\n' "$STORE"
      printf 'Before starting, skim this repo (%%s) index and open any relevant note:\n' "$REPO"
      printf '  %%s/%%s/MEMORY.md   (this repo, index)\n' "$STORE" "$REPO"
      printf '  %%s/<owner>/<repo>/MEMORY.md   (other repos, cross-reference freely)\n\n' "$STORE"
      printf 'If you learn a durable, reusable fact, append a short note in this repo'"'"'s\n'
      printf 'dir so the next session inherits it.\n\n## This repo index\n'
      grep -E '^- ' "$STORE/$REPO/MEMORY.md" 2>/dev/null || true
    } > "$CDIR/AGENTS.md"
  }
  if [ -n "$CODEX_HOME" ]; then
    trust_codex "$CODEX_HOME"
    dismiss_codex_update "$CODEX_HOME"
    stamp_codex_agents "$CODEX_HOME"
  else
    trust_codex "$HOME/.codex"
    dismiss_codex_update "$HOME/.codex"
    stamp_codex_agents "$HOME/.codex"
    if [ -n "$SESSION_HOME" ] && [ "$SESSION_HOME" != "~" ] && [ "$SESSION_HOME" != "$HOME" ]; then
      trust_codex "$SESSION_HOME/.codex"
      dismiss_codex_update "$SESSION_HOME/.codex"
      stamp_codex_agents "$SESSION_HOME/.codex"
    fi
  fi
fi

# 5) launch the pane. When CODEX_HOME is set, export it into the session so the
# codex CLI uses the isolated auth/config/rollouts dir (single source of truth:
# the VM block's codex_home drives both the trust stamp above and the launch).
LAUNCH="$SESSION_CMD"
if [ -n "$CODEX_HOME" ]; then
  LAUNCH="CODEX_HOME=\"$CODEX_HOME\" $SESSION_CMD"
fi
# Per-target auto-memory: redirect claude's auto-memory to this repo's dir inside
# the git-backed memory clone (MEM_OVERRIDE = <clone>/<dir>/<owner>/<repo>), set
# by the caller. The sync loop commits + pushes it. Per-process env => no
# settings-file race between concurrent sessions; cross-target notes still
# reference each other (the dashboard resolves links across all repo dirs).
if [ "$AGENT" = "claude" ] && [ -n "$MEM_STORE" ]; then
  MEM_OVERRIDE="$MEM_STORE/$REPO"
  mkdir -p "$MEM_OVERRIDE"
  LAUNCH="CLAUDE_COWORK_MEMORY_PATH_OVERRIDE=\"$MEM_OVERRIDE\" $SESSION_CMD"
fi
tmux kill-session -t "$SESSION" 2>/dev/null || true
tmux new-session -d -c "$WORKDIR" -s "$SESSION" "$LAUNCH"
`, sharedDir, repo, workdir, branch, session, sessionCmd, sessionHome, botLogin, botEmail, agent, codexHome, memOverrideDir, askHookB64)

	_, errStr, err := sshExecIn(vm, script, "bash -s")
	if err != nil {
		return fmt.Errorf("tmux start: %v: %s", err, errStr)
	}
	return nil
}

// tmuxKill tears down a session AND reaps its process tree. A bare
// `tmux kill-session` only SIGHUPs the pane's foreground process group, so any
// descendant that setsid'd into its OWN session/group survives and keeps
// spinning on the dead PTY (observed: codex under `--dangerously-bypass...`
// and clawpatrol-wrapped agents detach this way → 60+ orphans pinning the box
// at load ~80, each ~19% CPU in a futex spin). So before killing the session we
// snapshot the pane pid(s), walk the full descendant closure (ppid chain is
// still intact while the pane lives), collect every distinct pgid in that
// subtree, then kill-session and `kill -9` each captured PROCESS GROUP (negative
// pid) plus any straggler pids. Killing by pgid is what reaches the setsid'd
// codex tree. Safe: tmux puts each pane in its own session, so the captured
// groups belong only to this pane's subtree — never the tmux server (the pane's
// parent, not a descendant) or orch. Piped through `bash -s` so it runs under
// bash regardless of the remote login shell (mac's is zsh, which won't
// word-split the unquoted pid lists). Best-effort; every step is `|| true`.
func tmuxKill(vm VMBlock, session string) {
	script := fmt.Sprintf(`
S=%s
ROOTS=$(tmux list-panes -t "$S" -F '#{pane_pid}' 2>/dev/null | tr '\n' ' ')
PIDS=""; PGIDS=""
if [ -n "$ROOTS" ]; then
  PIDS=$(ps -A -o pid=,ppid= 2>/dev/null | awk -v roots="$ROOTS" '
    BEGIN{n=split(roots,r," ");for(i=1;i<=n;i++)if(r[i]!="")keep[r[i]]=1}
    {pid[NR]=$1;par[NR]=$2}
    END{c=1;while(c){c=0;for(i=1;i<=NR;i++)if(!(pid[i] in keep)&&(par[i] in keep)){keep[pid[i]]=1;c=1}}
        for(k in keep)print k}')
  for p in $PIDS; do g=$(ps -o pgid= -p "$p" 2>/dev/null | tr -d ' '); [ -n "$g" ] && PGIDS="$PGIDS $g"; done
  PGIDS=$(echo "$PGIDS" | tr ' ' '\n' | sort -u | tr '\n' ' ')
fi
tmux kill-session -t "$S" 2>/dev/null || true
for g in $PGIDS; do kill -9 -"$g" 2>/dev/null || true; done
for p in $PIDS; do kill -9 "$p" 2>/dev/null || true; done
`, session)
	_, _, _ = sshExecIn(vm, script, "bash -s")
}

// tmuxSignalGroup sends sig (STOP/CONT) to the ENTIRE process group of the
// pane's foreground tree (sh -c -> clawpatrol -> claude), not just the pane
// pid's direct children. tmux launches "$SESSION_CMD" via /bin/sh -c, so the
// pane pid is the sh wrapper and clawpatrol/claude are deeper descendants;
// SIGSTOP does NOT cascade into a stopped process's children, so signalling one
// level (pkill -P) would freeze only the wrapper and leave claude burning
// tokens. tmux puts each pane in its own session/process-group, so we resolve
// the pane pid's pgid and signal the whole group (-pgid). The tmux SERVER is a
// separate process, so has-session / capture-pane keep working while the group
// is stopped, and CONT later thaws the whole tree. Idempotent; a failed signal
// just means that session keeps running (fail-open). Errors are returned for
// the caller to log.
// Duty-cycle pause = kill the session (process gone, RAM freed) while KEEPING
// its worktree, so the governor can later respawn it with --resume. The kill is
// tmuxKill (best-effort) and the respawn reuses the scheduler's normal
// dead-session-with-PR → spawnResume path (governor_loop.go). No process freeze
// (host SIGSTOP can't freeze the clawpatrol tree; cgroup.freeze is Linux-only).

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
	for k := range paneActivity {
		if !live[k] {
			delete(paneActivity, k)
		}
	}
	paneActivityMu.Unlock()
	paneActionMu.Lock()
	for k := range paneAction {
		if !live[k] {
			delete(paneAction, k)
		}
	}
	paneActionMu.Unlock()
}

// paneAction holds the latest extracted "current action" line per session — a
// one-line glance at what the agent is doing right now, lifted from the pane
// tail the activity loop already captures (no extra ssh). Surfaced per job on
// /api/state so the list view shows live work without opening the pane.
var (
	paneActionMu sync.RWMutex
	paneAction   = map[string]string{}
)

func paneActionSnapshot(tmux string) string {
	paneActionMu.RLock()
	defer paneActionMu.RUnlock()
	return paneAction[tmux]
}

func paneActionSet(tmux, s string) {
	paneActionMu.Lock()
	paneAction[tmux] = s
	paneActionMu.Unlock()
}

// extractPaneAction picks the most recent meaningful line from a pane tail: the
// agent's current step (a "• Exploring", "⏺ Edited x", "Working (12s…)", spinner
// line, etc.), skipping UI chrome — the input prompt box, separator rules, and
// the permissions footer. Best-effort: returns "" when nothing meaningful.
func extractPaneAction(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s == "" {
			continue
		}
		low := strings.ToLower(s)
		// Chrome: footer hint + the empty-input prompt placeholders.
		if strings.Contains(low, "bypass permissions") || strings.Contains(low, "shift+tab") ||
			strings.Contains(low, "? for shortcuts") || strings.Contains(low, "/clear to") {
			continue
		}
		// Separator rules: a line that's essentially all box-drawing / dashes.
		if isSeparatorRule(s) {
			continue
		}
		// Strip a leading marker glyph (bullet, prompt caret) to judge emptiness.
		t := strings.TrimSpace(strings.TrimLeft(s, "•◦●·⏺⎿└│✻✶✳✦➤>›❯⏵ "))
		if t == "" {
			continue
		}
		// The input box placeholder lines start with › / ❯ and are hints, not work.
		if (strings.HasPrefix(s, "›") || strings.HasPrefix(s, "❯")) && t == strings.TrimSpace(strings.TrimLeft(s, "›❯ ")) {
			// Keep only if it looks like real content longer than a short hint.
			if len(t) < 4 {
				continue
			}
		}
		if len([]rune(t)) > 120 {
			t = string([]rune(t)[:120]) + "…"
		}
		return t
	}
	return ""
}

// isSeparatorRule reports whether a line is essentially a horizontal rule made
// of box-drawing / dash characters (TUI divider), so it can be skipped.
func isSeparatorRule(s string) bool {
	n := 0
	for _, r := range s {
		switch r {
		case '─', '-', '━', '═', '·', ' ', '\t':
		default:
			n++
		}
	}
	return n == 0
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
	// Codex's TUI collapses large input into a "[Pasted Content N chars]"
	// placeholder via a paste-burst heuristic. A raw paste-buffer streams
	// in split across PTY reads, so codex sees SEVERAL placeholders and the
	// trailing Enter lands mid-burst → the message never submits (observed
	// 2026-05-30 on local-codex). -p wraps the content in bracketed-paste
	// markers (ESC[200~ … ESC[201~) so codex treats it as one atomic paste
	// and the following C-m submits. Claude's reader handles raw multiline
	// paste fine, so only opt codex in to avoid regressing the fleet.
	pflag := ""
	if vm.Agent == "codex" {
		pflag = "-p "
	}
	cmd := fmt.Sprintf("tmux paste-buffer %s-b %s -t %s -d; rc=$?; tmux delete-buffer -b %s 2>/dev/null || true; [ $rc -eq 0 ] || exit $rc; sleep 1; tmux send-keys -t %s C-m", pflag, buf, session, buf, session)
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

// runVMHealthLoop probes every configured VM over SSH every 15s and
// updates the in-memory health map. A successful `true` from `tmux ls`
// (or an exit code we recognise as "tmux ran") flips Online=true; any
// other failure flips it false. Kicks the broadcast channel on
// transitions so the dashboard re-renders immediately.
func runVMHealthLoop(ctx context.Context, cfg *Config, st *State) {
	probe := func() {
		for i := range cfg.VMs {
			vm := cfg.VMs[i]
			if isLocal(vm) {
				osName := "Linux"
				if runtime.GOOS == "darwin" {
					osName = "Darwin"
				}
				st.SetVMHealth(vm.Name, VMHealth{Online: true, LastOK: time.Now(), OS: osName})
				continue
			}
			before := st.VMHealth(vm.Name)
			// Piggyback `uname -s` onto the existing health probe (one ssh, no
			// extra connection) so the dashboard can show an OS icon per machine.
			out, errStr, err := sshExec(vm, "uname -s 2>/dev/null; tmux ls 2>/dev/null; true")
			now := VMHealth{LastOK: before.LastOK, OS: before.OS}
			if err != nil {
				now.Online = false
				now.LastErr = strings.TrimSpace(errStr)
				if now.LastErr == "" {
					now.LastErr = err.Error()
				}
			} else {
				now.Online = true
				now.LastOK = time.Now()
				now.LastErr = ""
				if line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0]); line == "Darwin" || line == "Linux" {
					now.OS = line
				}
			}
			if before.Online != now.Online {
				log.Printf("vm %s: %s", vm.Name, map[bool]string{true: "online", false: "offline"}[now.Online])
				if st.Bcast != nil {
					select {
					case st.Bcast <- struct{}{}:
					default:
					}
				}
			}
			st.SetVMHealth(vm.Name, now)
		}
	}
	probe()
	t := time.NewTicker(vmHealthProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			probe()
		}
	}
}

func freeVM(cfg *Config, st *State) *VMBlock {
	return freeVMAllow(cfg, st, nil)
}

// freeVMAllow is freeVM with an optional per-agent admission filter: when allow
// != nil, VMs whose agent is not currently admittable (throttle/governor) are
// skipped, so an issue routes to an agent that still has headroom instead of
// stranding behind a capped one. allow==nil considers every VM (legacy freeVM).
func freeVMAllow(cfg *Config, st *State, allow func(agent string) bool) *VMBlock {
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
		if allow != nil {
			a := vmAgent(*vm).name
			if a == "" {
				a = "claude"
			}
			if !allow(a) {
				continue
			}
		}
		// Skip VMs the SSH probe has marked offline. The local
		// bootstrap path (localhost) is always assumed reachable
		// since there's no SSH to fail.
		if !isLocal(*vm) {
			h := st.VMHealth(vm.Name)
			if !h.LastOK.IsZero() && !h.Online {
				continue
			}
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

// vmAccount is the billing/metering identity a VM's sessions count against —
// the key the quota sampler + governor pace independently. Defaults to the
// agent name (so single-account agents behave exactly as before); set the VM's
// `account` to run two logins of the same agent under distinct quota buckets.
func vmAccount(vm VMBlock) string {
	if vm.Account != "" {
		return vm.Account
	}
	return vmAgent(vm).name
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
	// Seed the adhoc counter so reissued ids skip everything persisted.
	for k := range jobs {
		if int64(k) < s.adhocSeq.Load() {
			s.adhocSeq.Store(int64(k))
		}
	}
	return s, nil
}

// saveStateLogged calls saveState and logs any persistence error.
// tick.go writes happen on every state mutation in the scheduler loop;
// callers that have no way to surface the error to a request use this
// instead of `_ = saveState(...)` so failures don't silently rot.
func saveStateLogged(s *State) {
	if err := saveState(s); err != nil {
		log.Printf("saveState: %v", err)
	}
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
		// A duty-cycle-paused job is already killed (no live pane); tmuxKill is a
		// harmless no-op then. pruneWorkdir cleans the worktree we kept for
		// --resume, since this is a real teardown (issue closed / PR merged).
		tmuxKill(*vm, j.Tmux)
		pruneWorkdir(*vm, vmWorkdirRoot(cfg.Orch, *vm), issue)
	}
	delete(st.Jobs, issue)
	clearSessionState(issue)
	log.Printf("issue #%d: torn down (was on %s/%s)", issue, j.VM, j.Tmux)
}

// closeInboxIssue closes the inbox issue once its session's PR has reached a
// terminal state (merged or closed). Cross-repo "Closes #N" doesn't auto-link,
// so without this the inbox issue stays open and the scheduler respawns the
// session every tick — which, when the PR was closed-not-merged, becomes an
// endless spawn↔teardown flap. gh issue close on an already-closed issue is a
// harmless no-op.
func closeInboxIssue(cfg *Config, issue int, prState, repo string, pr int) {
	comment := fmt.Sprintf("Auto-closing: PR https://github.com/%s/pull/%d is %s.", repo, pr, strings.ToLower(prState))
	if _, errStr, err := run("gh", "issue", "close", fmt.Sprint(issue),
		"--repo", cfg.GitHub.InboxRepo, "--comment", comment); err != nil {
		log.Printf("issue #%d: close inbox issue failed: %v: %s", issue, err, strings.TrimSpace(errStr))
		return
	}
	log.Printf("issue #%d: closed inbox issue (PR %s)", issue, strings.ToLower(prState))
	// Outcome reached — distill a one-line lesson into shared memory so future
	// workers inherit what merged smoothly vs what got rejected. Async +
	// best-effort; never holds the tick.
	go runPostmortem(cfg, issue, prState, repo, pr)
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

func diffPR(t *prTracker, v *PRView, botLogin string) (
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
	rs := seen(t.SeenReviewIDs)
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
	tc := seen(t.SeenThreadCommentIDs)
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
	ic := seen(t.SeenIssueCommentIDs)
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
	if t.LastHeadOID != "" && t.LastHeadOID != v.HeadRefOid {
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
	prev := t.LastCheckConclusions
	for name, conclusion := range latest {
		if prev[name] != conclusion && isActionableCheck(conclusion) {
			checkChanges = append(checkChanges, fmt.Sprintf("%s: %s", name, conclusion))
		}
	}
	mergeable = mergeableTransition(t.LastMergeable, v.Mergeable)
	return
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// extPRURLRe / extPRShortRe match cross-repo PR references in a PR body or
// comment: a full PR URL, or the `owner/repo#N` shorthand. Issue refs in the
// short form also match; they're filtered out later when ghPRView fails or the
// author isn't our bot.
var extPRURLRe = regexp.MustCompile(`https?://github\.com/([\w.-]+/[\w.-]+)/pull/(\d+)`)
var extPRShortRe = regexp.MustCompile(`\b([\w.-]+/[\w.-]+)#(\d+)\b`)

// extractExternalRefs pulls PR references that point at repos OTHER than the
// ones in skip (the job's target + inbox) out of the given texts (a PR body +
// comments). Deduped.
func extractExternalRefs(skip map[string]bool, texts ...string) []ExtraPR {
	seen := map[string]bool{}
	var out []ExtraPR
	add := func(repo, numStr string) {
		repo = strings.ToLower(repo)
		if skip[repo] {
			return
		}
		num, err := strconv.Atoi(numStr)
		if err != nil || num <= 0 {
			return
		}
		key := fmt.Sprintf("%s#%d", repo, num)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, ExtraPR{Repo: repo, Number: num})
	}
	for _, t := range texts {
		for _, m := range extPRURLRe.FindAllStringSubmatch(t, -1) {
			add(m[1], m[2])
		}
		for _, m := range extPRShortRe.FindAllStringSubmatch(t, -1) {
			add(m[1], m[2])
		}
	}
	return out
}

// discoverExtraPRs scans the primary PR's body + comments for upstream PRs the
// session opened in other repos and adds any new ones to j.ExtraPRs. Idempotent
// (skips refs already tracked, the target repo, and the inbox).
func discoverExtraPRs(j *Job, v *PRView, cfg *Config) {
	skip := map[string]bool{strings.ToLower(j.TargetRepo): true}
	if cfg.GitHub.InboxRepo != "" {
		skip[strings.ToLower(cfg.GitHub.InboxRepo)] = true
	}
	have := map[string]bool{}
	for _, e := range j.ExtraPRs {
		have[fmt.Sprintf("%s#%d", e.Repo, e.Number)] = true
	}
	for _, k := range j.IgnoredPRs {
		have[k] = true
	}
	texts := []string{v.Body}
	for _, c := range v.Comments {
		texts = append(texts, c.Body)
	}
	for _, ref := range extractExternalRefs(skip, texts...) {
		if !have[fmt.Sprintf("%s#%d", ref.Repo, ref.Number)] {
			j.ExtraPRs = append(j.ExtraPRs, ref)
		}
	}
}

// markPRSeen records that everything in this PRView (the just-relayed reviews/
// comments/checks + head + mergeable) has been told to the worker, so diffPR
// won't re-surface it. Shared by the primary PR and each tracked upstream PR.
func markPRSeen(t *prTracker, v *PRView, vr, sr, vt, st, vi, si []string) {
	t.SeenReviewIDs = append(append(t.SeenReviewIDs, vr...), sr...)
	t.SeenThreadCommentIDs = append(append(t.SeenThreadCommentIDs, vt...), st...)
	t.SeenIssueCommentIDs = append(append(t.SeenIssueCommentIDs, vi...), si...)
	t.LastHeadOID = v.HeadRefOid
	if v.Mergeable != "" && v.Mergeable != "UNKNOWN" {
		t.LastMergeable = v.Mergeable
	}
	if t.LastCheckConclusions == nil {
		t.LastCheckConclusions = map[string]string{}
	}
	latestAt := map[string]string{}
	for _, c := range v.StatusCheckRollup {
		if c.Status == "COMPLETED" && c.CompletedAt > latestAt[c.Name] {
			latestAt[c.Name] = c.CompletedAt
			t.LastCheckConclusions[c.Name] = c.Conclusion
		}
	}
}

// summarizeExternal is summarize with an upstream-PR header so the worker knows
// the activity is on a dependency PR it opened, not its main PR.
func summarizeExternal(repo string, num int, v *PRView, nr, ntc, nic []string, pushed bool, checks []string, mergeable string) string {
	body := strings.TrimPrefix(summarize(v, nr, ntc, nic, pushed, checks, mergeable), "PR update from orchestrator:\n\n")
	return fmt.Sprintf("Upstream PR update — %s#%d (a dependency PR you opened):\n\n%s", repo, num, body)
}

// maxReviewRepokes caps how many times orch re-surfaces an unaddressed review /
// failing CI before logging a needs-human escalation and backing off — so a
// session that genuinely can't act doesn't get nudged forever.
const maxReviewRepokes = 4

// resurfaceMsg is the reminder orch re-sends when a CHANGES_REQUESTED review or
// failing CI is still unaddressed (no new commit since it was raised) — the
// resilience net for a poke that landed in a stuck/dead pane and got dropped.
func resurfaceMsg(reviewBlocking bool, failingChecks []string) string {
	var b strings.Builder
	b.WriteString("Reminder from the orchestrator — this PR has unaddressed items and no new commits since they were raised:\n\n")
	if reviewBlocking {
		b.WriteString("- A reviewer requested CHANGES. Re-read the review, make the requested changes, and push.\n")
	}
	if len(failingChecks) > 0 {
		b.WriteString(fmt.Sprintf("- CI is failing: %s. Reproduce locally, fix, and push.\n", strings.Join(failingChecks, ", ")))
	}
	b.WriteString("\nIf you genuinely cannot act (out of scope, needs a human decision, or blocked on something you don't control — e.g. a PR title only the maintainer sets), say so in one line and stop. Otherwise address it and push.")
	return b.String()
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
	b.WriteString("\nAddress every item above. After pushing fixes, re-read the original issue and explicitly verify: is EVERY requirement fully implemented in the PR? If anything remains, keep working. Do NOT stop until the full goal is done.")
	return b.String()
}

// startSession does the workdir + tmux + bootstrap-paste dance for one
// session. It does NOT touch State.Jobs — the caller decides whether this
// is a fresh oneshot job or a recurring cron tick.
