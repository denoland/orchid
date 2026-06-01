package orch

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	osuser "os/user"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

const (
	// HTTP client timeout for `orch join` calls against central — local
	// network latency only, so 30s is plenty.
	joinHTTPTimeout = 30 * time.Second
	// `orch run` calls into central can take longer because the spawn
	// pipeline contacts a worker VM over SSH before responding.
	adhocHTTPTimeout = 60 * time.Second
	// Max bytes to read from a non-2xx response body when surfacing the
	// error message to the user.
	errBodySnippet = 4096
	// Cadence at which the assignment-tick claims unassigned issues for
	// bot accounts.
	assignmentTickInterval = 60 * time.Second
)

func runJoin(args []string) {
	if len(args) >= 1 {
		switch args[0] {
		case "vm":
			runJoinVM(args[1:])
			return
		case "relay":
			runJoinRelay(args[1:])
			return
		}
	}
	runJoinRelay(args)
}

// runJoinRelay writes the relay URL + agent token into the operator's env
// file and restarts the systemd unit so the daemon picks them up.
func runJoinRelay(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: orch join [relay] <relay-url> <relay-token>")
		os.Exit(2)
	}
	relayArg, token := args[0], args[1]
	for _, c := range relayArg + token {
		if c == '\n' || c == '\r' || c == 0 || c < 0x20 || c == 0x7f {
			fmt.Fprintln(os.Stderr, "orch join: url/token contains control characters")
			os.Exit(2)
		}
	}
	u, perr := url.Parse(relayArg)
	if perr != nil || (u.Scheme != "wss" && u.Scheme != "ws") || u.Host == "" {
		fmt.Fprintf(os.Stderr, "orch join: bad relay url (expect wss://host/path): %v\n", perr)
		os.Exit(2)
	}
	envPath := os.Getenv("ORCHID_ENV_FILE")
	if envPath == "" {
		envPath = "/root/orch/env"
	}
	// Read existing env, drop any old RELAY_* lines, append new ones.
	var keep []string
	if b, err := os.ReadFile(envPath); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "RELAY_URL=") || strings.HasPrefix(line, "RELAY_TOKEN=") {
				continue
			}
			if line != "" {
				keep = append(keep, line)
			}
		}
	}
	keep = append(keep, "RELAY_URL="+relayArg, "RELAY_TOKEN="+token)
	if err := os.WriteFile(envPath, []byte(strings.Join(keep, "\n")+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", envPath, err)
		os.Exit(1)
	}
	_ = exec.Command("systemctl", "daemon-reload").Run()
	if err := exec.Command("systemctl", "restart", "orchid").Run(); err != nil {
		fmt.Fprintf(os.Stderr, "systemctl restart: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("orch: joined " + relayArg)
}

// vmJoinRequest is the payload `orch join vm` POSTs to /api/vm/join.
type vmJoinRequest struct {
	Name     string `json:"name,omitempty"`
	Hostname string `json:"hostname"`
	SSHUser  string `json:"ssh_user"`
	HostKey  string `json:"host_key,omitempty"`
}

// vmJoinResponse is central's reply with the SSH material the worker
// installs into the orchid user's ~/.ssh.
type vmJoinResponse struct {
	Name                string `json:"name"`
	AccessPublicKey     string `json:"access_public_key"`
	BotGithubPrivateKey string `json:"bot_github_private_key,omitempty"`
	BotGithubPublicKey  string `json:"bot_github_public_key,omitempty"`
}

func runJoinVM(args []string) {
	fs := flag.NewFlagSet("orch join vm", flag.ExitOnError)
	hostname := fs.String("hostname", "", "public hostname/IP central will SSH to (default: first non-loopback address)")
	sshUser := fs.String("user", "orchid", "SSH user on this VM central connects as (must already exist)")
	name := fs.String("name", "", "VM name central should record (default: central allocates)")
	insecure := fs.Bool("insecure-http", false, "allow plain-http central-url (required to send bot github keys over the wire)")
	// Go's flag.Parse stops at the first non-flag arg, so flags placed
	// after positionals are silently ignored. Re-order so flags come
	// first — lets users type `orch join vm URL TOKEN --user=...` too.
	args = reorderFlagsFirst(args)
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	pos := fs.Args()
	if len(pos) < 2 {
		fmt.Fprintln(os.Stderr, "usage: orch join vm [--hostname=H] [--user=U] [--name=N] <central-url> <invite-token>")
		os.Exit(2)
	}
	centralURL, token := strings.TrimRight(pos[0], "/"), pos[1]
	parsed, err := url.Parse(centralURL)
	if err != nil || parsed.Host == "" {
		fmt.Fprintf(os.Stderr, "bad central url %q: %v\n", centralURL, err)
		os.Exit(2)
	}
	if parsed.Scheme != "https" && !*insecure {
		fmt.Fprintf(os.Stderr, "refusing to send join request over %q — pass --insecure-http if you trust this network\n", parsed.Scheme)
		os.Exit(2)
	}

	if _, err := exec.Command("id", "-u", *sshUser).Output(); err != nil {
		fmt.Fprintf(os.Stderr, "ssh user %q does not exist on this host (create it first, e.g. `useradd -m %s`)\n", *sshUser, *sshUser)
		os.Exit(1)
	}
	host := *hostname
	if host == "" {
		host = detectPublicAddr()
	}
	if host == "" {
		fmt.Fprintln(os.Stderr, "could not auto-detect a public hostname/IP — pass --hostname=<addr>")
		os.Exit(1)
	}
	hostKey, _ := readLocalHostKey()

	body, _ := json.Marshal(vmJoinRequest{
		Name: *name, Hostname: host, SSHUser: *sshUser, HostKey: hostKey,
	})
	req, _ := http.NewRequest(http.MethodPost, centralURL+"/api/vm/join", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: joinHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "central: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, errBodySnippet))
		fmt.Fprintf(os.Stderr, "central returned %d: %s\n", resp.StatusCode, strings.TrimSpace(string(msg)))
		os.Exit(1)
	}
	var got vmJoinResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		fmt.Fprintf(os.Stderr, "decode response: %v\n", err)
		os.Exit(1)
	}
	if got.AccessPublicKey == "" {
		fmt.Fprintln(os.Stderr, "central returned no access_public_key")
		os.Exit(1)
	}
	if err := installJoinMaterial(*sshUser, got); err != nil {
		fmt.Fprintf(os.Stderr, "install ssh material: %v\n", err)
		os.Exit(1)
	}
	if err := writeWorkerEnv(centralURL, token, got.Name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save worker env (%v) — `orch run` will need --central/--token explicitly\n", err)
	}
	fmt.Printf("orch: joined as VM %q. Central can now SSH in as %s@%s.\n", got.Name, *sshUser, host)
	if got.BotGithubPrivateKey == "" {
		fmt.Println("warning: central did not push a bot github key — make sure this host has its own SSH access to github (e.g. `gh auth login` + ssh-keygen + add to github bot account) or claude sessions won't be able to `git clone` here.")
	}
}

// reorderFlagsFirst moves --flag / -flag tokens (and their values) to
// the front of args so flag.Parse picks them up regardless of placement.
// Stops splitting on `--` so trailing pass-through args still work.
func reorderFlagsFirst(args []string) []string {
	var flags, pos []string
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			// `--flag value` form: pull the next token too unless it
			// itself looks like a flag.
			if !strings.Contains(a, "=") && i+1 < len(args) {
				next := args[i+1]
				if !strings.HasPrefix(next, "-") {
					flags = append(flags, next)
					i += 2
					continue
				}
			}
			i++
			continue
		}
		pos = append(pos, a)
		i++
	}
	return append(flags, pos...)
}

