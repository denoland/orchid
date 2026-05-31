package orch

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"
)

// Memory is persisted in a real git repo (default: the inbox repo, on main,
// under a `memory/` subtree) so the swarm's accumulated notes are durable,
// versioned, browsable on GitHub, and shareable across boxes. The flow:
//
//   - Each box keeps a persistent clone (memoryRepoDir).
//   - Per-target auto-memory writes into <clone>/<dir>/<owner>/<repo>/ (set via
//     CLAUDE_COWORK_MEMORY_PATH_OVERRIDE in tmuxStart).
//   - A single sync goroutine commits the memory subtree, pull --rebases, and
//     pushes on a timer. Memory shares no path with code, so rebases against a
//     moving main never conflict; the one serial committer avoids self-races.
//   - The dashboard reads the local clone.

// MemoryBlock configures the git-backed memory store. Absent block => disabled
// (auto-memory falls back to the local per-target dir, no push).
type MemoryBlock struct {
	Enabled      bool   `hcl:"enabled,optional" json:"enabled,omitempty"`
	Repo         string `hcl:"repo,optional" json:"repo,omitempty"`                   // default: inbox repo
	Branch       string `hcl:"branch,optional" json:"branch,omitempty"`               // default: main
	Dir          string `hcl:"dir,optional" json:"dir,omitempty"`                     // subtree, default: memory
	SyncInterval string `hcl:"sync_interval,optional" json:"sync_interval,omitempty"` // default: 5m
}

func memoryOn(cfg *Config) bool { return cfg.Orch.Memory != nil && cfg.Orch.Memory.Enabled }

func memRepo(cfg *Config) string {
	if cfg.Orch.Memory != nil && cfg.Orch.Memory.Repo != "" {
		return cfg.Orch.Memory.Repo
	}
	return cfg.GitHub.InboxRepo
}
func memBranch(cfg *Config) string {
	if cfg.Orch.Memory != nil && cfg.Orch.Memory.Branch != "" {
		return cfg.Orch.Memory.Branch
	}
	return "main"
}
func memDir(cfg *Config) string {
	if cfg.Orch.Memory != nil && cfg.Orch.Memory.Dir != "" {
		return strings.Trim(cfg.Orch.Memory.Dir, "/")
	}
	return "memory"
}
func memInterval(cfg *Config) time.Duration {
	if cfg.Orch.Memory != nil && cfg.Orch.Memory.SyncInterval != "" {
		if d, err := time.ParseDuration(cfg.Orch.Memory.SyncInterval); err == nil && d > 0 {
			return d
		}
	}
	return 5 * time.Minute
}

// memoryRepoDir is the persistent clone path on a VM (the box's session home).
func memoryRepoDir(vm *VMBlock) string {
	home := vm.SessionHome
	if home == "" || home == "~" {
		home = "$HOME"
	}
	return filepath.Join(home, ".orch", "memory-repo")
}

// memoryStoreDir is the dir the dashboard reads + the parent of the per-target
// override dirs: <clone>/<dir>.
func memoryStoreDir(cfg *Config, vm *VMBlock) string {
	return filepath.Join(memoryRepoDir(vm), memDir(cfg))
}

// memoryStoreArg is the store base passed to tmuxStart (the script derives the
// per-repo override + the codex pointer from it). Empty when memory is disabled,
// which leaves auto-memory on its local default.
func memoryStoreArg(cfg *Config, vm *VMBlock) string {
	if !memoryOn(cfg) {
		return ""
	}
	return memoryStoreDir(cfg, vm)
}

