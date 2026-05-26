package orch

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var validRepo = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// DraftPayload mirrors capture/DRAFT_PAYLOAD.md.
type DraftPayload struct {
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"createdAt"`
	Source    string        `json:"source"`
	Kind      string        `json:"kind"`
	Note      string        `json:"note"`
	Image     *DraftImage   `json:"image,omitempty"`
	Link      *DraftLink    `json:"link,omitempty"`
	Text      *DraftText    `json:"text,omitempty"`
	Voice     *DraftVoice   `json:"voice,omitempty"`
	Context   *DraftContext `json:"context,omitempty"`
	Target    *DraftTarget  `json:"target,omitempty"`
}

type DraftImage struct {
	Mime        string `json:"mime"`
	BytesBase64 string `json:"bytes_base64"`
}

type DraftLink struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

type DraftText struct {
	Body      string `json:"body"`
	OriginURL string `json:"originURL,omitempty"`
}

type DraftVoice struct {
	Mime        string  `json:"mime"`
	BytesBase64 string  `json:"bytes_base64"`
	DurationSec float64 `json:"durationSec"`
}

type DraftContext struct {
	AppName     string `json:"appName,omitempty"`
	WindowTitle string `json:"windowTitle,omitempty"`
	Selection   string `json:"selection,omitempty"`
}

type DraftTarget struct {
	Repo   string   `json:"repo,omitempty"`
	Labels []string `json:"labels,omitempty"`
}

// captureAssetsDirOrPlaceholder is the startup log's best effort at showing
// where assets will land. It doesn't fail-fast on permission errors — the
// per-request handler does that.
func captureAssetsDirOrPlaceholder(cfg *Config) string {
	if dir, err := captureAssetsDir(cfg); err == nil {
		return dir
	}
	if cfg.Orch.Capture != nil && cfg.Orch.Capture.AssetsDir != "" {
		return cfg.Orch.Capture.AssetsDir
	}
	return "(unset)"
}

// captureAssetsDir resolves the asset directory, creating it if needed.
// Defaults to <dir of state_db>/captures when AssetsDir isn't configured,
// so a fresh `capture`-block-only install Just Works.
func captureAssetsDir(cfg *Config) (string, error) {
	dir := cfg.Orch.Capture.AssetsDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(cfg.Orch.StateDB), "captures")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func registerCaptureRoutes(mux *http.ServeMux, cfg *Config) {
	cap := cfg.Orch.Capture

	expectToken := []byte(cap.AuthToken)
	mux.HandleFunc("/api/drafts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		got := r.Header.Get("X-Capture-Token")
		if len(expectToken) == 0 ||
			subtle.ConstantTimeCompare([]byte(got), expectToken) != 1 {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}

		maxMB := cap.MaxBodyMB
		if maxMB <= 0 {
			maxMB = 12
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, int64(maxMB)<<20))
		if err != nil {
			http.Error(w, "body read: "+err.Error(), http.StatusBadRequest)
			return
		}
		var d DraftPayload
		if err := json.Unmarshal(body, &d); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if d.ID == "" {
			http.Error(w, "draft.id required", http.StatusBadRequest)
			return
		}
		if d.Kind == "" {
			http.Error(w, "draft.kind required", http.StatusBadRequest)
			return
		}

		repo, labels, terr := resolveDraftTarget(cfg, &d)
		if terr != nil {
			http.Error(w, terr.Error(), http.StatusForbidden)
			return
		}
		if !validRepo.MatchString(repo) {
			http.Error(w, "bad repo: "+repo, http.StatusBadRequest)
			return
		}

		assetPath, assetURL, err := persistDraftAsset(cfg, &d)
		if err != nil {
			http.Error(w, "asset write: "+err.Error(), http.StatusInternalServerError)
			return
		}

		title, issueBody := renderDraftIssue(&d, assetURL, assetPath)

		args := []string{"issue", "create", "--repo", repo, "--title", title, "--body", issueBody}
		for _, l := range labels {
			args = append(args, "--label", l)
		}
		out, errStr, err := run("gh", args...)
		if err != nil {
			http.Error(w, "gh issue create: "+strings.TrimSpace(errStr)+": "+err.Error(),
				http.StatusBadGateway)
			return
		}

		issueURL := strings.TrimSpace(out)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":        true,
			"id":        d.ID,
			"issue_url": issueURL,
			"asset_url": assetURL,
		})
	})

	mux.HandleFunc("/captures/", func(w http.ResponseWriter, r *http.Request) {
		dir, err := captureAssetsDir(cfg)
		if err != nil {
			http.Error(w, "assets dir: "+err.Error(), http.StatusInternalServerError)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/captures/")
		if name == "" || strings.HasPrefix(name, ".") ||
			strings.Contains(name, "..") ||
			strings.ContainsRune(name, '/') ||
			strings.ContainsRune(name, '\\') ||
			strings.ContainsRune(name, 0) {
			http.Error(w, "bad name", http.StatusBadRequest)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, name))
	})
}

// persistDraftAsset writes the image/voice blob (if any) under the assets dir
// and returns the on-disk path plus the public URL the issue body should
// embed. For drafts that carry no blob it returns empty strings.
func persistDraftAsset(cfg *Config, d *DraftPayload) (string, string, error) {
	var bytesB64, mime, ext string
	switch d.Kind {
	case "screenshot":
		if d.Image == nil || d.Image.BytesBase64 == "" {
			return "", "", nil
		}
		bytesB64 = d.Image.BytesBase64
		mime = d.Image.Mime
		ext = guessExtForMime(mime, ".png")
	case "voice":
		if d.Voice == nil || d.Voice.BytesBase64 == "" {
			return "", "", nil
		}
		bytesB64 = d.Voice.BytesBase64
		mime = d.Voice.Mime
		ext = guessExtForMime(mime, ".m4a")
	default:
		return "", "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(bytesB64)
	if err != nil {
		return "", "", fmt.Errorf("base64 decode: %w", err)
	}
	dir, err := captureAssetsDir(cfg)
	if err != nil {
		return "", "", err
	}
	name := safeDraftFilename(d.ID) + ext
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return "", "", err
	}
	base := strings.TrimRight(cfg.Orch.Capture.PublicURL, "/")
	url := ""
	if base != "" {
		url = base + "/captures/" + name
	}
	return path, url, nil
}

func guessExtForMime(mime, fallback string) string {
	switch strings.ToLower(mime) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/tiff":
		return ".tiff"
	case "audio/m4a", "audio/mp4", "audio/aac", "audio/x-m4a":
		return ".m4a"
	case "audio/wav", "audio/wave", "audio/x-wav":
		return ".wav"
	}
	return fallback
}

// safeDraftFilename strips anything that isn't alphanum, dash, underscore so
// /captures/<id> URLs stay predictable and impossible to weaponise as a path.
func safeDraftFilename(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '-' || r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return fmt.Sprintf("draft-%d", time.Now().UnixNano())
	}
	return b.String()
}

func renderDraftIssue(d *DraftPayload, assetURL, assetPath string) (string, string) {
	title := firstLine(d.Note)
	if title == "" {
		switch d.Kind {
		case "screenshot":
			title = "Captured screenshot"
		case "link":
			if d.Link != nil {
				title = "Captured link: " + truncate(d.Link.URL, 64)
			} else {
				title = "Captured link"
			}
		case "voice":
			title = fmt.Sprintf("Captured voice note (%.1fs)", durationOf(d))
		case "text":
			title = "Captured text"
		default:
			title = "Captured idea"
		}
	}
	title = truncate(title, 80)

	var b strings.Builder
	if d.Note != "" {
		b.WriteString(d.Note)
		b.WriteString("\n\n")
	}
	switch d.Kind {
	case "screenshot":
		if assetURL != "" {
			fmt.Fprintf(&b, "![screenshot](%s)\n", assetURL)
		} else if assetPath != "" {
			fmt.Fprintf(&b, "_screenshot saved to `%s` (not embedded — orch capture has no public_url configured)_\n", assetPath)
		}
	case "link":
		if d.Link != nil {
			fmt.Fprintf(&b, "**link:** %s\n", d.Link.URL)
			if d.Link.Title != "" {
				fmt.Fprintf(&b, "_%s_\n", d.Link.Title)
			}
		}
	case "text":
		if d.Text != nil {
			b.WriteString("\n**selected text:**\n\n> ")
			b.WriteString(strings.ReplaceAll(d.Text.Body, "\n", "\n> "))
			b.WriteString("\n")
			if d.Text.OriginURL != "" {
				fmt.Fprintf(&b, "\n_from %s_\n", d.Text.OriginURL)
			}
		}
	case "voice":
		fmt.Fprintf(&b, "Voice note · %.1fs\n", durationOf(d))
		if assetURL != "" {
			fmt.Fprintf(&b, "\n[audio](%s)\n", assetURL)
		} else if assetPath != "" {
			fmt.Fprintf(&b, "\n_audio saved to `%s`_\n", assetPath)
		}
	}
	if d.Context != nil {
		var ctxBits []string
		if d.Context.AppName != "" {
			ctxBits = append(ctxBits, "app=`"+d.Context.AppName+"`")
		}
		if d.Context.WindowTitle != "" {
			ctxBits = append(ctxBits, "window=`"+truncate(d.Context.WindowTitle, 80)+"`")
		}
		if len(ctxBits) > 0 {
			b.WriteString("\n<sub>context: " + strings.Join(ctxBits, " · ") + "</sub>\n")
		}
	}
	fmt.Fprintf(&b, "\n<sub>captured from %s at %s · draft `%s`</sub>\n",
		d.Source, d.CreatedAt.UTC().Format(time.RFC3339), d.ID)
	return title, b.String()
}

func resolveDraftTarget(cfg *Config, d *DraftPayload) (string, []string, error) {
	defaultRepo := cfg.Orch.Capture.DefaultRepo
	if defaultRepo == "" {
		defaultRepo = cfg.GitHub.InboxRepo
	}
	var labels []string
	repo := ""
	if d.Target != nil {
		repo = strings.TrimSpace(d.Target.Repo)
		for _, l := range d.Target.Labels {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "-") {
				continue
			}
			labels = append(labels, l)
		}
	}
	if repo == "" {
		repo = defaultRepo
	} else if repo != defaultRepo {
		ok := false
		for _, allowed := range cfg.Orch.Capture.AllowedRepos {
			if repo == allowed {
				ok = true
				break
			}
		}
		if !ok {
			return "", nil, fmt.Errorf("repo %q not in capture.allowed_repos", repo)
		}
	}
	if len(labels) == 0 {
		labels = append(labels, cfg.Orch.Capture.DefaultLabels...)
	}
	return repo, labels, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

func durationOf(d *DraftPayload) float64 {
	if d.Voice != nil {
		return d.Voice.DurationSec
	}
	return 0
}