func detectPublicAddr() string {
	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	var privateFallback string
	for _, a := range ifaces {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.IsLinkLocalUnicast() {
			continue
		}
		ip := ipnet.IP
		if ip.To4() == nil && ip.To16() != nil {
			if privateFallback == "" {
				privateFallback = ip.String()
			}
			continue
		}
		if ip.IsPrivate() {
			if privateFallback == "" {
				privateFallback = ip.String()
			}
			continue
		}
		return ip.String()
	}
	return privateFallback
}

// readLocalHostKey grabs the ed25519 host key sshd already published so
// the join response can carry it back to central for known_hosts. Best
// effort — empty result is fine.
func readLocalHostKey() (string, error) {
	for _, p := range []string{"/etc/ssh/ssh_host_ed25519_key.pub", "/etc/ssh/ssh_host_rsa_key.pub"} {
		b, err := os.ReadFile(p)
		if err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	return "", fmt.Errorf("no host key found")
}

// installJoinMaterial places central's access pubkey into the ssh user's
// authorized_keys and (when provided) drops the bot's github keypair so
// the worker can clone/push as the bot.
func installJoinMaterial(sshUser string, m vmJoinResponse) error {
	home, err := lookupHome(sshUser)
	if err != nil {
		return err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", sshDir, err)
	}

	authPath := filepath.Join(sshDir, "authorized_keys")
	marker := "# orchid-central:" + m.Name
	if err := appendAuthorizedKey(authPath, marker, m.AccessPublicKey); err != nil {
		return fmt.Errorf("authorized_keys: %w", err)
	}
	_ = os.Chmod(authPath, 0o600)

	if m.BotGithubPrivateKey != "" {
		privPath := filepath.Join(sshDir, "id_ed25519")
		if _, err := os.Stat(privPath); os.IsNotExist(err) {
			if err := os.WriteFile(privPath, []byte(ensureTrailingNL(m.BotGithubPrivateKey)), 0o600); err != nil {
				return fmt.Errorf("write %s: %w", privPath, err)
			}
			if m.BotGithubPublicKey != "" {
				_ = os.WriteFile(privPath+".pub", []byte(ensureTrailingNL(m.BotGithubPublicKey)), 0o644)
			}
		}
	}

	khPath := filepath.Join(sshDir, "known_hosts")
	if !knownHostHas(khPath, "github.com") {
		out, _ := exec.Command("ssh-keyscan", "-t", "ed25519,rsa", "github.com").Output()
		if len(out) > 0 {
			f, err := os.OpenFile(khPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err == nil {
				_, _ = f.Write(out)
				_ = f.Close()
			}
		}
	}

	_ = exec.Command("chown", "-R", sshUser+":"+sshUser, sshDir).Run()
	return nil
}

func ensureTrailingNL(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// appendAuthorizedKey writes (key) into (path) under a one-line marker
// comment so re-running `orch join vm` for the same name rotates the
// key in place rather than appending duplicates.
func appendAuthorizedKey(path, marker, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("empty key")
	}
	var existing []byte
	if b, err := os.ReadFile(path); err == nil {
		existing = b
	}
	out := stripMarkerBlock(existing, marker)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte{'\n'}) {
		out = append(out, '\n')
	}
	out = append(out, []byte(marker+"\n"+key+"\n")...)
	return os.WriteFile(path, out, 0o600)
}

// stripMarkerBlock returns src with any "# marker" line and the single
// line that follows removed. Used to make authorized_keys updates
// idempotent when central reissues an access key.
func stripMarkerBlock(src []byte, marker string) []byte {
	if len(src) == 0 {
		return src
	}
	lines := strings.Split(string(src), "\n")
	out := make([]string, 0, len(lines))
	skipNext := false
	for _, line := range lines {
		if skipNext {
			skipNext = false
			continue
		}
		if strings.TrimSpace(line) == marker {
			skipNext = true
			continue
		}
		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n"))
}

func knownHostHas(path, host string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, host+" ") || strings.HasPrefix(line, host+",") {
			return true
		}
	}
	return false
}

