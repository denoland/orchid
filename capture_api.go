package main

// Orchid Capture intake. Two HTTP endpoints wired into the orch HTTP server
// when the operator sets the `capture` config block:
//
//	POST /api/drafts        — accepts a Draft JSON, files a GitHub issue
//	GET  /captures/<id>     — serves the binary asset (image/voice) so the
//	                          GitHub issue body can embed it via a public URL
//
// See capture/DRAFT_PAYLOAD.md for the wire format and capture/README.md for
// the product context. The handler is intentionally narrow:
//   - one POST → one issue
//   - assets are written to disk under cfg.Orch.Capture.AssetsDir and served
//     by orch itself (no S3, no gist fallback) so the whole flow is owned by
//     one binary you already trust
//   - failures return HTTP errors and write nothing partial — clients retry
//     by replaying the same Draft (drafts carry their own id; idempotency at
//     issue level is the GitHub layer's problem, not ours yet)

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DraftPayload mirrors capture/DRAFT_PAYLOAD.md.
type DraftPayload struct {
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"createdAt"`
	Source    string        `json:"source"` // "macos" | "ios"
	Kind      string        `json:"kind"`   // "screenshot" | "link" | "text" | "voice"
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

// captureAssetsDir resolves the asset directory, creating it if needed.
// Defaults to <dir of state_file>/captures when AssetsDir isn't configured,
// so a fresh `capture`-block-only install Just Works.
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

func captureAssetsDir(cfg *Config) (string, error) {
	dir := cfg.Orch.Capture.AssetsDir
	if dir == "" {
		dir = filepath.Join(filepath.Dir(cfg.Orch.StateFile), "captures")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func registerCaptureRoutes(mux *http.ServeMux, cfg *Config) {
	cap := cfg.Orch.Capture

	mux.HandleFunc("/api/drafts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		// Cheap CORS so the macOS app + a hypothetical web composer can hit
		// this from anywhere. Token auth still gates access.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if got := r.Header.Get("X-Capture-Token"); got != cap.AuthToken {
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

		assetPath, assetURL, err := persistDraftAsset(cfg, &d)
		if err != nil {
			http.Error(w, "asset write: "+err.Error(), http.StatusInternalServerError)
			return
		}

		title, issueBody := renderDraftIssue(&d, assetURL, assetPath)
		repo, labels := resolveDraftTarget(cfg, &d)

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

	// /captures/<id>.<ext> — static file server scoped to the assets dir.
	// Anyone with the URL can read; that's deliberate so GitHub can render
	// the image inline in the issue body without an authenticated fetch.
	mux.HandleFunc("/captures/", func(w http.ResponseWriter, r *http.Request) {
		dir, err := captureAssetsDir(cfg)
		if err != nil {
			http.Error(w, "assets dir: "+err.Error(), http.StatusInternalServerError)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/captures/")
		// Defense in depth: reject any path that tries to climb out.
		if name == "" || strings.Contains(name, "..") || strings.ContainsRune(name, '/') {
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

// renderDraftIssue produces the GitHub issue title and body. Deterministic
// formatting only — no LLM in the prototype. The first line of the note
// becomes the title (truncated to 80 chars), or a sensible fallback if the
// note is empty. The body has the note + a typed block per kind.
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

func resolveDraftTarget(cfg *Config, d *DraftPayload) (string, []string) {
	repo := ""
	var labels []string
	if d.Target != nil {
		repo = d.Target.Repo
		labels = append([]string(nil), d.Target.Labels...)
	}
	if repo == "" {
		repo = cfg.Orch.Capture.DefaultRepo
	}
	if repo == "" {
		repo = cfg.GitHub.InboxRepo
	}
	if len(labels) == 0 {
		labels = append(labels, cfg.Orch.Capture.DefaultLabels...)
	}
	return repo, labels
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
