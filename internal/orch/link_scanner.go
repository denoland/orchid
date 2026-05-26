package orch

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"regexp"
	"sync"
)

// URLs we auto-promote into link cards on the canvas. The regex captures
// the full URL; variant classification mirrors what the SPA's
// detectVariant() does — see whiteboard/src/nodes.tsx.
var linkURLRe = regexp.MustCompile(
	`https?://(?:` +
		`gist\.github\.com/[\w./-]+|` +
		`github\.com/[\w./-]+(?:/[\w./?&=-]+)?|` +
		`docs\.google\.com/[\w./?&=#-]+|` +
		`(?:www\.)?notion\.so/[\w./?&=-]+|` +
		`(?:www\.)?youtube\.com/[\w./?&=-]+|` +
		`youtu\.be/[\w./?&=-]+|` +
		`pastebin\.com/[\w/]+|` +
		`vercel\.app/[\w./?&=-]*` +
		`)`,
)

// seenLinks tracks URLs we've already promoted, per tmux session. Cleared
// when the session goes away. Bounded by the cardinality of jobs — tiny.
var (
	seenLinksMu sync.Mutex
	seenLinks   = map[string]map[string]bool{} // tmux → url → seen
)

func seenLinksHas(tmux, url string) bool {
	seenLinksMu.Lock()
	defer seenLinksMu.Unlock()
	if seenLinks[tmux] == nil {
		return false
	}
	return seenLinks[tmux][url]
}

func seenLinksMark(tmux, url string) {
	seenLinksMu.Lock()
	defer seenLinksMu.Unlock()
	if seenLinks[tmux] == nil {
		seenLinks[tmux] = map[string]bool{}
	}
	seenLinks[tmux][url] = true
}

func seenLinksPrune(live map[string]bool) {
	seenLinksMu.Lock()
	defer seenLinksMu.Unlock()
	for tmux := range seenLinks {
		if !live[tmux] {
			delete(seenLinks, tmux)
		}
	}
}

// detectLinkVariant mirrors www/whiteboard/src/nodes.tsx detectVariant so
// the auto-injected node renders the same way as a user-pasted one.
func detectLinkVariant(url string) string {
	switch {
	case regexp.MustCompile(`^https?://gist\.github\.com/`).MatchString(url):
		return "gist"
	case regexp.MustCompile(`^https?://github\.com/[^/]+/[^/]+/pull/\d+`).MatchString(url):
		return "pr"
	case regexp.MustCompile(`^https?://github\.com/[^/]+/[^/]+/issues/\d+`).MatchString(url):
		return "issue"
	case regexp.MustCompile(`^https?://github\.com/`).MatchString(url):
		return "github-code"
	case regexp.MustCompile(`^https?://(?:www\.)?youtube\.com/|^https?://youtu\.be/`).MatchString(url):
		return "youtube"
	case regexp.MustCompile(`^https?://docs\.google\.com/`).MatchString(url):
		return "docs"
	}
	return "generic"
}

// ulidish is a short, sortable-enough id for new canvas nodes. We use
// hex of crypto/rand because the SPA only cares that the id is stable
// + unique within the snap.
func ulidish() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// canvasInjectLinks looks at the tail of a pane capture, finds new
// URLs, and appends a link node to the persisted snap for each one. The
// new node is positioned next to the originating card. Returns the
// number of links injected.
func canvasInjectLinks(st *State, tmux, paneOut string) int {
	matches := linkURLRe.FindAllString(paneOut, -1)
	if len(matches) == 0 {
		return 0
	}
	// First pass: dedupe + filter to new ones.
	fresh := make([]string, 0, len(matches))
	dedup := map[string]bool{}
	for _, u := range matches {
		if dedup[u] || seenLinksHas(tmux, u) {
			continue
		}
		dedup[u] = true
		fresh = append(fresh, u)
	}
	if len(fresh) == 0 {
		return 0
	}

	// Load + mutate the snap. Skip if we can't parse — the canvas
	// shape is owned by the SPA and we don't want to clobber on
	// version drift.
	body, err := st.store.GetSnap()
	if err != nil || len(body) == 0 {
		body = []byte(`{}`)
	}
	var snap map[string]any
	if err := json.Unmarshal(body, &snap); err != nil {
		return 0
	}
	users, _ := snap["user"].([]any)
	cards, _ := snap["cards"].(map[string]any)
	anchor, _ := cards[tmux].(map[string]any)
	bx, by := 80.0, 80.0
	if anchor != nil {
		if v, ok := anchor["x"].(float64); ok {
			bx = v + 340
		}
		if v, ok := anchor["y"].(float64); ok {
			by = v
		}
	}

	for i, url := range fresh {
		node := map[string]any{
			"type": "link",
			"id":   "auto-" + ulidish(),
			"x":    bx,
			"y":    by + float64(i*180),
			"data": map[string]any{
				"url":     url,
				"title":   url,
				"variant": detectLinkVariant(url),
			},
		}
		users = append(users, node)
		seenLinksMark(tmux, url)
	}
	snap["user"] = users
	out, err := json.Marshal(snap)
	if err != nil {
		return 0
	}
	if err := st.store.PutSnap(out); err != nil {
		log.Printf("canvas inject: PutSnap failed: %v", err)
		return 0
	}
	pushSnap(out)
	log.Printf("canvas: injected %d link(s) for %s", len(fresh), tmux)
	return len(fresh)
}