// lookupHome returns the home directory for a system user. Uses Go's
// os/user (cgo path on darwin reads dscl; pure-Go on linux reads
// /etc/passwd) so the worker join works on both platforms without
// shelling out to getent (which only exists on glibc systems).
func lookupHome(user string) (string, error) {
	u, err := osuser.Lookup(user)
	if err != nil {
		return "", fmt.Errorf("lookup %s: %w", user, err)
	}
	if u.HomeDir == "" {
		return "", fmt.Errorf("user %s has no home directory", user)
	}
	return u.HomeDir, nil
}

// handleVMJoin is the server-side counterpart to runJoinVM. Auth is
// handled by the wrapping `auth()` (Bearer == http_secret).
func handleVMJoin(w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	var req vmJoinRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return err
	}
	if strings.TrimSpace(req.Hostname) == "" || strings.TrimSpace(req.SSHUser) == "" {
		http.Error(w, "hostname and ssh_user required", http.StatusBadRequest)
		return fmt.Errorf("missing hostname/ssh_user")
	}
	if !validVMName(req.Name) && req.Name != "" {
		http.Error(w, "invalid name (alnum + dash/underscore only)", http.StatusBadRequest)
		return fmt.Errorf("invalid name %q", req.Name)
	}

	if globalConfigPath == "" {
		http.Error(w, "config path unknown", http.StatusInternalServerError)
		return fmt.Errorf("globalConfigPath unset")
	}
	src, err := os.ReadFile(globalConfigPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	var current Config
	if err := hclsimple.Decode(filepath.Base(globalConfigPath), src, nil, &current); err != nil {
		http.Error(w, "parse swarm.hcl: "+err.Error(), http.StatusInternalServerError)
		return err
	}

	name := req.Name
	if name == "" {
		// Reuse an existing join_managed block whose host+user match,
		// so a worker retrying after a failed install doesn't spawn a
		// duplicate vm-N entry every attempt.
		for _, v := range current.VMs {
			if v.JoinManaged && v.Host == req.Hostname && v.User == req.SSHUser {
				name = v.Name
				break
			}
		}
	}
	if name == "" {
		name = allocVMName(&current, req.Hostname)
	}

	keyPath, pubBytes, err := generateVMAccessKey(globalConfigPath, name)
	if err != nil {
		http.Error(w, "keygen: "+err.Error(), http.StatusInternalServerError)
		return err
	}

	botPriv, botPub := readBotGithubKey(current.Orch.BotGithubKey)

	patch := map[string]map[string]any{
		"vm." + name: {
			"host":         req.Hostname,
			"user":         req.SSHUser,
			"key":          keyPath,
			"capacity":     float64(4),
			"join_managed": true,
		},
	}
	out, err := patchHCL(src, patch)
	if err != nil {
		http.Error(w, "patch: "+err.Error(), http.StatusInternalServerError)
		return err
	}
	// Validate before touching disk — hclsimple.DecodeFile picks its
	// parser by extension, and `.tmp` blows it up. Decode in-memory
	// so the trial parse uses the real filename's parser.
	var trial Config
	if err := hclsimple.Decode(filepath.Base(globalConfigPath), out, nil, &trial); err != nil {
		http.Error(w, "swarm.hcl invalid after patch: "+err.Error(), http.StatusInternalServerError)
		return err
	}
	tmp := globalConfigPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	if err := os.Rename(tmp, globalConfigPath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}

	resp := vmJoinResponse{
		Name:                name,
		AccessPublicKey:     strings.TrimSpace(string(pubBytes)),
		BotGithubPrivateKey: botPriv,
		BotGithubPublicKey:  botPub,
	}
	log.Printf("vm join: registered %q at %s@%s (key=%s, github_key=%v)",
		name, req.SSHUser, req.Hostname, keyPath, botPriv != "")
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(resp)
}

