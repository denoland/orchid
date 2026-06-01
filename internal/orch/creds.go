package orch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Credential providers are the seam that lets orch source coding-agent auth from
// different backends without the scheduler caring. Today agent creds are copied
// to every worker by hand; this replaces that with a pluggable provider.
//
//   - "local" (default): orch keeps the creds in its own store and writes the
//     agent's auth files onto each VM at spawn.
//   - "clawpatrol" (future, drop-in): the VM is joined to a clawpatrol gateway
//     that injects the real OAuth at the wire — nothing is written to the VM.
//     One registerCredProvider("clawpatrol", …) call; no scheduler changes.
//     (clawpatrol never exports a secret, so it implements CredentialProvider
//     but NOT CredentialStore.)
//
// Creds are keyed by ACCOUNT, not by agent — a swarm runs several accounts of
// the same provider (e.g. codex + codex-mini) and different machines can hold
// different creds of the same provider. A VM's `account` field (defaults to its
// `agent`) selects which cred it gets; the agent type (claude/codex) decides the
// file layout + where on the box they're written.

// CredentialProvider makes an agent usable on a worker VM before its session
// launches.
type CredentialProvider interface {
	Name() string
	// Provision authenticates `agent` on vm for vm's account. Called once per
	// spawn, before the tmux pane launches. nil when there's nothing to do (no
	// stored creds for that account — leave whatever's already on the box).
	Provision(ctx context.Context, vm *VMBlock, agent string) error
}

// CredentialStore is the write/connect side — implemented only by providers that
// actually hold the secret (the local store). The Integrations "Connect" device
// flow and `orch creds import` land here. clawpatrol does not implement it.
type CredentialStore interface {
	SetCreds(account string, ac accountCreds) error
	HasCreds(account string) bool
}

// KV is the small slice of the store credential providers need.
type KV interface {
	PutKV(key string, value []byte) error
	GetKV(key string) ([]byte, error)
}

// CredentialsBlock selects the provider. Lives inside `orchestrator`.
type CredentialsBlock struct {
	Provider string `hcl:"provider,optional" json:"provider,omitempty"` // "local" (default) | "clawpatrol"
}

// accountCreds is one account's auth: which agent (file layout) + the files,
// keyed by path relative to the agent's auth base on the worker.
type accountCreds struct {
	Agent string            `json:"agent"` // claude | codex
	Files map[string][]byte `json:"files"`
}

// agentCredRel lists the auth files per agent, relative to that agent's base on
// the worker (claude base = session home; codex base = CODEX_HOME). The codex
// config.toml is optional.
var agentCredRel = map[string][]string{
	"claude": {".claude/.credentials.json", ".claude.json"},
	"codex":  {"auth.json", "config.toml"},
}

type credFactory func(cfg *Config, kv KV) CredentialProvider

var credRegistry = map[string]credFactory{}

// registerCredProvider wires a provider impl under a config name — the whole
// extension point.
func registerCredProvider(name string, f credFactory) { credRegistry[name] = f }

// credProviderName is the active provider name for display ("local" before Main
// wires one up).
func credProviderName() string {
	if credProvider != nil {
		return credProvider.Name()
	}
	return "local"
}

func activeCredProvider(cfg *Config, kv KV) CredentialProvider {
	name := "local"
	if cfg.Orch.Credentials != nil && cfg.Orch.Credentials.Provider != "" {
		name = cfg.Orch.Credentials.Provider
	}
	if f := credRegistry[name]; f != nil {
		return f(cfg, kv)
	}
	log.Printf("credentials: unknown provider %q, falling back to local", name)
	return credRegistry["local"](cfg, kv)
}

func init() {
	registerCredProvider("local", func(cfg *Config, kv KV) CredentialProvider { return &localCreds{kv: kv} })
}

// localCreds keeps each account's auth files in orch's kv store and writes them
// onto a VM at spawn — only when absent, so a CLI's self-refreshed token is
// never clobbered.
type localCreds struct{ kv KV }

func (*localCreds) Name() string { return "local" }

func (l *localCreds) load(account string) (*accountCreds, error) {
	b, err := l.kv.GetKV("creds_" + account)
	if err != nil || len(b) == 0 {
		return nil, err
	}
	var ac accountCreds
	if err := json.Unmarshal(b, &ac); err != nil {
		return nil, err
	}
	return &ac, nil
}

func (l *localCreds) SetCreds(account string, ac accountCreds) error {
	b, err := json.Marshal(ac)
	if err != nil {
		return err
	}
	return l.kv.PutKV("creds_"+account, b)
}

func (l *localCreds) HasCreds(account string) bool {
	ac, _ := l.load(account)
	return ac != nil && len(ac.Files) > 0
}

// credSessionHome is the worker-side home for a VM (remote path string).
func credSessionHome(vm *VMBlock) string {
	if vm.SessionHome != "" && vm.SessionHome != "~" {
		return vm.SessionHome
	}
	return "$HOME"
}

// credBase is where an agent's auth files live on the worker: claude under the
// session home, codex under CODEX_HOME.
func credBase(vm *VMBlock, agent string) string {
	if agent == "codex" {
		if vm.CodexHome != "" {
			return vm.CodexHome
		}
		return credSessionHome(vm) + "/.codex"
	}
	return credSessionHome(vm)
}

func (l *localCreds) Provision(ctx context.Context, vm *VMBlock, agent string) error {
	account := vmAccount(*vm)
	ac, err := l.load(account)
	if err != nil {
		return fmt.Errorf("load creds for account %q: %w", account, err)
	}
	if ac == nil || len(ac.Files) == 0 {
		return nil // nothing stored for this account — leave the box's creds (BYO)
	}
	base := credBase(vm, ac.Agent)
	for rel, content := range ac.Files {
		dst := path.Join(base, rel)
		// Write only if absent: never overwrite a CLI's freshly-refreshed token.
		script := fmt.Sprintf(`d=%q; mkdir -p "$(dirname "$d")"; [ -e "$d" ] || { cat > "$d" && chmod 600 "$d"; }`, dst)
		if _, errStr, err := sshExecIn(*vm, string(content), script); err != nil {
			return fmt.Errorf("write %s on %s: %v: %s", rel, vm.Name, err, strings.TrimSpace(errStr))
		}
	}
	return nil
}

// importCreds reads an account's auth files from `fromDir` (claude: the home
// dir; codex: the CODEX_HOME dir) and stores them under the account, so the
// local provider writes them onto every VM that uses that account.
func importCreds(kv KV, account, agent, fromDir string) (int, error) {
	rels, ok := agentCredRel[agent]
	if !ok {
		return 0, fmt.Errorf("unknown agent %q (want claude|codex)", agent)
	}
	files := map[string][]byte{}
	for _, rel := range rels {
		b, err := os.ReadFile(filepath.Join(fromDir, rel))
		if err != nil {
			continue // optional file may be absent
		}
		files[rel] = b
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("no %s creds found under %s", agent, fromDir)
	}
	return len(files), (&localCreds{kv: kv}).SetCreds(account, accountCreds{Agent: agent, Files: files})
}
