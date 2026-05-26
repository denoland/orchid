package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/hcl/v2/hclsimple"
)

// collabHub broadcasts every message it receives to every other connected
// client. Used by the canvas dashboard for cursor positions, ink strokes,
// node moves, etc. Server is dumb — clients converge state themselves.
type collabHub struct {
	mu      sync.Mutex
	clients map[*collabClient]bool
}
type collabClient struct {
	id string
	ch chan []byte
}

func newCollabHub() *collabHub { return &collabHub{clients: map[*collabClient]bool{}} }

func (h *collabHub) add(c *collabClient) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
}
func (h *collabHub) remove(c *collabClient) {
	h.mu.Lock()
	delete(h.clients, c)
	close(c.ch)
	h.mu.Unlock()
}
func (h *collabHub) peers(self *collabClient) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.clients))
	for c := range h.clients {
		if c == self {
			continue
		}
		out = append(out, c.id)
	}
	return out
}
func (h *collabHub) broadcast(from *collabClient, msg []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		if c == from {
			continue
		}
		select {
		case c.ch <- msg:
		default:
			// Slow client — drop the message. Cursor updates are
			// disposable; on reconnect we replay.
		}
	}
}

type Config struct {
	GitHub              GitHubBlock   `hcl:"github,block" json:"github"`
	Orch                OrchBlock     `hcl:"orchestrator,block" json:"orchestrator"`
	BootstrapPrompt     string        `hcl:"bootstrap_prompt" json:"bootstrap_prompt"`
	CronBootstrapPrompt string        `hcl:"cron_bootstrap_prompt,optional" json:"cron_bootstrap_prompt,omitempty"`
	Targets             []TargetBlock `hcl:"target,block" json:"targets"`
	VMs                 []VMBlock     `hcl:"vm,block" json:"vms"`
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
	PollInterval string `hcl:"poll_interval" json:"poll_interval"`
	// StateDB is the path to the sqlite database file that holds every
	// piece of orchid state that survives a restart: tracked jobs, the
	// mention watcher's cursor + maintainer cache, and the dashboard's
	// opaque layout blob. Replaced the old state_file / snap.json pair.
	StateDB      string `hcl:"state_db" json:"state_db"`
	BranchPrefix string `hcl:"branch_prefix" json:"branch_prefix"`
	WorkdirRoot   string         `hcl:"workdir_root" json:"workdir_root"`
	HTTPAddr      string         `hcl:"http_addr,optional" json:"http_addr,omitempty"`
	HTTPSecret    string         `hcl:"http_secret,optional" json:"http_secret,omitempty"`
	AllowedLogins []string       `hcl:"allowed_logins,optional" json:"allowed_logins,omitempty"`
	BotLogin      string         `hcl:"bot_login,optional" json:"bot_login,omitempty"`
	BotEmail      string         `hcl:"bot_email,optional" json:"bot_email,omitempty"`
	NtfyTopic     string         `hcl:"ntfy_topic,optional" json:"ntfy_topic,omitempty"`
	// Path on central to the bot's GitHub SSH private key. Pushed to
	// joined VMs by the `orch join vm` handshake so the worker can
	// `git clone git@github.com:…` as the bot account without each
	// worker having to register a separate key. Empty = no push (the
	// worker must arrange its own github SSH auth, e.g. via `gh auth
	// login` + an ssh-keygen). Default lookup is ~/.ssh/id_ed25519.
	BotGithubKey string         `hcl:"bot_github_key,optional" json:"bot_github_key,omitempty"`
	Mentions     *MentionsBlock `hcl:"mentions,block" json:"mentions,omitempty"`
	Capture      *CaptureBlock  `hcl:"capture,block" json:"capture,omitempty"`
}

// CaptureBlock configures the /api/drafts endpoint that the Orchid Capture
// macOS / iOS apps post to. When unset, the endpoint is disabled.
//
// Security note: the capture token is one secret that grants the bearer the
// ability to file GitHub issues using the orchestrator's GH_TOKEN, which
// is typically a broadly-scoped PAT. By default, drafts are filed against
// DefaultRepo (or the inbox repo if unset). If you want clients to target
// other repos via DraftPayload.Target.Repo, list them in AllowedRepos —
// otherwise the handler rejects custom targets so a leaked capture token
// can't be turned into "spam any repo this PAT can write to".
type CaptureBlock struct {
	AuthToken     string   `hcl:"auth_token" json:"auth_token"`
	AssetsDir     string   `hcl:"assets_dir,optional" json:"assets_dir,omitempty"`
	PublicURL     string   `hcl:"public_url,optional" json:"public_url,omitempty"`
	DefaultRepo   string   `hcl:"default_repo,optional" json:"default_repo,omitempty"`
	DefaultLabels []string `hcl:"default_labels,optional" json:"default_labels,omitempty"`
	MaxBodyMB     int      `hcl:"max_body_mb,optional" json:"max_body_mb,omitempty"`
	// AllowedRepos lists every repo a Draft may explicitly target via
	// `target.repo`. The default_repo / inbox_repo are always implicitly
	// allowed. When unset (the safe default), clients cannot override the
	// target — they get the default.
	AllowedRepos []string `hcl:"allowed_repos,optional" json:"allowed_repos,omitempty"`
}

// MentionsBlock configures the cross-repo mention watcher. When set, orch
// polls the configured org's repos for @-mentions of any bot account
// (gathered from VM bot_login fields), classifies the mentioner as
// org-member or external (using a periodically refreshed cache), and
// dispatches: (a) mention on a tracked PR → poke that session; (b) member
// mention → open inbox issue with LLM-summarized title + ack on source;
// (c) external mention → canned reply on source.
type MentionsBlock struct {
	PollInterval   string   `hcl:"poll_interval,optional"`   // mention polling cadence; default 5m
	Org            string   `hcl:"org"`                      // GitHub org used for membership classification (e.g. "denoland")
	MaintainerTTL  string   `hcl:"maintainer_ttl,optional"`  // how often to refresh the cached member list; default 1h
	Acknowledge    bool     `hcl:"acknowledge,optional"`     // if true, add a 👀 reaction to the mentioning comment after opening an inbox issue (GitHub auto-creates the "mentioned in" backlink, so a separate ack comment isn't needed)
	HumanOverrides []string `hcl:"human_overrides,optional"` // logins to force-treat as humans even if they match the bot heuristic
	LLMCommand     []string `hcl:"llm_command,optional"`     // command for the actionable-mention LLM gate; default ["claude", "-p"]; e.g. ["codex", "exec"] to keep claude budget for workers
}

type VMBlock struct {
	Name            string `hcl:",label" json:"name"`
	Host            string `hcl:"host" json:"host"`
	User            string `hcl:"user,optional" json:"user,omitempty"`
	Key             string `hcl:"key,optional" json:"key,omitempty"`           // not needed for localhost
	Capacity        int    `hcl:"capacity,optional" json:"capacity,omitempty"` // 0 = unlimited
	Sccache         bool   `hcl:"sccache,optional" json:"sccache,omitempty"`
	SccacheDir      string `hcl:"sccache_dir,optional" json:"sccache_dir,omitempty"`           // default ~/.cache/sccache
	SessionCmd      string `hcl:"session_cmd,optional" json:"session_cmd,omitempty"`           // default: clawpatrol run -- claude --dangerously-skip-permissions
	SessionHome     string `hcl:"session_home,optional" json:"session_home,omitempty"`         // home dir of user running the session (for trust stamp)
	WorkdirRoot     string `hcl:"workdir_root,optional" json:"workdir_root,omitempty"`         // overrides orchestrator.workdir_root for sessions on this VM
	BotLogin        string `hcl:"bot_login,optional" json:"bot_login,omitempty"`               // overrides orchestrator.bot_login for sessions on this VM
	BotEmail        string `hcl:"bot_email,optional" json:"bot_email,omitempty"`               // overrides orchestrator.bot_email for sessions on this VM
	Agent           string `hcl:"agent,optional" json:"agent,omitempty"`                       // "claude" (default) or "codex"
	IdleMarker      string `hcl:"idle_marker,optional" json:"idle_marker,omitempty"`           // optional override of the agent default idle pane substring
	BusyMarker      string `hcl:"busy_marker,optional" json:"busy_marker,omitempty"`           // optional override of the agent default busy pane substring
	BootstrapPrompt string `hcl:"bootstrap_prompt,optional" json:"bootstrap_prompt,omitempty"` // optional override of orchestrator.bootstrap_prompt for this VM
	// Joined VMs (added via `orch join vm`) already got their github auth
	// material seeded by the join handshake — set this true so bootstrapVM
	// doesn't try to re-push the dedicated per-VM SSH access key as the
	// worker's id_ed25519, which would clobber the bot's github auth.
	JoinManaged bool `hcl:"join_managed,optional" json:"join_managed,omitempty"`
}