// memorySyncScript stages the memory subtree, commits when dirty, integrates the
// remote with rebase, and pushes. Idempotent: clones on first run, no-ops when
// clean. Runs on the memory VM (root on the box) using gh's git credential
// helper for auth; the memory subtree is chowned to the session user so agents
// can write into it.
func memorySyncScript(cfg *Config, vm *VMBlock) string {
	repo := memRepo(cfg)
	return fmt.Sprintf(`set -e
REPODIR=%q
REPO=%q
BRANCH=%q
DIR=%q
BOT_LOGIN=%q
BOT_EMAIL=%q
SESSION_HOME=%q

git config --global --add safe.directory "$REPODIR" 2>/dev/null || true
# Same auth path as tmuxStart: prefer the bot's ssh key, fall back to gh https.
if ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -T git@github.com 2>&1 | grep -q 'successfully authenticated'; then
  URL="git@github.com:$REPO.git"
else
  gh auth setup-git -h github.com >/dev/null 2>&1 || true
  URL="https://github.com/$REPO.git"
fi

if [ ! -d "$REPODIR/.git" ]; then
  rm -rf "$REPODIR"; mkdir -p "$REPODIR"
  git clone --branch "$BRANCH" "$URL" "$REPODIR" >/dev/null 2>&1 || git clone "$URL" "$REPODIR" >/dev/null 2>&1
  cd "$REPODIR"; git checkout "$BRANCH" >/dev/null 2>&1 || git checkout -b "$BRANCH" >/dev/null 2>&1 || true
else
  cd "$REPODIR"
  git remote set-url origin "$URL" 2>/dev/null || true
fi
git config user.name "$BOT_LOGIN"
git config user.email "$BOT_EMAIL"
mkdir -p "$DIR"

# let the session user (agents) write into the memory subtree
if [ -n "$SESSION_HOME" ] && [ "$SESSION_HOME" != "~" ]; then
  U=$(stat -c '%%U' "$SESSION_HOME" 2>/dev/null || stat -f '%%Su' "$SESSION_HOME" 2>/dev/null)
  [ -n "$U" ] && chown -R "$U" "$DIR" 2>/dev/null || true
fi

git add "$DIR" 2>/dev/null || true
if ! git diff --cached --quiet 2>/dev/null; then
  git commit -q -m "memory: sync $(date -u +%%FT%%TZ)" || true
fi
for i in 1 2 3; do
  git pull --rebase --autostash origin "$BRANCH" >/dev/null 2>&1 || git rebase --abort >/dev/null 2>&1 || true
  if git push origin "$BRANCH" >/dev/null 2>&1; then break; fi
  sleep 2
done
git rev-parse --short HEAD 2>/dev/null || true
`, memoryRepoDir(vm), repo, memBranch(cfg), memoryStoreDir(cfg, vm), vmBotIdentityLogin(cfg, vm), vmBotIdentityEmail(cfg, vm), vm.SessionHome)
}

// memorySyncOnce runs the sync script on the memory VM.
func memorySyncOnce(cfg *Config) (string, error) {
	vm, _ := memoryStore(cfg)
	if vm == nil {
		return "", fmt.Errorf("no memory VM")
	}
	out, errStr, err := sshExecIn(*vm, memorySyncScript(cfg, vm), "bash -s")
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(errStr))
	}
	return strings.TrimSpace(out), nil
}

// runMemorySyncLoop clones (first tick) then commits+pushes memory on a timer.
func runMemorySyncLoop(ctx context.Context, cfg *Config) {
	if !memoryOn(cfg) {
		return
	}
	t := time.NewTicker(memInterval(cfg))
	defer t.Stop()
	for {
		if head, err := memorySyncOnce(cfg); err != nil {
			log.Printf("memory sync: %v", err)
		} else if head != "" {
			log.Printf("memory sync ok (%s@%s %s)", memRepo(cfg), memBranch(cfg), head)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// vmBotIdentityLogin/Email are thin helpers so the sync script commits under the
// bot identity (same precedence as session commits: VM block then orch block).
func vmBotIdentityLogin(cfg *Config, vm *VMBlock) string {
	l, _ := vmBotIdentity(cfg.Orch, *vm)
	return l
}
func vmBotIdentityEmail(cfg *Config, vm *VMBlock) string {
	_, e := vmBotIdentity(cfg.Orch, *vm)
	return e
}