// validVMName mirrors what HCL accepts as a block label and what
// filenames under vm-keys/ tolerate without quoting headaches.
var vmNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

func validVMName(name string) bool { return vmNamePattern.MatchString(name) }

// allocVMName picks the next free vm name. Prefers a hostname-derived
// slug ("vm-<sanitized-host>") then falls back to vm-N.
func allocVMName(cfg *Config, hostname string) string {
	used := map[string]bool{}
	for _, v := range cfg.VMs {
		used[v.Name] = true
	}
	if slug := slugifyHostname(hostname); slug != "" && !used[slug] {
		return slug
	}
	for i := 1; ; i++ {
		n := fmt.Sprintf("vm-%d", i)
		if !used[n] {
			return n
		}
	}
}

func slugifyHostname(h string) string {
	h = strings.ToLower(h)
	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	if i := strings.Index(h, "."); i >= 0 {
		h = h[:i]
	}
	var b strings.Builder
	for _, r := range h {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r == '-' || r == '_' {
			b.WriteRune('-')
		}
	}
	s := b.String()
	if s == "" {
		return ""
	}
	if s[0] >= '0' && s[0] <= '9' {
		s = "vm-" + s
	}
	if !validVMName(s) {
		return ""
	}
	return s
}

// generateVMAccessKey writes a fresh ed25519 keypair under
// <install_dir>/vm-keys/<name>. install_dir is derived from
// configPath's directory so the layout matches install.sh.
func generateVMAccessKey(configPath, name string) (string, []byte, error) {
	dir := filepath.Join(filepath.Dir(configPath), "vm-keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	keyPath := filepath.Join(dir, name)
	_ = os.Remove(keyPath)
	_ = os.Remove(keyPath + ".pub")
	cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-q", "-N", "", "-C", "orchid-central→"+name, "-f", keyPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("ssh-keygen: %v: %s", err, strings.TrimSpace(string(out)))
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return "", nil, fmt.Errorf("read pub: %w", err)
	}
	return keyPath, pub, nil
}

