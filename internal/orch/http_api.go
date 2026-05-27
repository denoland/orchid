package orch

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

const (
	// Cap on /api/pane POST bodies. Pane input is keystrokes — never
	// large; oversized requests are dropped at the limit reader.
	maxPaneInputBytes = 4096
	// SSE keepalive cadence for /api/pane/stream so middleboxes don't
	// idle-close the connection between frames.
	paneStreamKeepalive = 20 * time.Second
)

type apiJobEntry struct {
	Issue int `json:"issue"`
	Job
	Activity   []int         `json:"activity,omitempty"`
	Usage      *apiPaneUsage `json:"usage,omitempty"`
	NeedsInput bool          `json:"needs_input,omitempty"`
	VMOnline   bool          `json:"vm_online"`
}

type apiVMEntry struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Capacity int    `json:"capacity"`
	Used     int    `json:"used"`
	Bot      string `json:"bot"`
	Agent    string `json:"agent"`
	Online   bool   `json:"online"`
	LastErr  string `json:"last_err,omitempty"`
}

type apiStateResp struct {
	Jobs  []apiJobEntry `json:"jobs"`
	VMs   []apiVMEntry  `json:"vms"`
	Inbox string        `json:"inbox"`
	Quota *apiQuota     `json:"quota,omitempty"`
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

func buildAPIStateJSON(cfg *Config, st *State) []byte {
	var snap map[int]Job
	if v := st.httpSnap.Load(); v != nil {
		snap = v.(map[int]Job)
	}
	// Per-VM online flag — unprobed VMs (LastOK zero) are treated as
	// online so a freshly started orch doesn't grey the dashboard.
	vmOnline := map[string]bool{}
	for _, vm := range cfg.VMs {
		h := st.VMHealth(vm.Name)
		vmOnline[vm.Name] = h.Online || h.LastOK.IsZero() || isLocal(vm)
	}

	load := map[string]int{}
	jobs := make([]apiJobEntry, 0, len(snap))
	for issue, j := range snap {
		entry := apiJobEntry{
			Issue:      issue,
			Job:        j,
			Activity:   paneActivitySnapshot(j.Tmux),
			NeedsInput: paneNeedsInputSnapshot(j.Tmux),
			VMOnline:   vmOnline[j.VM],
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
		h := st.VMHealth(vm.Name)
		vms = append(vms, apiVMEntry{
			Name:     vm.Name,
			Host:     vm.Host,
			Capacity: vm.Capacity,
			Used:     load[vm.Name],
			Bot:      bot,
			Agent:    vmAgent(vm).name,
			Online:   vmOnline[vm.Name],
			LastErr:  h.LastErr,
		})
	}
	resp := apiStateResp{
		Jobs:  jobs,
		VMs:   vms,
		Inbox: cfg.GitHub.InboxRepo,
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
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusUnauthorized)
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

	secretMatches := func(tok string) bool {
		if tok == "" {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(tok), secretBytes) == 1
	}

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

	mux.HandleFunc("/api/state", auth(func(w http.ResponseWriter, r *http.Request) {
		body := buildAPIStateJSON(cfg, st)
		etag := fmt.Sprintf(`W/"%x"`, fnv64(string(body)))
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))

	paneVM := func(session string) *VMBlock { return lookupPaneVM(cfg, st, session) }

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
		body, err := io.ReadAll(io.LimitReader(r.Body, maxPaneInputBytes))
		if err != nil || len(body) == 0 {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
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
					}
				} else {
					buf.WriteByte(b)
				}
			}
		}()

		gzbuf := new(bytes.Buffer)
		gzwriter := gzip.NewWriter(gzbuf)
		gzipFrame := func(s string) string {
			gzbuf.Reset()
			gzwriter.Reset(gzbuf)
			_, _ = gzwriter.Write([]byte(s))
			_ = gzwriter.Close()
			return "z:" + base64.StdEncoding.EncodeToString(gzbuf.Bytes())
		}

		keepalive := time.NewTicker(paneStreamKeepalive)
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
			var trial Config
			if err := hclsimple.Decode(filepath.Base(path), out, nil, &trial); err != nil {
				http.Error(w, "invalid hcl after patch: "+err.Error(), http.StatusBadRequest)
				return
			}
			tmp := path + ".tmp"
			if err := os.WriteFile(tmp, out, 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if err := os.Rename(tmp, path); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			cfg.Orch.AllowedLogins = append([]string(nil), trial.Orch.AllowedLogins...)
			pushAllowedLogins(cfg.Orch.AllowedLogins)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "GET/PUT/POST only", http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc("/api/vm/join", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := handleVMJoin(w, r); err != nil {
			log.Printf("vm join: %v", err)
		}
	}))

	mux.HandleFunc("/api/adhoc", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := handleAdhoc(w, r, cfg, st); err != nil {
			log.Printf("adhoc: %v", err)
		}
	}))

	if cfg.Orch.Capture != nil {
		registerCaptureRoutes(mux, cfg)
	}

	spaFS, _ := fs.Sub(wwwFS, "embed-dist")
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