// Job lifecycle: "oneshot" (default) — issue → session → PR → teardown.
// "cron" — issue stays open, ephemeral session fires every Schedule, no PR.
type Job struct {
	VM                   string            `json:"vm"`
	Tmux                 string            `json:"tmux"`
	Target               string            `json:"target"`      // target block name
	TargetRepo           string            `json:"target_repo"` // resolved (e.g. denoland/deno)
	Branch               string            `json:"branch"`
	IssueTitle           string            `json:"issue_title,omitempty"`     // mirrored from inbox issue; refreshed each poll
	Lifecycle            string            `json:"lifecycle,omitempty"`       // "oneshot" (default) or "cron"
	Schedule             string            `json:"schedule,omitempty"`        // cron only: parseable by time.ParseDuration
	Timeout              string            `json:"timeout,omitempty"`         // cron only: max runtime per tick before orch kills the pane
	NextFireAt           time.Time         `json:"next_fire_at,omitempty"`    // cron only: when to spawn the next ephemeral tick
	FireStartedAt        time.Time         `json:"fire_started_at,omitempty"` // cron only: when the current tick started (used to enforce Timeout)
	PR                   int               `json:"pr,omitempty"`
	SeenReviewIDs        []string          `json:"seen_review_ids,omitempty"`
	SeenThreadCommentIDs []string          `json:"seen_thread_comment_ids,omitempty"`
	SeenIssueCommentIDs  []string          `json:"seen_issue_comment_ids,omitempty"`
	LastHeadOID          string            `json:"last_head_oid,omitempty"`
	LastCheckConclusions map[string]string `json:"last_check_conclusions,omitempty"`
	// LastMergeable is the last MERGEABLE/CONFLICTING state we observed.
	// Empty until the first non-UNKNOWN observation. Used to detect
	// transitions (clean → conflicting, conflicting → clean) so workers
	// learn about a base-branch conflict without a human nudge.
	LastMergeable string `json:"last_mergeable,omitempty"`
}