// handleAdhoc is the server-side counterpart to `orch run <title>`. A
// worker POSTs {title, vm} here; we ssh into that VM, spawn a tmux
// session running the VM's configured session_cmd, and record the
// result as a Job under a negative synthetic issue id so it surfaces
// in the dashboard alongside real PR work.
func handleAdhoc(w http.ResponseWriter, r *http.Request, cfg *Config, st *State) error {
	defer r.Body.Close()
	var req struct {
		Title string `json:"title"`
		VM    string `json:"vm"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return err
	}
	req.Title = strings.TrimSpace(req.Title)
	req.VM = strings.TrimSpace(req.VM)
	if req.Title == "" || req.VM == "" {
		http.Error(w, "title and vm required", http.StatusBadRequest)
		return fmt.Errorf("missing title/vm")
	}
	vm := vmByName(cfg, req.VM)
	if vm == nil {
		http.Error(w, "unknown vm "+req.VM, http.StatusBadRequest)
		return fmt.Errorf("unknown vm %q", req.VM)
	}

	// Synthetic id off the atomic counter — no st.mu wait, so /api/adhoc
	// returns even when tick() holds the state lock for a full pass.
	id := int(st.adhocSeq.Add(-1))
	session := fmt.Sprintf("adhoc-%d", -id)
	workdir := strings.TrimRight(vmWorkdirRoot(cfg.Orch, *vm), "/") + "/" + session

	sessionCmd := vm.SessionCmd
	if sessionCmd == "" {
		sessionCmd = "claude --dangerously-skip-permissions"
	}
	script := fmt.Sprintf(`set -e
mkdir -p %q
tmux kill-session -t %q 2>/dev/null || true
tmux new-session -d -c %q -s %q %q
`, workdir, session, workdir, session, sessionCmd)
	if _, errStr, err := sshExecIn(*vm, script, "bash -s"); err != nil {
		http.Error(w, "spawn: "+strings.TrimSpace(errStr), http.StatusInternalServerError)
		return fmt.Errorf("spawn: %v: %s", err, errStr)
	}

	st.mu.Lock()
	st.Jobs[id] = &Job{
		VM:         vm.Name,
		Tmux:       session,
		IssueTitle: req.Title,
		Lifecycle:  "adhoc",
	}
	st.mu.Unlock()
	if err := saveState(st); err != nil {
		log.Printf("adhoc: saveState: %v", err)
	}
	log.Printf("adhoc: spawned %s on %s — %q (id=%d)", session, vm.Name, req.Title, id)

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{"tmux": session, "id": id, "vm": vm.Name})
}

// readBotGithubKey returns the (private, public) bytes of the bot's
// github ssh key when discoverable. Empty strings on miss; the worker
// prints a warning but the join still succeeds.
func readBotGithubKey(configured string) (priv, pub string) {
	path := configured
	if path == "" {
		home, _ := os.UserHomeDir()
		if home == "" {
			return "", ""
		}
		path = filepath.Join(home, ".ssh", "id_ed25519")
	} else {
		path = expand(path)
	}
	pb, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	pubB, _ := os.ReadFile(path + ".pub")
	return string(pb), string(pubB)
}

// subcommands maps the first argv element to a handler that takes the
// remaining args. Centralised so adding a new top-level command means
// adding one entry here — no chained `if os.Args[1] == ...` ladders.
var subcommands = map[string]func([]string){
	"join": runJoin,
	"run":  runRun,
}

// defaultConfigPath is where a no-flag `orch` reads/creates its config:
// ~/.orch/swarm.hcl, falling back to ./swarm.hcl when HOME is unavailable
// (e.g. a systemd unit with no HOME set).
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "swarm.hcl"
	}
	return filepath.Join(home, ".orch", "swarm.hcl")
}

// scaffoldConfig writes a minimal, valid starter config so a fresh install
// boots straight into a usable dashboard. Everything beyond the bare minimum
// (inbox repo, targets, machines) is left empty for the user to fill in via
// the Settings panel. A random http_secret is generated and the ready-to-use
// dashboard URL is logged.
func scaffoldConfig(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	secret := hex.EncodeToString(buf)
	cfg := fmt.Sprintf(defaultConfigTemplate,
		filepath.Join(dir, "state.db"),
		filepath.Join(dir, "work"),
		secret,
	)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		return err
	}
	log.Printf("no config found — wrote a starter at %s", path)
	log.Printf("open the dashboard: http://localhost:8000/?token=%s", secret)
	return nil
}

// defaultConfigTemplate is a minimal valid config: %s = state_db path,
// workdir_root path, http_secret. No inbox/targets/machines — the user adds
// those in the dashboard. Until then orch runs but has nothing to dispatch.
const defaultConfigTemplate = `# Orchid config. Edit here or in the dashboard Settings panel.
github {
  inbox_repo = ""   # owner/repo where you file issues to dispatch work
}

orchestrator {
  poll_interval = "30s"
  state_db      = "%s"
  branch_prefix = "orch/issue-"
  workdir_root  = "%s"
  http_addr     = ":8000"
  http_secret   = "%s"   # dashboard token; set "" for no auth (trusted networks only)
}

# Add work repos: an issue labeled "x" in the inbox is cloned + worked here.
# target "example" {
#   label = "example"
#   repo  = "owner/repo"
# }

# Add a machine to run the swarm on. localhost needs claude/codex installed.
# machine "local" {
#   host = "localhost"
#   agent "claude" { capacity = 4, session_cmd = "claude --dangerously-skip-permissions" }
# }

bootstrap_prompt = <<EOT
You are an autonomous coding agent working issue #{{issue.number}} ({{issue.title}})
in {{target.repo}}. Clone is at {{workdir}} on branch {{branch}}.

Read the issue, make the change, open a pull request, and end the PR body with:
Closes {{inbox.repo}}#{{issue.number}}

Then stop and wait for review.
EOT
`

// workerEnvPath is where `orch join vm` saves enough state for later
// `orch run` invocations to phone home to central without re-typing
// the URL + token every time.
func workerEnvPath() string {
	if p := os.Getenv("ORCH_WORKER_ENV"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "orch", "worker.env")
}

func writeWorkerEnv(centralURL, token, vmName string) error {
	p := workerEnvPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	body := fmt.Sprintf("CENTRAL_URL=%s\nCENTRAL_TOKEN=%s\nVM_NAME=%s\n",
		centralURL, token, vmName)
	return os.WriteFile(p, []byte(body), 0o600)
}

func readWorkerEnv() (url, token, vm string, err error) {
	b, err := os.ReadFile(workerEnvPath())
	if err != nil {
		return "", "", "", err
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "CENTRAL_URL":
			url = strings.TrimSpace(v)
		case "CENTRAL_TOKEN":
			token = strings.TrimSpace(v)
		case "VM_NAME":
			vm = strings.TrimSpace(v)
		}
	}
	if url == "" || token == "" || vm == "" {
		return "", "", "", fmt.Errorf("incomplete worker env at %s", workerEnvPath())
	}
	return
}

// runRun is the worker-side `orch run <title>` entry point — pings
// central to spawn a tmux session on this host running the configured
// agent. The title becomes the card label; lifecycle is adhoc so the
// job sticks around until the operator kills the pane.
func runRun(args []string) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		fmt.Fprintln(os.Stderr, "usage: orch run <title>")
		os.Exit(2)
	}
	title := strings.Join(args, " ")
	centralURL, token, vmName, err := readWorkerEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "orch run: %v\n  (this host hasn't been joined yet — run `orch join vm <central-url> <token>` first)\n", err)
		os.Exit(1)
	}
	body, _ := json.Marshal(map[string]string{"title": title, "vm": vmName})
	req, _ := http.NewRequest(http.MethodPost, strings.TrimRight(centralURL, "/")+"/api/adhoc", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: adhocHTTPTimeout}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "orch run: central unreachable: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, errBodySnippet))
		fmt.Fprintf(os.Stderr, "orch run: central returned %d: %s\n", resp.StatusCode, strings.TrimSpace(string(msg)))
		os.Exit(1)
	}
	var reply struct {
		Tmux string `json:"tmux"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&reply)
	fmt.Printf("orch run: started %s — attaching (detach with Ctrl-b d).\n", reply.Tmux)
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		fmt.Fprintf(os.Stderr, "orch run: tmux not on PATH (%v) — pane is live on this host, attach manually: tmux attach -t %s\n", err, reply.Tmux)
		return
	}
	// Replace ourselves with tmux so the user lands directly in the
	// pane and Ctrl-C / detach behave normally.
	if err := syscall.Exec(tmux, []string{"tmux", "attach-session", "-t", reply.Tmux}, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "orch run: exec tmux: %v\n", err)
	}
}