type State struct {
	mu            sync.Mutex
	Jobs          map[int]*Job     // active jobs, keyed by inbox issue number
	MentionCursor *time.Time       // last "updated" timestamp seen by the mention poller
	Maintainers   *MaintainerCache // cached org member list
	store         *Store           // sqlite persistence backing all three fields above
	httpSnap      atomic.Value     // stores map[int]Job; refreshed at tick start, lock-free reads
	// Bcast is a coalescing change signal. saveState non-blocking-sends
	// here so any subscriber goroutine (the relay agent's state pusher)
	// can wake up and emit a fresh state snapshot. Capacity-1 channel
	// means rapid-fire saves collapse into one wake.
	Bcast chan struct{} `json:"-"`
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

// retry wraps an exec.Command-style call with bounded retries on non-zero
// exit. clawpatrol's MITM proxy is known to drop connections sporadically
// (gh: "error connecting to api.github.com", ssh: exit 255); this hides
// those blips so a single tick doesn't lose work. Backoff: 1s, 2s, 4s.
const runAttempts = 4

// maxKillsPerTick caps how many dead-session respawns the polling loop will
// fire in a single tick. Each respawn registers a fresh peer on the clawpatrol
// WG relay; firing several together overwhelms the relay and the new sessions
// die within minutes (denoland/clawpatrol#306). Two-per-tick keeps respawns
// spaced by the poll interval, so a herd of 5–6 simultaneous deaths is paid
// back over several ticks instead of all at once.
const maxKillsPerTick = 2

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

// orchBootTime is captured at process start. The mention poller never
// looks at mentions older than this — even if state.MentionCursor
// somehow points further back (long downtime, hand-edited state file,
// etc.), so a restart never replays accumulated upstream mentions.
// Missing a mention is acceptable; dispatching the same mention twice
// is not.
var orchBootTime = time.Now()

func run(name string, args ...string) (string, string, error) {
	return runIn("", name, args...)
}

// runIn execs name+args with optional string stdin, retrying transient
// failures (clawpatrol MITM blips, ssh exit 255) with exponential backoff.
// Pass "" for no stdin. Each retry creates a fresh strings.Reader so the
// stdin replays cleanly.
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

// mustJSON marshals v or returns "null" so format strings always parse.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// stampUserID rewrites the incoming JSON message to set/override `userId`
// to the server-assigned id. Falls back to the raw payload if parsing
// fails — broadcast still happens, attribution just relies on the client.
func stampUserID(data []byte, id string) []byte {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return data
	}
	m["userId"] = id
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
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

// sshExecIn runs a shell command on the VM with stdin. For localhost, runs
// it under `bash -c` so shell operators (&&, |, redirects) work — the
// previous Fields-split version treated `&&` as a literal argv element and
// blew up commands like `tmux load-buffer -b X - && tmux paste-buffer ...`
// with "too many arguments".
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
// vmWorkdirRoot resolves where this VM stages worktrees + the shared
// clone. Per-VM override wins so a joined worker on a host that doesn't
// have /home/orchid (e.g. exe.dev VMs where the only writable user is
// /home/exedev) can point at its own writable tree without affecting
// the orch-wide default.
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
		// Joined VMs already received their bot github key + known_hosts
		// stamp during the `orch join vm` handshake. vm.Key here points
		// at the dedicated per-VM access key central holds; pushing it
		// would clobber the bot's separate github-auth id_ed25519, so
		// just run the common setup.
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
	// Pass on either signal: ssh's "Hi <user>! You've successfully
	// authenticated" or `gh auth status`'s "Logged in to github.com"
	// (used on hosts where outbound port 22 is intercepted).
	if !strings.Contains(out, "successfully authenticated") &&
		!strings.Contains(out, "Logged in to github.com") {
		return fmt.Errorf("github auth check failed (no ssh + no gh login): %q", strings.TrimSpace(out))
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
	agent := vmAgent(vm).name
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
  SESSION_USER=$(stat -c '%%U' "$SESSION_HOME")
  chown -R "$SESSION_USER:$SESSION_USER" "$WORKDIR" "$SHARED" 2>/dev/null || true
fi

# 4) agent-specific pre-warm. Claude needs its per-folder trust dialog
# pre-stamped so the TUI does not prompt; codex stores trust in ~/.codex/
# and is operator-onboarded via the codex login subcommand once per VM,
# no per-folder stamping. Stamp $HOME (the user running this script) and
# SESSION_HOME if set to a different user.
if [ "$AGENT" = "claude" ]; then
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
fi

# 5) launch the pane
tmux kill-session -t "$SESSION" 2>/dev/null || true
tmux new-session -d -c "$WORKDIR" -s "$SESSION" "$SESSION_CMD"
`, sharedDir, repo, workdir, branch, session, sessionCmd, sessionHome, botLogin, botEmail, agent)

	_, errStr, err := sshExecIn(vm, script, "bash -s")
	if err != nil {
		return fmt.Errorf("tmux start: %v: %s", err, errStr)
	}
	return nil
}

func tmuxKill(vm VMBlock, session string) {
	_, _, _ = sshExec(vm, fmt.Sprintf("tmux kill-session -t %s 2>/dev/null", session))
}

// tmuxIdle returns whether the agent TUI is at its input prompt AND, if it
// can tell, which agent is actually running in the pane. The detected name
// is independent of vm.Agent — it reads from the pane content — so it can
// be used to detect rename-worthy drift (a session started under one agent
// before the VM was switched to another).
//
// detected can be "" if the pane content matches no known agent marker
// (transient state during startup, or unknown agent).
//
// We capture the entire visible pane (not `tail -N`) because some agents'
// welcome screens leave trailing blank rows below the footer; a small tail
// window would miss the marker and falsely report not-idle.
//
// A pane showing a permission/decision dialog (Yes/No, plan approval, etc.)
// is NOT idle — it's blocked on a human. We detect this via the agent's
// promptMarkers and refuse to treat it as a "safe to poke" idle state, so
// orch never pastes a review summary on top of an open Yes/No prompt.
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

// panePrompted reports whether the pane content shows a modal/dialog the
// agent is blocked on (Claude's "Do you want to proceed? ❯ 1. Yes / 2. No"
// permission dialogs, plan approval, etc.). The check is a substring match
// against the agent's promptMarkers — a single hit is enough.
//
// Tool calls in flight (Bash, WebFetch, Read, …) render the footer as
// "(esc to interrupt)" which matches busyMarker but NOT promptMarker
// ("Esc to cancel" — capital E, distinct phrasing). To be safe against
// ever observing both simultaneously during a screen redraw, we treat a
// busy pane as never-prompted: the user can't answer a dialog that isn't
// blocking the agent yet.
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
	ring     []int // 0/1, oldest first
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
	defer paneActivityMu.Unlock()
	for k := range paneActivity {
		if !live[k] {
			delete(paneActivity, k)
		}
	}
}

// Per-session "needs human input" state. Updated by the pane sampler from
// the same capture it uses for activity ticks, read by /api/state so the
// dashboard's attention model can elevate a card to "Needs you" the moment
// claude shows a Yes/No permission dialog — without waiting for the slower
// activity-trailing-edge heuristic to kick in (which also misses the case
// where a PR is already open).
var (
	paneNeedsInputMu sync.Mutex
	paneNeedsInput   = map[string]bool{}
)

func paneNeedsInputSnapshot(tmux string) bool {
	paneNeedsInputMu.Lock()
	defer paneNeedsInputMu.Unlock()
	return paneNeedsInput[tmux]
}

// paneNeedsInputSet stores the current prompt-pending state for tmux and
// returns true if the state changed from the previous tick. Callers use the
// transition to fire a single WS notification per state change rather than
// re-broadcasting every sample.
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

// patchableBlocks lists top-level HCL block names the dashboard is
// allowed to mutate. `singleton` blocks have one body keyed only by
// name; `keyed` blocks are addressed by an additional label (vm
// "name" {}).
// Package-level hub so the pane-activity sampler (running outside the
// HTTP handler closure) can push events to dashboard subscribers.
var globalCollabHub = newCollabHub()

// Path to the HCL config file this process was started with. Read once
// in main() before any state loads; the /api/config endpoint reads/writes
// here so the dashboard's Settings page edits the same file the operator
// would edit by hand.
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
	cmd := fmt.Sprintf("tmux paste-buffer -b %s -t %s -d; status=$?; tmux delete-buffer -b %s 2>/dev/null || true; [ $status -eq 0 ] || exit $status; sleep 1; tmux send-keys -t %s C-m", buf, session, buf, session)
	if _, errStr, err := sshExec(vm, cmd); err != nil {
		return fmt.Errorf("paste-buffer+enter: %v: %s", err, errStr)
	}
	return nil
}

const operatorTmux = "operator"

// localVM returns the first localhost VM in the config, or nil.
func localVM(cfg *Config) *VMBlock {
	for i := range cfg.VMs {
		if isLocal(cfg.VMs[i]) {
			return &cfg.VMs[i]
		}
	}
	return nil
}

// operatorAlive returns whether the operator tmux session exists on any local VM.
func operatorAlive(cfg *Config) bool {
	vm := localVM(cfg)
	if vm == nil {
		return false
	}
	_, _, err := sshExec(*vm, fmt.Sprintf("tmux has-session -t %s 2>/dev/null", operatorTmux))
	return err == nil
}

// spawnOperator creates the operator tmux session running claude as the
// session_home user, waits for the idle prompt, then enables remote-control.
// Runs in its own goroutine — blocks up to 3 min waiting for claude to start.
func spawnOperator(cfg *Config) {
	vm := localVM(cfg)
	if vm == nil {
		return
	}
	sessionHome := "/home/orchid"
	for _, v := range cfg.VMs {
		if isLocal(v) && v.SessionHome != "" {
			sessionHome = v.SessionHome
			break
		}
	}
	// Pre-stamp trust for sessionHome so the TUI never shows the dialog.
	stampCmd := fmt.Sprintf(
		`CJSON=/home/orchid/.claude.json; [ -f "$CJSON" ] || echo '{}' > "$CJSON"; `+
			`jq --arg d %q '.projects[$d].hasTrustDialogAccepted = true' "$CJSON" > "$CJSON.tmp" && mv "$CJSON.tmp" "$CJSON"; `+
			`chown orchid:orchid "$CJSON" 2>/dev/null || true`,
		sessionHome,
	)
	_, _, _ = sshExec(*vm, stampCmd)
	cmd := fmt.Sprintf(
		"tmux new-session -d -s %s -c %s 'runuser -u orchid -- claude --dangerously-skip-permissions'",
		operatorTmux, sessionHome,
	)
	if _, _, err := sshExec(*vm, cmd); err != nil {
		log.Printf("operator: spawn failed: %v", err)
		return
	}
	log.Printf("operator: session started, waiting for idle prompt")
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		out, _, _ := sshExec(*vm, fmt.Sprintf("tmux capture-pane -p -t %s", operatorTmux))
		if strings.Contains(out, "trust this folder") || strings.Contains(out, "I trust") {
			_, _, _ = sshExec(*vm, fmt.Sprintf("tmux send-keys -t %s '1' C-m", operatorTmux))
			continue
		}
		if strings.Contains(out, "bypass permissions") {
			log.Printf("operator: ready")
			return
		}
	}
	log.Printf("operator: timed out waiting for idle prompt")
}

// ensureOperator spawns the operator session if missing and dismisses
// claude's trust dialog if it's blocking the pane (e.g. after an OOM
// respawn).
func ensureOperator(cfg *Config) {
	vm := localVM(cfg)
	if vm == nil {
		return
	}
	_, _, err := sshExec(*vm, fmt.Sprintf("tmux has-session -t %s 2>/dev/null", operatorTmux))
	if err != nil {
		log.Printf("operator: session not found, spawning")
		go spawnOperator(cfg)
		return
	}
	out, _, _ := sshExec(*vm, fmt.Sprintf("tmux capture-pane -p -t %s", operatorTmux))
	if strings.Contains(out, "trust this folder") || strings.Contains(out, "I trust") {
		_, _, _ = sshExec(*vm, fmt.Sprintf("tmux send-keys -t %s '1' C-m", operatorTmux))
		log.Printf("operator: dismissed trust dialog")
	}
}

// sessionName picks a tmux session id that reflects the agent running in
// the pane. Empty agent falls back to "claude" for back-compat.
func sessionName(issue int, agent string) string {
	if agent == "" {
		agent = "claude"
	}
	return fmt.Sprintf("%s-%d", agent, issue)
}

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

// agentSpec describes the per-agent quirks orch needs to drive a worker
// session: how to detect the TUI's idle/busy state by capturing the pane,
// and how to transform a fresh start command into a resume command.
type agentSpec struct {
	name          string
	idleMarker    string                         // substring present when the TUI is at its input prompt
	busyMarker    string                         // substring present when the TUI is processing (empty = "busy iff !idle")
	promptMarkers []string                       // substrings indicating the TUI is showing a modal/dialog awaiting user input
	resumeXform   func(sessionCmd string) string // returns a session_cmd that resumes the most recent conversation
}

// agentSpecs are the built-in defaults; per-VM idle_marker/busy_marker can override.
var agentSpecs = map[string]agentSpec{
	"claude": {
		name:       "claude",
		idleMarker: "bypass permissions",
		busyMarker: "esc to interrupt",
		// Claude's permission/decision dialogs render a footer of the form
		// "Esc to cancel · Tab to amend · ctrl+e to explain". The exact tail
		// rotates between variants ("Esc to cancel" / "Esc to interrupt and
		// edit") but the leading "Esc to cancel" is stable and case-distinct
		// from the busy marker "esc to interrupt". A pane with this footer is
		// blocked on a human answering Yes/No/etc., not idle.
		promptMarkers: []string{"Esc to cancel"},
		resumeXform: func(s string) string {
			return strings.Replace(s,
				"claude --dangerously-skip-permissions",
				"claude --dangerously-skip-permissions --resume", 1)
		},
	},
	"codex": {
		name: "codex",
		// Codex's footer always renders the model line "<model> <preset> · <workdir>".
		// "gpt-" matches gpt-5.5 / gpt-5.6 / etc. without binding to a specific
		// version. It's also stable across the welcome screen and the input
		// prompt (the per-tip hints below the prompt rotate).
		idleMarker: "gpt-",
		busyMarker: "esc to interrupt",
		resumeXform: func(s string) string {
			// Handle both `exec codex` (shell wrapper) and bare binary invocation.
			// Insert `resume --last` right after the codex binary (before any flags).
			if strings.Contains(s, "exec codex") {
				return strings.Replace(s, "exec codex", "exec codex resume --last", 1)
			}
			// bare: `.../bin/codex [flags]` → `.../bin/codex resume --last [flags]`
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

// loadState opens the sqlite store at dbPath and reads any existing
// persisted state. If a legacy state.json (or snap.json siblings) lives
// alongside an empty DB, they're imported once and renamed with a
// .migrated suffix so we don't re-import them on the next boot. This
// keeps an in-flight orchestrator's tracked jobs intact across the
// json→sqlite cutover.
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
	return s, nil
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
	// Wake any state-push subscriber (relay agent). Non-blocking — if
	// the channel buffer is already full, the subscriber will see this
	// change folded into the next wake.
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
		tmuxKill(*vm, j.Tmux)
		pruneWorkdir(*vm, vmWorkdirRoot(cfg.Orch, *vm), issue)
	}
	delete(st.Jobs, issue)
	log.Printf("issue #%d: torn down (was on %s/%s)", issue, j.VM, j.Tmux)
}

// pruneWorkdir removes the per-issue workdir (git worktree + build artifacts).
// Called on teardown and periodically for orphans.
func pruneWorkdir(vm VMBlock, root string, issue int) {
	root = strings.TrimRight(root, "/")
	dir := fmt.Sprintf("%s/issue-%d", root, issue)
	// remove the worktree from git first so the shared clone stays consistent
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
	active := make(map[int]string) // issue → VM name
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
			// extract issue number from path like /path/issue-42
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
				continue // still active (any VM — co-located VMs share filesystem)
			}
			log.Printf("pruning orphan workdir %s on %s", line, vm.Name)
			pruneWorkdir(vm, root, n)
		}
	}
}

// mergeableTransition returns the post-transition mergeable state we should
// notify about, or "" if the change isn't worth surfacing. Rules:
//   - UNKNOWN is transient (GitHub still computing): never notify on it,
//     never set it as a baseline.
//   - First observation of MERGEABLE: silent baseline (the expected case).
//   - First observation of CONFLICTING: notify — we may have just discovered
//     a PR that became conflicted before we attached to it.
//   - MERGEABLE → CONFLICTING: notify (base branch moved, worker must rebase).
//   - CONFLICTING → MERGEABLE: notify (resolved — worker pushed the fix or
//     upstream backed out the change; useful confirmation either way).
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

// isActionableCheck reports whether a CI check conclusion warrants poking
// the worker. SUCCESS / NEUTRAL / SKIPPED / STALE are the expected case
// — there's no fix to push for a green check, and waking the worker just
// to have them reply "nothing to address" wastes a session turn. Anything
// else (FAILURE, CANCELLED, TIMED_OUT, ACTION_REQUIRED, STARTUP_FAILURE,
// and any conclusion GitHub adds in the future) is treated as actionable
// — better to over-notify on an unknown state than swallow a real
// failure.
func isActionableCheck(conclusion string) bool {
	switch conclusion {
	case "SUCCESS", "NEUTRAL", "SKIPPED", "STALE":
		return false
	}
	return true
}

// headPushedByBot reports whether the PR's current HEAD commit was
// authored solely by the configured bot. Used to suppress "new commits
// pushed" wake-ups when the worker is responding to its own activity.
//
// We deliberately check only the HEAD commit, not every commit since
// LastHeadOID — if a third party (human or another bot) had pushed in
// between, our worker would have had to fetch+rebase before pushing on
// top, so it's already aware of that intermediate work. If the bot
// authors HEAD it has, by transitive reasoning, seen everything below
// it. Empty botLogin returns false (defensive: any misconfiguration
// still surfaces pushes).
func headPushedByBot(v *PRView, botLogin string) bool {
	if botLogin == "" || v.HeadRefOid == "" {
		return false
	}
	for _, c := range v.Commits {
		if c.Oid != v.HeadRefOid {
			continue
		}
		if len(c.Authors) == 0 {
			return false
		}
		for _, a := range c.Authors {
			if a.Login != botLogin {
				return false
			}
		}
		return true
	}
	return false
}

// diffPR partitions unseen PR activity into two buckets:
//
//   - visible*: items the worker should be woken about (third-party
//     reviews/comments and pushes by anyone other than the bot).
//   - silent*: items authored by the bot itself. Must still be tracked
//     so we don't keep re-scanning them on every tick, but pasting them
//     back into the bot's own pane is noise — the worker already knows
//     it wrote that comment or pushed that commit.
//
// pushed is true only if HEAD changed *and* the latest commit was not
// authored solely by the bot. If botLogin is empty, no filtering is
// applied (defensive — a misconfigured swarm still gets notifications).
func diffPR(j *Job, v *PRView, botLogin string) (
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
	rs := seen(j.SeenReviewIDs)
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
	tc := seen(j.SeenThreadCommentIDs)
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
	ic := seen(j.SeenIssueCommentIDs)
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
	if j.LastHeadOID != "" && j.LastHeadOID != v.HeadRefOid {
		pushed = !headPushedByBot(v, botLogin)
	}
	// Build latest-per-name map: GitHub returns all historical runs; we only
	// care about the most recent completed run for each check name. Then
	// drop transitions to non-actionable conclusions — a freshly-passed
	// check has nothing for the worker to address, so waking them on it
	// just burns a session turn on a "nothing to address" round-trip
	// (denoland/orchid#224). Failures, cancellations, timeouts, etc. still
	// surface.
	latest := map[string]string{} // name → conclusion
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
	prev := j.LastCheckConclusions
	for name, conclusion := range latest {
		if prev[name] != conclusion && isActionableCheck(conclusion) {
			checkChanges = append(checkChanges, fmt.Sprintf("%s: %s", name, conclusion))
		}
	}
	mergeable = mergeableTransition(j.LastMergeable, v.Mergeable)
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
	b.WriteString("\nAddress these, push fixes if needed, then stop and wait for the next message.")
	return b.String()
}

// startSession does the workdir + tmux + bootstrap-paste dance for one
// session. It does NOT touch State.Jobs — the caller decides whether this
// is a fresh oneshot job or a recurring cron tick.
func startSession(cfg *Config, vm *VMBlock, is Issue, target TargetBlock, lifecycle, schedule string) error {
	session := sessionName(is.Number, vmAgent(*vm).name)
	branch := cfg.Orch.BranchPrefix + fmt.Sprint(is.Number)
	root := vmWorkdirRoot(cfg.Orch, *vm)
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
	_, _, _ = sshExec(*vm, fmt.Sprintf("tmux send-keys -t %s C-m", session))
	// 3 minutes covers slow claude TUI startup in heavy worktrees (e.g.
	// fresh deno checkout: lockfile parse + project scan can push first
	// idle past the 60s mark on a contended VM).
	const idleWaitTimeout = 3 * time.Minute
	deadline := time.Now().Add(idleWaitTimeout)
	sawIdle := false
	for time.Now().Before(deadline) {
		if idle, _, err := tmuxIdle(*vm, session); err == nil && idle {
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
	tmpl := cfg.BootstrapPrompt
	if vm.BootstrapPrompt != "" {
		tmpl = vm.BootstrapPrompt
	}
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
		VM: vm.Name, Tmux: sessionName(is.Number, vmAgent(*vm).name),
		Target: target.Name, TargetRepo: target.Repo,
		Branch: branch, Lifecycle: "oneshot",
		IssueTitle:           is.Title,
		LastCheckConclusions: map[string]string{},
	}
	log.Printf("issue #%d: spawned on %s/%s, target=%s (%s), branch=%s",
		is.Number, vm.Name, sessionName(is.Number, vmAgent(*vm).name), target.Name, target.Repo, branch)
	// React 👀 on the inbox issue so a human watching the inbox sees an
	// immediate "picked up" signal without having to open the dashboard.
	// Idempotent (GitHub returns the existing reaction on duplicate POSTs)
	// so respawns from death/resume don't compound. Best-effort: a failure
	// here doesn't abort the spawn.
	_, _, err := run("gh", "api", "-X", "POST",
		fmt.Sprintf("repos/%s/issues/%d/reactions", cfg.GitHub.InboxRepo, is.Number),
		"-f", "content=eyes")
	if err != nil {
		log.Printf("issue #%d: eyes reaction on inbox failed: %v", is.Number, err)
	}
	return nil
}

// spawnResume restarts a dead session that had an open PR, using --resume so
// claude recovers its conversation context, then pastes a short situation report.
func spawnResume(cfg *Config, st *State, vm *VMBlock, n int, j *Job) error {
	session := sessionName(n, vmAgent(*vm).name)
	// VM may have switched agents between spawn and respawn — keep state in
	// sync with the new tmux name (no in-pane rename needed; old session is
	// already dead and tmuxStart creates the new one fresh).
	if j.Tmux != session {
		log.Printf("issue #%d: tmux name updating %s → %s (agent change)", n, j.Tmux, session)
		j.Tmux = session
	}
	root := vmWorkdirRoot(cfg.Orch, *vm)
	workdir := fmt.Sprintf("%s/issue-%d", root, n)
	sharedDir := fmt.Sprintf("%s/repos/%s", root, strings.ReplaceAll(j.TargetRepo, "/", "-"))

	base := vm.SessionCmd
	if base == "" {
		base = "clawpatrol run -- claude --dangerously-skip-permissions"
	}
	resumeCmd := vmAgent(*vm).resumeXform(base)

	botLogin, botEmail := vmBotIdentity(cfg.Orch, *vm)
	if err := tmuxStart(*vm, session, workdir, sharedDir, j.TargetRepo, j.Branch, resumeCmd, botLogin, botEmail); err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	_, _, _ = sshExec(*vm, fmt.Sprintf("tmux send-keys -t %s C-m", session))
	// Same 3-minute window as startSession; claude --resume on a heavy
	// worktree replays the conversation and can take a while.
	deadline := time.Now().Add(3 * time.Minute)
	resumeIdle := false
	for time.Now().Before(deadline) {
		if idle, _, err := tmuxIdle(*vm, session); err == nil && idle {
			resumeIdle = true
			break
		}
		time.Sleep(2 * time.Second)
	}
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
	if !resumeIdle {
		log.Printf("issue #%d: resumed before idle prompt was observed; bootstrap poke still pasted best-effort", n)
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
	session := sessionName(is.Number, vmAgent(*vm).name)
	if j := st.Jobs[is.Number]; j != nil {
		j.VM = vm.Name
		j.Tmux = session
	}
	log.Printf("issue #%d: cron tick fired on %s/%s (schedule=%s)",
		is.Number, vm.Name, session, schedule)
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
		_ = saveState(st)
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
					_ = saveState(st)
				}
				return
			}
			// Session is gone — claude finished or exited. Clear the
			// stale Tmux marker so the next fire-due check spawns fresh.
			j.Tmux = ""
			j.VM = ""
			j.FireStartedAt = time.Time{}
			_ = saveState(st)
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
	_ = saveState(st)
}

// --- mention watcher ---

// Mention is one @-mention of a configured bot found in a comment on an
// issue or PR in a configured target repo.

func tick(cfg *Config, st *State) {
	st.mu.Lock()
	defer st.mu.Unlock()
	// Publish a lock-free snapshot for the HTTP handler before doing any I/O.
	snap := make(map[int]Job, len(st.Jobs))
	for n, j := range st.Jobs {
		snap[n] = *j
	}
	st.httpSnap.Store(snap)
	// One inbox call instead of one-per-target. allOpen holds every open
	// inbox issue (used to detect closed-issue teardown without a separate
	// ghIssueIsOpen probe per job). open is the subset routed to a target
	// by label match — first target whose label matches wins.
	type routed struct {
		is     Issue
		target TargetBlock
	}
	allOpen := map[int]Issue{}
	open := map[int]routed{}
	issues, err := ghIssueList(cfg.GitHub.InboxRepo, "")
	if err != nil {
		log.Printf("list inbox issues: %v; preserving tracked jobs until next successful poll", err)
		return
	}
	for _, is := range issues {
		allOpen[is.Number] = is
		for _, t := range cfg.Targets {
			if is.hasLabel(t.Label) {
				open[is.Number] = routed{is: is, target: t}
				break
			}
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
			_ = saveState(st)
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
		_ = saveState(st)
	}

	// Bound how many dead-session respawns we issue per tick. Each respawn
	// registers a new peer on the clawpatrol WG relay; firing several at
	// once overwhelms it and the freshly-spawned sessions die within
	// minutes (see denoland/clawpatrol#306). Sessions still down on the
	// next tick are picked up then, so kills stagger naturally.
	budget := killBudget{max: maxKillsPerTick}
	for n, j := range st.Jobs {
		if r, routedOpen := open[n]; routedOpen {
			j.IssueTitle = r.is.Title
			// Lifecycle drift: an operator may have added or removed the
			// `cron` label after we registered the job. Re-evaluate from
			// the live label set; if it disagrees with j.Lifecycle, drop
			// the job so the next tick's registration loop picks it up
			// fresh under the right lifecycle. Avoids the issue #4 case
			// (registered as oneshot, label flipped to cron, schedule
			// never fired).
			wantCron := r.is.hasLabel("cron")
			isCron := j.Lifecycle == "cron"
			if wantCron != isCron {
				log.Printf("issue #%d: lifecycle drift (have=%q want=%s) — dropping for re-registration",
					n, j.Lifecycle, map[bool]string{true: "cron", false: "oneshot"}[wantCron])
				tearDown(cfg, st, n)
				_ = saveState(st)
				continue
			}
		} else if _, stillOpen := allOpen[n]; !stillOpen {
			// Not in the freshly-fetched open list — issue is closed.
			// (Or its target label was removed; in that case allOpen
			// would still contain it and we'd keep the job running.)
			tearDown(cfg, st, n)
			_ = saveState(st)
			continue
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
				_ = saveState(st)
				continue
			}
			tickCron(cfg, st, n, j, r.is, r.target)
			continue
		}
		vm := vmByName(cfg, j.VM)
		if vm == nil {
			log.Printf("issue #%d: vm %q gone from config, dropping", n, j.VM)
			delete(st.Jobs, n)
			_ = saveState(st)
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
			_ = saveState(st)
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
						// Branch already has a PR opened by a different
						// account — a peer orchestrator (fibibot etc.)
						// won the race. Adopting their PR is wrong: our
						// local worker's tree diverged, we can't push fixes
						// onto someone else's branch, and relaying their
						// reviews into our pane is noise. Free the slot and
						// let them finish.
						if strings.Contains(err.Error(), "already exists") {
							log.Printf("issue #%d: branch %s already has a PR by another account, tearing down", n, j.Branch)
							tearDown(cfg, st, n)
							_ = saveState(st)
							continue
						}
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
			_ = saveState(st)
		}
		v, err := ghPRView(j.TargetRepo, j.PR)
		viewedAt := time.Now()
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
			_ = saveState(st)
			continue
		}
		botLogin, _ := vmBotIdentity(cfg.Orch, *vm)
		vr, vt, vi, sr, st_, si, pushed, checks, mergeable := diffPR(j, v, botLogin)
		// silent items (bot's own comments/reviews) must be marked seen
		// even when nothing else changed, or we'll re-evaluate them on
		// every tick forever.
		hasSilent := len(sr) > 0 || len(st_) > 0 || len(si) > 0
		if len(vr) == 0 && len(vt) == 0 && len(vi) == 0 && !pushed && len(checks) == 0 && mergeable == "" {
			j.LastHeadOID = v.HeadRefOid
			// Record the mergeable baseline (silently) on no-diff ticks so
			// the first MERGEABLE observation after PR open doesn't show up
			// as a transition later. UNKNOWN is left out — it's transient.
			if v.Mergeable != "" && v.Mergeable != "UNKNOWN" {
				j.LastMergeable = v.Mergeable
			}
			if hasSilent {
				j.SeenReviewIDs = append(j.SeenReviewIDs, sr...)
				j.SeenThreadCommentIDs = append(j.SeenThreadCommentIDs, st_...)
				j.SeenIssueCommentIDs = append(j.SeenIssueCommentIDs, si...)
				_ = saveState(st)
			}
			continue
		}
		idle, detected, err := tmuxIdle(*vm, j.Tmux)
		if err != nil {
			log.Printf("issue #%d: idle check failed: %v", n, err)
			continue
		}
		// Tmux name drift: if we detected a different agent in the pane than
		// the session id implies, rename the live tmux session and update
		// state. This catches the case where a session was respawned under
		// a different agent (operator switched vm.agent between deploys)
		// and the old "claude-N" name now lies about a codex pane.
		if detected != "" {
			if want := sessionName(n, detected); want != j.Tmux {
				if _, _, e := sshExec(*vm, fmt.Sprintf("tmux rename-session -t %s %s", j.Tmux, want)); e == nil {
					log.Printf("issue #%d: tmux renamed %s → %s (detected %s in pane)", n, j.Tmux, want, detected)
					j.Tmux = want
					_ = saveState(st)
				}
			}
		}
		if !idle {
			log.Printf("issue #%d: pane busy, deferring poke", n)
			continue
		}
		// Re-check PR state immediately before poking — but only if the
		// original view is stale enough to be worth a fresh API call.
		// Within a single tick iteration the gap is usually 1-2s (just
		// the SSH idle check). Re-fetching every time burns ~10 calls/tick
		// for a near-zero merge-race window. Threshold is conservative.
		const reCheckAfter = 5 * time.Second
		if time.Since(viewedAt) >= reCheckAfter {
			fresh, ferr := ghPRView(j.TargetRepo, j.PR)
			if ferr != nil {
				log.Printf("issue #%d: pre-poke pr re-check failed: %v", n, ferr)
			} else if fresh.State == "MERGED" || fresh.State == "CLOSED" {
				log.Printf("issue #%d: PR %s between view and poke — skipping poke and tearing down", n, fresh.State)
				tearDown(cfg, st, n)
				_ = saveState(st)
				continue
			}
		}
		msg := summarize(v, vr, vt, vi, pushed, checks, mergeable)
		if err := tmuxPaste(*vm, j.Tmux, msg); err != nil {
			log.Printf("issue #%d: poke failed: %v", n, err)
			continue
		}
		j.SeenReviewIDs = append(j.SeenReviewIDs, vr...)
		j.SeenReviewIDs = append(j.SeenReviewIDs, sr...)
		j.SeenThreadCommentIDs = append(j.SeenThreadCommentIDs, vt...)
		j.SeenThreadCommentIDs = append(j.SeenThreadCommentIDs, st_...)
		j.SeenIssueCommentIDs = append(j.SeenIssueCommentIDs, vi...)
		j.SeenIssueCommentIDs = append(j.SeenIssueCommentIDs, si...)
		j.LastHeadOID = v.HeadRefOid
		if v.Mergeable != "" && v.Mergeable != "UNKNOWN" {
			j.LastMergeable = v.Mergeable
		}
		if j.LastCheckConclusions == nil {
			j.LastCheckConclusions = map[string]string{}
		}
		latestAt := map[string]string{}
		for _, c := range v.StatusCheckRollup {
			if c.Status == "COMPLETED" && c.CompletedAt > latestAt[c.Name] {
				latestAt[c.Name] = c.CompletedAt
				j.LastCheckConclusions[c.Name] = c.Conclusion
			}
		}
		_ = saveState(st)
		log.Printf("issue #%d: poked PR #%d", n, j.PR)
	}
}

// --- HTTP UI ---

//go:embed all:www/dist
var wwwFS embed.FS

type apiJobEntry struct {
	Issue int `json:"issue"`
	Job
	// Activity is a 0/1 ring of recent activity ticks (1 = pane changed
	// since the previous tick). The dashboard uses the last N values to
	// drive the "working / idle / needs-you" attention indicators.
	Activity []int `json:"activity,omitempty"`
	// Usage is the latest Claude statusline reading for this session,
	// keyed off the claude session UUID resolved from the worktree cwd.
	// Omitted when no statusline event has landed yet.
	Usage *apiPaneUsage `json:"usage,omitempty"`
	// NeedsInput is true when the agent's TUI is showing a modal/dialog
	// awaiting a human response (Claude's "Do you want to proceed? ❯ 1.
	// Yes / 2. No" permission dialogs, plan approval, etc.). The dashboard
	// elevates these cards to "Needs you" regardless of PR state.
	NeedsInput bool `json:"needs_input,omitempty"`
}

type apiVMEntry struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Capacity int    `json:"capacity"`
	Used     int    `json:"used"`
	Bot      string `json:"bot"`   // resolved bot login (per-VM override → orch default)
	Agent    string `json:"agent"` // "claude" / "codex" / etc — drives dashboard display
}

type apiStateResp struct {
	Jobs     []apiJobEntry `json:"jobs"`
	VMs      []apiVMEntry  `json:"vms"`
	Inbox    string        `json:"inbox"`
	Operator string        `json:"operator"` // tmux session name if alive, "" if dead
	// Quota carries the most recent Claude subscription rate-limit
	// snapshot we've observed. Used by the dashboard's header strip
	// (5-hour + 7-day usage bars + reset countdown). Omitted on a
	// fresh install before any statusline event has landed.
	Quota *apiQuota `json:"quota,omitempty"`
}

type apiPaneUsage struct {
	Model      string   `json:"model,omitempty"`
	CostUSD    float64  `json:"cost_usd,omitempty"`
	ContextPct *float64 `json:"context_pct,omitempty"`
}

type apiQuota struct {
	FiveHourPct      float64 `json:"five_hour_pct"`
	FiveHourResetsAt int64   `json:"five_hour_resets_at"`
	SevenDayPct      float64 `json:"seven_day_pct"`
	SevenDayResetsAt int64   `json:"seven_day_resets_at"`
}

// lookupPaneVM resolves a tmux session id to the VM it's running on.
// Shared by the HTTP pane handlers and the relay-agent pane mux —
// keeps both paths in sync on session→VM resolution rules.
func lookupPaneVM(cfg *Config, st *State, session string) *VMBlock {
	if v := st.httpSnap.Load(); v != nil {
		for _, j := range v.(map[int]Job) {
			if j.Tmux == session {
				return vmByName(cfg, j.VM)
			}
		}
	}
	for i := range cfg.VMs {
		if isLocal(cfg.VMs[i]) {
			_, _, err := sshExec(cfg.VMs[i], fmt.Sprintf("tmux has-session -t %s 2>/dev/null", session))
			if err == nil {
				return &cfg.VMs[i]
			}
		}
	}
	return nil
}

// buildAPIStateJSON renders the /api/state payload from the current
// snapshot. Shared by the HTTP handler and the relay agent's WS push so
// both deliver byte-identical bodies — dashboard can swap from polling
// to push without any client-side schema branching.
func buildAPIStateJSON(cfg *Config, st *State) []byte {
	var snap map[int]Job
	if v := st.httpSnap.Load(); v != nil {
		snap = v.(map[int]Job)
	}
	load := map[string]int{}
	jobs := make([]apiJobEntry, 0, len(snap))
	for issue, j := range snap {
		entry := apiJobEntry{
			Issue:      issue,
			Job:        j,
			Activity:   paneActivitySnapshot(j.Tmux),
			NeedsInput: paneNeedsInputSnapshot(j.Tmux),
		}
		if u := usageForIssue(issue); u != nil {
			entry.Usage = &apiPaneUsage{
				Model:      u.Model.DisplayName,
				CostUSD:    u.Cost.TotalCostUSD,
				ContextPct: u.ContextWindow.UsedPct,
			}
		}
		jobs = append(jobs, entry)
		load[j.VM]++
	}
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].Tmux < jobs[b].Tmux })

	vms := make([]apiVMEntry, 0, len(cfg.VMs))
	for _, vm := range cfg.VMs {
		bot, _ := vmBotIdentity(cfg.Orch, vm)
		vms = append(vms, apiVMEntry{
			Name:     vm.Name,
			Host:     vm.Host,
			Capacity: vm.Capacity,
			Used:     load[vm.Name],
			Bot:      bot,
			Agent:    vmAgent(vm).name,
		})
	}
	op := ""
	if operatorAlive(cfg) {
		op = operatorTmux
	}
	resp := apiStateResp{
		Jobs:     jobs,
		VMs:      vms,
		Inbox:    cfg.GitHub.InboxRepo,
		Operator: op,
	}
	if five, seven, ok := latestQuota(); ok {
		resp.Quota = &apiQuota{
			FiveHourPct:      five.UsedPct,
			FiveHourResetsAt: five.ResetsAt,
			SevenDayPct:      seven.UsedPct,
			SevenDayResetsAt: seven.ResetsAt,
		}
	}
	body, _ := json.Marshal(resp)
	return body
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
	fmt.Fprintf(&b, "- State:        %s\n", cfg.Orch.StateDB)
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
	// CSP locks the login page to its own inline <style> and forbids any
	// script or framing — a defence-in-depth shield against any future
	// reflected-content bug on this template.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusUnauthorized)
	// Both `next` (came from the URL query / form field) and `errMsg`
	// (constant in practice today, but future-proof anyway) flow into
	// HTML attribute / element content. Escape with html.EscapeString
	// — fmt.Sprintf's %q is Go-syntax, NOT HTML-safe, and previously
	// allowed `next=" autofocus=" onfocus="…"` style attribute-injection.
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
<input type=hidden name=next value="%s">
<input type=password name=token placeholder="token" autofocus>
<button type=submit>Sign in</button>
</form></body></html>`,
		func() string {
			if errMsg != "" {
				return `<div class="err">` + html.EscapeString(errMsg) + `</div>`
			}
			return ""
		}(),
		html.EscapeString(next))
}

func httpHandler(cfg *Config, st *State) http.Handler {
	secret := cfg.Orch.HTTPSecret
	secretBytes := []byte(secret)

	const cookieName = "orchid_token"

	// secretMatches is a constant-time string compare against the configured
	// http_secret. Empty secret never matches — caller must short-circuit on
	// secret == "" instead.
	secretMatches := func(tok string) bool {
		if tok == "" {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(tok), secretBytes) == 1
	}

	// safeRedirectPath validates a `next=` redirect target so a crafted URL
	// can't bounce the user off-host. Only same-origin paths (must start with
	// a single "/", must not start with "//" which is a protocol-relative
	// URL, must not contain control chars) survive; everything else falls
	// back to "/".
	safeRedirectPath := func(dest string) string {
		if dest == "" || !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") {
			return "/"
		}
		for _, c := range dest {
			if c < 0x20 || c == 0x7f {
				return "/"
			}
		}
		return dest
	}

	// behindTLS reports whether the proxy that forwarded the request is
	// using TLS. Direct connections set r.TLS; the Cloudflare relay agent
	// strips TLS but a future fronting proxy can advertise via the
	// X-Forwarded-Proto header. We only flip Secure on cookies when we're
	// sure — local-only operators on plain http would otherwise lose their
	// session every request.
	behindTLS := func(r *http.Request) bool {
		if r.TLS != nil {
			return true
		}
		return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	}

	makeSessionCookie := func(r *http.Request) *http.Cookie {
		return &http.Cookie{
			Name: cookieName, Value: secret,
			Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
			Secure: behindTLS(r),
		}
	}

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
			if !secretMatches(tok) {
				renderLogin(w, safeRedirectPath(r.URL.RequestURI()), "")
				return
			}
			if r.URL.Query().Get("token") != "" {
				http.SetCookie(w, makeSessionCookie(r))
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
			if secretMatches(r.FormValue("token")) {
				http.SetCookie(w, makeSessionCookie(r))
				http.Redirect(w, r, safeRedirectPath(r.FormValue("next")), http.StatusSeeOther)
			} else {
				renderLogin(w, safeRedirectPath(r.FormValue("next")), "invalid token")
			}
		})
	}

	// /api/state — JSON snapshot of jobs + VMs
	mux.HandleFunc("/api/state", auth(func(w http.ResponseWriter, r *http.Request) {
		body := buildAPIStateJSON(cfg, st)
		etag := fmt.Sprintf(`W/"%x"`, fnv64(string(body)))
		w.Header().Set("ETag", etag)
		// no-cache (not no-store) so the browser revalidates with
		// If-None-Match on every poll and 304s cost nothing.
		w.Header().Set("Cache-Control", "no-cache")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))

	// paneVM resolves a tmux session to its VM. Checks active jobs first,
	// then falls back to scanning localhost VMs (for operator sessions not
	// tracked in state).
	paneVM := func(session string) *VMBlock { return lookupPaneVM(cfg, st, session) }

	// POST /api/pane?s=<session> — forward body bytes as tmux input.
	// Snapshots stream over /api/pane/stream below.
	mux.HandleFunc("/api/pane", auth(func(w http.ResponseWriter, r *http.Request) {
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
		vm := paneVM(session)
		if vm == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only — use /api/pane/stream for snapshots", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil || len(body) == 0 {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		// Unique buffer name per request — the previous shared `input`
		// buffer raced when two keystrokes arrived close together (second
		// load clobbered first, first paste then found buffer empty and
		// returned non-zero).
		buf := tmuxPasteBuf()
		cmd := fmt.Sprintf(
			"tmux load-buffer -b %s - && tmux paste-buffer -b %s -t %s -d",
			buf, buf, session,
		)
		if _, errStr, err := sshExecIn(*vm, string(body), cmd); err != nil {
			http.Error(w, "send failed: "+errStr, http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// POST /api/pane/resize?s=<session>&cols=N&rows=M — resize the tmux
	// window to match the client's xterm so claude's TUI lays out cleanly.
	mux.HandleFunc("/api/pane/resize", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		session := r.URL.Query().Get("s")
		for _, c := range session {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				http.Error(w, "invalid session", http.StatusBadRequest)
				return
			}
		}
		vm := paneVM(session)
		if vm == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		cols := atoiClamp(r.URL.Query().Get("cols"), 80, 40, 300)
		rows := atoiClamp(r.URL.Query().Get("rows"), 24, 10, 200)
		_, _, _ = sshExec(*vm, fmt.Sprintf(
			"tmux resize-window -t %s -x %d -y %d 2>/dev/null", session, cols, rows,
		))
		w.WriteHeader(http.StatusNoContent)
	}))

	// GET /api/pane/stream?s=<session> — server-sent events stream of pane
	// snapshots. One persistent SSH session per viewer runs a tight loop on
	// the VM emitting `tmux capture-pane` output separated by 0x1E. The
	// server diffs against the last snapshot it sent and only forwards
	// changes, base64-encoded inside an SSE `data:` line.
	mux.HandleFunc("/api/pane/stream", auth(func(w http.ResponseWriter, r *http.Request) {
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
		vm := paneVM(session)
		if vm == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Connection", "keep-alive")
		fl.Flush()

		// Resize the tmux window to match the client's xterm dimensions
		// so claude's TUI lays out exactly to the visible pane. Defaults
		// generous so a connect without cols/rows still shows something
		// useful. Best-effort — failures here don't block the stream.
		cols := atoiClamp(r.URL.Query().Get("cols"), 80, 40, 300)
		rows := atoiClamp(r.URL.Query().Get("rows"), 24, 10, 200)
		_, _, _ = sshExec(*vm, fmt.Sprintf(
			"tmux resize-window -t %s -x %d -y %d 2>/dev/null", session, cols, rows,
		))

		remote := fmt.Sprintf(
			`while :; do tmux capture-pane -p -e -t %s -S -200 2>&1; printf '\x1e'; sleep 0.2; done`,
			session,
		)
		var cmd *exec.Cmd
		if isLocal(*vm) {
			cmd = exec.CommandContext(r.Context(), "bash", "-c", remote)
		} else {
			cmd = exec.CommandContext(r.Context(), "ssh", append(sshArgs(*vm), remote)...)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			fl.Flush()
			return
		}
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", err.Error())
			fl.Flush()
			return
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

		snapCh := make(chan string, 1)
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			rd := bufio.NewReader(stdout)
			var buf strings.Builder
			for {
				b, err := rd.ReadByte()
				if err != nil {
					return
				}
				if b == 0x1e {
					snap := buf.String()
					buf.Reset()
					select {
					case snapCh <- snap:
					default:
						// Drop snapshots when the client is slower than the
						// VM loop — last writer wins.
					}
				} else {
					buf.WriteByte(b)
				}
			}
		}()

		// Per-frame compression: we wrap each pane snapshot in a gzip
		// stream, base64-encode the result, and prefix the SSE data
		// with "z:" so the browser knows to ungzip via DecompressionStream.
		// Doing this at the application layer (rather than via
		// Content-Encoding) sidesteps the relay tunnel — which doesn't
		// guarantee transparent passthrough of the encoding header — and
		// avoids any chance of CF double-encoding the response.
		gzbuf := new(bytes.Buffer)
		gzwriter := gzip.NewWriter(gzbuf)
		gzipFrame := func(s string) string {
			gzbuf.Reset()
			gzwriter.Reset(gzbuf)
			_, _ = gzwriter.Write([]byte(s))
			_ = gzwriter.Close()
			return "z:" + base64.StdEncoding.EncodeToString(gzbuf.Bytes())
		}

		keepalive := time.NewTicker(20 * time.Second)
		defer keepalive.Stop()
		var last string
		for {
			select {
			case <-r.Context().Done():
				return
			case <-readDone:
				return
			case snap := <-snapCh:
				if snap == last {
					continue
				}
				last = snap
				if _, err := fmt.Fprintf(w, "data: %s\n\n", gzipFrame(snap)); err != nil {
					return
				}
				fl.Flush()
			case <-keepalive.C:
				if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
					return
				}
				fl.Flush()
			}
		}
	}))

	// GET /api/usage_history?days=30 — daily Claude spend + token
	// breakdown sourced from the per-conversation jsonl files. Rows
	// are returned at session×model granularity; the dashboard
	// aggregates client-side so the same payload powers day/week/
	// month rollups and per-session drill-ins.
	mux.HandleFunc("/api/usage_history", auth(func(w http.ResponseWriter, r *http.Request) {
		days := atoiClamp(r.URL.Query().Get("days"), 30, 1, 365)
		since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
		rows, err := st.store.LoadUsageHistory(since)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"since": since,
			"days":  days,
			"rows":  rows,
		})
	}))

	// GET /api/og?url=... — fetch a remote page and pull its OpenGraph
	// metadata (og:image, og:title, og:description). Used by the canvas to
	// render rich link cards. Has a hard 6s timeout + 1MB read limit + an
	// SSRF guard that blocks loopback / RFC1918 / link-local destinations
	// both at URL-parse time and at the actual dial (close DNS-rebinding
	// TOCTOU). Only http(s) URLs accepted — no file://, ftp://, gopher:// etc.
	ogTransport := safeSSRFTransport()
	mux.HandleFunc("/api/og", auth(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("url")
		if raw == "" {
			http.Error(w, "url required", http.StatusBadRequest)
			return
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			http.Error(w, "bad url", http.StatusBadRequest)
			return
		}
		// Reject URLs with embedded userinfo (https://user:pass@host/) —
		// the credentials would leak to a third-party host if redirected.
		if u.User != nil {
			http.Error(w, "userinfo in url not allowed", http.StatusBadRequest)
			return
		}
		if isPrivateHost(u.Hostname()) {
			http.Error(w, "host blocked", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", raw, nil)
		req.Header.Set("User-Agent", "OrchidLinkBot/1.0 (+orchid)")
		req.Header.Set("Accept", "text/html,application/xhtml+xml")
		client := &http.Client{
			Transport: ogTransport,
			Timeout:   6 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) > 5 {
					return fmt.Errorf("too many redirects")
				}
				// Re-validate scheme on every hop: a 30x to file:// or
				// javascript:// would otherwise sneak past the entry check.
				if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
					return fmt.Errorf("redirect to non-http scheme blocked")
				}
				if isPrivateHost(req.URL.Hostname()) {
					return fmt.Errorf("redirect to private host blocked")
				}
				return nil
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "fetch failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		out := parseOG(string(body), raw)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=600")
		_ = json.NewEncoder(w).Encode(out)
	}))

	// GET /api/canvas/ws — websocket that relays canvas events between
	// dashboard tabs for realtime collab (cursors, ink, node moves). Server
	// is a dumb hub: each client sends JSON messages and receives every
	// other client's. Clients converge state on their own.
	hub := globalCollabHub
	mux.HandleFunc("/api/canvas/ws", auth(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // same-origin is already enforced by auth
		})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		client := &collabClient{id: tmuxPasteBuf(), ch: make(chan []byte, 64)}
		hub.add(client)
		defer hub.remove(client)

		hello := fmt.Sprintf(`{"type":"hello","userId":%q,"peers":%s}`,
			client.id, mustJSON(hub.peers(client)))
		_ = conn.Write(ctx, websocket.MessageText, []byte(hello))
		hub.broadcast(client, []byte(fmt.Sprintf(`{"type":"join","userId":%q}`, client.id)))

		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			for {
				_, data, err := conn.Read(ctx)
				if err != nil {
					return
				}
				// Stamp the originating userId so clients can attribute
				// messages without trusting the payload itself.
				stamped := stampUserID(data, client.id)
				hub.broadcast(client, stamped)
			}
		}()

		for {
			select {
			case <-ctx.Done():
				_ = conn.Close(websocket.StatusNormalClosure, "context done")
				return
			case <-readDone:
				_ = conn.Close(websocket.StatusNormalClosure, "client closed")
				hub.broadcast(client, []byte(fmt.Sprintf(`{"type":"leave","userId":%q}`, client.id)))
				return
			case msg, ok := <-client.ch:
				if !ok {
					return
				}
				if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
					return
				}
			}
		}
	}))

	// GET/PUT /api/snap — opaque dashboard layout state (card positions,
	// notes, links, strokes). Persisted in the sqlite state DB so the
	// canvas survives across browsers; replaces the localStorage scheme.
	// PutSnap also rotates the prior value into a snap.bak row so a buggy
	// client clobbering positions doesn't destroy the last good layout.
	mux.HandleFunc("/api/snap", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Cache-Control", "no-store")
			b, err := st.store.GetSnap()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if b == nil {
				b = []byte("{}")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		case http.MethodPut, http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if !json.Valid(body) {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			if err := st.store.PutSnap(body); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "GET/PUT/POST only", http.StatusMethodNotAllowed)
		}
	}))

	// GET/PUT /api/config — structured view of the operator's swarm.hcl.
	// GET parses the file fresh (not the in-memory cfg, so it reflects
	// out-of-band edits too) and returns it as JSON. PUT accepts a partial
	// JSON object {section: {field: value}, …} and uses hclwrite to patch
	// only the touched attributes — comments and whitespace in the rest
	// of the file are preserved. Apply on next orchid restart.
	mux.HandleFunc("/api/config", auth(func(w http.ResponseWriter, r *http.Request) {
		path := globalConfigPath
		if path == "" {
			http.Error(w, "config path unknown", http.StatusInternalServerError)
			return
		}
		switch r.Method {
		case http.MethodGet:
			b, err := os.ReadFile(path)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			var current Config
			if err := hclsimple.Decode(filepath.Base(path), b, nil, &current); err != nil {
				http.Error(w, "parse: "+err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(current)
		case http.MethodPut, http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var patch map[string]map[string]any
			if err := json.Unmarshal(body, &patch); err != nil {
				http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
				return
			}
			src, err := os.ReadFile(path)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			out, perr := patchHCL(src, patch)
			if perr != nil {
				http.Error(w, "patch: "+perr.Error(), http.StatusBadRequest)
				return
			}
			// Re-parse the patched bytes before touching disk. hclsimple
			// picks its parser by file extension, so writing to `.tmp`
			// first (the old code) blew up with "unrecognized file
			// format suffix .tmp" even on perfectly valid output.
			// Decode in-memory, keyed off the real filename so error
			// messages point at swarm.hcl.
			var trial Config
			if err := hclsimple.Decode(filepath.Base(path), out, nil, &trial); err != nil {
				http.Error(w, "invalid hcl after patch: "+err.Error(), http.StatusBadRequest)
				return
			}
			tmp := path + ".tmp"
			// 0o600 — swarm.hcl carries http_secret, capture.auth_token,
			// and any per-VM credentials the operator wired in. World-
			// readable is a needless leak to anyone with shell on the box.
			if err := os.WriteFile(tmp, out, 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.Rename(tmp, path); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// Hot-apply the bits we can without an orch restart. The
			// allow-list is the one Settings-page edit that
			// matters most for owner-shared dashboards — push it to
			// the relay immediately so the invited login can refresh
			// and see the dashboard without a service bounce.
			cfg.Orch.AllowedLogins = append([]string(nil), trial.Orch.AllowedLogins...)
			pushAllowedLogins(cfg.Orch.AllowedLogins)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "GET/PUT/POST only", http.StatusMethodNotAllowed)
		}
	}))

	// POST /api/vm/join — register a freshly-installed worker VM. The
	// worker (running `orch join vm <central-url> <token>`) hands us its
	// public hostname/IP and target SSH user; in return we:
	//   1. Generate a dedicated ed25519 keypair under <install_dir>/vm-keys/<name>
	//      (private stays on central; only the public half goes back over the wire)
	//   2. Read the bot's GitHub SSH key (orchestrator.bot_github_key, default
	//      ~/.ssh/id_ed25519) so the worker can clone/push as the bot — see
	//      OrchBlock.BotGithubKey docstring for the security caveat
	//   3. Patch swarm.hcl to add `vm "<name>" {...}` with join_managed=true
	//      (so bootstrapVM skips re-pushing the access key as id_ed25519)
	// Idempotent on conflict: re-joining with the same `name` rotates the
	// access key and replaces the block.
	mux.HandleFunc("/api/vm/join", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := handleVMJoin(w, r); err != nil {
			log.Printf("vm join: %v", err)
		}
	}))

	// POST /api/operator — spawn (or respawn) the operator claude session.
	mux.HandleFunc("/api/operator", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if operatorAlive(cfg) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		go spawnOperator(cfg)
		w.WriteHeader(http.StatusAccepted)
	}))

	// /api/drafts and /captures/* — Orchid Capture intake. Only registered
	// when the operator opted in via the `capture` config block; otherwise
	// these routes 404 like any other unconfigured endpoint.
	if cfg.Orch.Capture != nil {
		registerCaptureRoutes(mux, cfg)
	}

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

// runJoin dispatches `orch join …` to the right handler:
//
//	orch join <relay-url> <relay-token>                — legacy: attach to a relay
//	orch join relay <relay-url> <relay-token>          — explicit relay form
//	orch join vm <central-url> <invite-token> [flags]  — register this host as a worker VM