func Main() {
	if len(os.Args) >= 2 {
		if h, ok := subcommands[os.Args[1]]; ok {
			h(os.Args[2:])
			return
		}
	}
	cfgPath := flag.String("config", "", "path to HCL config (default: ~/.orch/swarm.hcl, auto-created on first run)")
	describeFlag := flag.Bool("describe", false, "print a SKILL.md-shaped description of this instance and exit")
	captureOnly := flag.Bool("capture-only", false, "run only the /api/drafts capture HTTP server (no swarm polling, no VM bootstrap); requires a capture block in the config")
	relayURL := flag.String("relay", "", "outbound relay URL (e.g. wss://orchid.com/agent) — dashboard is reachable at <sub>.orchid.com without exposing this port")
	relayToken := flag.String("relay-token", "", "agent token issued by the relay on signup")
	flag.Parse()

	// Zero-config start: with no -config (and no file at the default path) we
	// scaffold a minimal swarm.hcl and boot from it, so a fresh user gets a
	// running dashboard to configure in — no hand-written config required. The
	// dashboard's Settings panel writes back to this same path.
	cfgFile := *cfgPath
	if cfgFile == "" {
		cfgFile = defaultConfigPath()
	}
	globalConfigPath = cfgFile

	if _, err := os.Stat(cfgFile); os.IsNotExist(err) {
		if err := scaffoldConfig(cfgFile); err != nil {
			log.Fatalf("create default config at %s: %v", cfgFile, err)
		}
	}

	var cfg Config
	if err := hclsimple.DecodeFile(cfgFile, nil, &cfg); err != nil {
		log.Fatalf("config: %v", err)
	}
	expandMachines(&cfg)
	// loadState opens the sqlite store. This is the only blocking I/O on the
	// main goroutine BEFORE the HTTP server + tick loop start, so if it hangs
	// the daemon serves nothing and (pre-this-guard) logged nothing — a silent
	// wedge. The usual cause is a stale prior instance still holding the WAL
	// lock across a fast restart. Bracket it with progress logs + a hard timeout
	// so the failure is loud and fast: fatal → systemd restarts cleanly instead
	// of spinning for minutes with an empty log.
	log.Printf("orch: loading state from %s", cfg.Orch.StateDB)
	var st *State
	{
		type loadRes struct {
			st  *State
			err error
		}
		ch := make(chan loadRes, 1)
		go func() {
			s, e := loadState(cfg.Orch.StateDB)
			ch <- loadRes{s, e}
		}()
		const loadTimeout = 20 * time.Second
		select {
		case r := <-ch:
			if r.err != nil {
				log.Fatalf("state: %v", r.err)
			}
			st = r.st
		case <-time.After(loadTimeout):
			log.Fatalf("state: loadState timed out after %s on %s — another orch instance likely still holding the sqlite lock. Confirm the previous process exited (systemctl status orchid; ps aux | grep '[o]rch -config') before restart.", loadTimeout, cfg.Orch.StateDB)
		}
	}
	log.Printf("orch: state loaded, %d jobs tracked", len(st.Jobs))
	if *captureOnly {
		if cfg.Orch.Capture == nil {
			log.Fatalf("-capture-only requires a `capture { ... }` block in the config")
		}
		if cfg.Orch.HTTPAddr == "" {
			log.Fatalf("-capture-only requires orchestrator.http_addr to be set")
		}
		log.Printf("orchid capture: listening on http://%s/, assets under %s",
			cfg.Orch.HTTPAddr, captureAssetsDirOrPlaceholder(&cfg))
		if err := http.ListenAndServe(cfg.Orch.HTTPAddr, httpHandler(&cfg, st)); err != nil {
			log.Fatalf("http: %v", err)
		}
		return
	}
	if *describeFlag {
		snap := make(map[int]Job, len(st.Jobs))
		for n, j := range st.Jobs {
			snap[n] = *j
		}
		st.httpSnap.Store(snap)
		fmt.Print(describe(&cfg, st, ""))
		return
	}
	interval, err := time.ParseDuration(cfg.Orch.PollInterval)
	if err != nil {
		log.Fatalf("poll_interval: %v", err)
	}
	// Auto-detect bot_login from gh's logged-in user. Operators don't
	// pin this in swarm.hcl any more — the install just starts the
	// daemon, the dashboard nudges you to connect GitHub, and once
	// gh's hosts.yml exists this resolves automatically on next boot
	// (or via hot-reload after the connect flow finishes).
	if cfg.Orch.BotLogin == "" {
		if out, _, err := run("gh", "api", "user", "--jq", ".login"); err == nil {
			cfg.Orch.BotLogin = strings.TrimSpace(out)
		}
	}
	for _, vm := range cfg.VMs {
		if login, _ := vmBotIdentity(cfg.Orch, vm); login == "" {
			log.Printf("vm %q: bot_login not set — sessions on this VM are paused until GitHub is connected from the dashboard.", vm.Name)
		}
	}
	tnames := make([]string, 0, len(cfg.Targets))
	for _, t := range cfg.Targets {
		tnames = append(tnames, fmt.Sprintf("%s(%s→%s)", t.Name, t.Label, t.Repo))
	}
	log.Printf("orch up: inbox=%s targets=[%s] vms=%d interval=%s tracked=%d",
		cfg.GitHub.InboxRepo, strings.Join(tnames, ","), len(cfg.VMs), interval, len(st.Jobs))

	if cfg.Orch.HTTPAddr != "" {
		if s := cfg.Orch.HTTPSecret; s == "" {
			log.Printf("SECURITY: orchestrator.http_secret is empty — dashboard is OPEN to anyone who can reach %s. Set http_secret to a 32+ char random string (openssl rand -hex 32).", cfg.Orch.HTTPAddr)
		} else if len(s) < 16 {
			log.Printf("SECURITY: orchestrator.http_secret is only %d chars — trivially brute-forceable. Replace with at least 32 hex chars (openssl rand -hex 32).", len(s))
		}
		go func() {
			log.Printf("http ui on http://%s/", cfg.Orch.HTTPAddr)
			if err := http.ListenAndServe(cfg.Orch.HTTPAddr, httpHandler(&cfg, st)); err != nil {
				log.Printf("http server: %v", err)
			}
		}()
	}

	go func() {
		// 4s (was 1s): this loop ssh-captures every live pane to detect the
		// needs-input prompt. The sparkline that wanted per-second resolution is
		// gone, so 4s cuts the ssh round-trips ~4x while keeping needs-input
		// detection responsive enough.
		t := time.NewTicker(4 * time.Second)
		defer t.Stop()
		for range t.C {
			var snap map[int]Job
			if v := st.httpSnap.Load(); v != nil {
				snap = v.(map[int]Job)
			}
			live := map[string]bool{}
			for _, j := range snap {
				if j.Tmux == "" {
					continue
				}
				live[j.Tmux] = true
				vm := vmByName(&cfg, j.VM)
				if vm == nil {
					continue
				}
				out, _, err := sshExec(*vm, fmt.Sprintf("tmux capture-pane -p -t %s 2>/dev/null | tail -8", j.Tmux))
				if err != nil {
					continue
				}
				paneActivityRecordTick(j.Tmux, fnv64(out))
				paneActionSet(j.Tmux, extractPaneAction(out))
				needs := panePrompted(out, vmAgent(*vm))
				if paneNeedsInputSet(j.Tmux, needs) {
					if st.Bcast != nil {
						select {
						case st.Bcast <- struct{}{}:
						default:
						}
					}
				}
			}
			paneActivityPrune(live)
			glancePrune(live)
			paneNeedsInputPrune(live)
		}
	}()

	if *relayURL != "" {
		if *relayToken == "" {
			log.Fatalf("-relay requires -relay-token (issued by the relay on signup)")
		}
		snapRead := func() []byte {
			b, err := st.store.GetSnap()
			if err != nil {
				return nil
			}
			return b
		}
		snapWrite := func(body []byte) error {
			if !json.Valid(body) {
				return fmt.Errorf("invalid json")
			}
			return st.store.PutSnap(body)
		}
		allowedLoginsProvider = func() []string { return append([]string(nil), cfg.Orch.AllowedLogins...) }
		go runRelayAgent(context.Background(), RelayDeps{
			URL:           *relayURL,
			Token:         *relayToken,
			HTTPSecret:    cfg.Orch.HTTPSecret,
			LocalAddr:     cfg.Orch.HTTPAddr,
			AllowedLogins: cfg.Orch.AllowedLogins,
			Handler:       httpHandler(&cfg, st),
			StateWake:     st.Bcast,
			StatePush:     func() []byte { return buildAPIStateJSON(&cfg, st) },
			SnapRead:      snapRead,
			SnapWrite:     snapWrite,
			PaneVM:        func(s string) *VMBlock { return lookupPaneVM(&cfg, st, s) },
		})
	}

	for i := range cfg.VMs {
		if err := bootstrapVM(cfg.VMs[i]); err != nil {
			log.Printf("vm %s: bootstrap FAILED: %v", cfg.VMs[i].Name, err)
		} else {
			log.Printf("vm %s: bootstrapped (github ssh ok)", cfg.VMs[i].Name)
		}
	}

	for i := range cfg.VMs {
		vm := cfg.VMs[i]
		// Each VM streams its agent's usage: claude via the statusline.jsonl
		// hook, codex via its rotating session rollouts. Both feed the per-agent
		// quota buckets the governor paces against.
		if vmAgent(vm).name == "codex" {
			go tailCodexUsage(context.Background(), vm, st.Bcast)
		} else {
			go tailStatusLine(context.Background(), vm, st.Bcast)
			// Reliable needs-input detection from claude's Notification /
			// UserPromptSubmit hooks (notify.jsonl), + ntfy on the rising edge.
			go tailNotify(context.Background(), vm, cfg.Orch.NtfyTopic, st.Bcast)
		}
	}

	for i := range cfg.VMs {
		vm := cfg.VMs[i]
		if !isLocal(vm) {
			continue
		}
		go runUsageScanLoop(context.Background(), projectsRootFor(vm), st.store, 5*time.Minute)
	}

	// Quota sampler: persist a reading of both rate-limit buckets every
	// SampleInterval into quota_samples so the governor's burn-rate estimator
	// has a time-series. Runs unconditionally (the sample is cheap and the
	// estimator only consumes it when the governor is enabled) so enabling the
	// governor later has immediate history to work from.
	go runQuotaSampleLoop(context.Background(), st.store, &cfg)
	// At-a-glance list signal: per-session git WIP stats (branch diff vs base).
	go runWipLoop(context.Background(), &cfg, st)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if cfg.Orch.Mentions != nil {
		if _, err := fetchMaintainers(cfg.Orch.Mentions.Org); err != nil {
			log.Fatalf("mentions: cannot read org %s members (token needs read:org and to be in the org): %v",
				cfg.Orch.Mentions.Org, err)
		}
		mentionInterval, err := time.ParseDuration(cfg.Orch.Mentions.PollInterval)
		if err != nil || mentionInterval == 0 {
			mentionInterval = 5 * time.Minute
		}
		go func() {
			mt := time.NewTicker(mentionInterval)
			defer mt.Stop()
			mentionTick(&cfg, st)
			for {
				select {
				case <-ctx.Done():
					return
				case <-mt.C:
					mentionTick(&cfg, st)
				}
			}
		}()
		log.Printf("mentions: poller started, every %s, org=%s", mentionInterval, cfg.Orch.Mentions.Org)
	}

	if len(botLogins(&cfg)) > 0 {
		go func() {
			at := time.NewTicker(assignmentTickInterval)
			defer at.Stop()
			assignmentTick(&cfg, st)
			for {
				select {
				case <-ctx.Done():
					return
				case <-at.C:
					assignmentTick(&cfg, st)
				}
			}
		}()
		log.Printf("assignments: poller started, every 60s, bots=%v", botLogins(&cfg))
	}

	go func() {
		pt := time.NewTicker(time.Hour)
		defer pt.Stop()
		pruneOrphanWorkdirs(&cfg, st)
		for {
			select {
			case <-ctx.Done():
				return
			case <-pt.C:
				pruneOrphanWorkdirs(&cfg, st)
			}
		}
	}()

	go runVMHealthLoop(ctx, &cfg, st)

	// Memory store (git-backed): clone synchronously before the first tick so the
	// per-target dirs exist before any session writes into them, then keep it
	// committed + pushed on a timer.
	if memoryOn(&cfg) {
		if head, err := memorySyncOnce(&cfg); err != nil {
			log.Printf("memory: initial clone/sync failed (will retry): %v", err)
		} else {
			log.Printf("memory: store ready (%s@%s %s, every %s)", memRepo(&cfg), memBranch(&cfg), head, memInterval(&cfg))
		}
		go runMemorySyncLoop(ctx, &cfg)
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	tick(&cfg, st)
	for {
		select {
		case <-ctx.Done():
			log.Printf("shutdown")
			return
		case <-t.C:
			tick(&cfg, st)
		}
	}
}
