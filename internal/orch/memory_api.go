package orch

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The swarm's shared knowledge base is Claude's auto-memory, redirected per
// target repo (CLAUDE_COWORK_MEMORY_PATH_OVERRIDE in tmuxStart): each repo
// accumulates its own notes under <store-base>/<owner>-<repo>/. The dashboard
// reads the whole base — local when the orchestrator runs on the store's host
// (the common deploy), over SSH otherwise — groups notes by target, and
// resolves [[links]] across targets (cross-repo references are fine).

// MemoryNode is one note plus the metadata the dashboard tree/search/backlinks
// need. Built by reading the note files directly (not MEMORY.md, which Claude
// truncates past 25KB and so can't be the source of truth at scale).
type MemoryNode struct {
	File      string   `json:"file"`      // store-relative path, e.g. denoland-deno/build-prereqs.md
	Target    string   `json:"target"`    // owner/repo from the subdir ("" = shared/top-level)
	Name      string   `json:"name"`      // frontmatter name (falls back to file stem)
	Summary   string   `json:"summary"`   // frontmatter description / first line
	Links     []string `json:"links"`     // outbound, resolved to existing note files
	Backlinks []string `json:"backlinks"` // inbound, note files that link here
}

// memoryStore picks the VM whose auto-memory base the dashboard reads, plus the
// absolute base dir on that VM. Prefers a local claude VM (zero-SSH read),
// otherwise the first claude VM with a known session_home. dir is "" when no
// usable store location can be derived.
func memoryStore(cfg *Config) (vm *VMBlock, dir string) {
	pick := func(v *VMBlock) string {
		if memoryOn(cfg) {
			return memoryStoreDir(cfg, v) // git-backed repo clone
		}
		return filepath.Join(v.SessionHome, ".claude", "auto-memory") // local fallback
	}
	var fallback *VMBlock
	for i := range cfg.VMs {
		v := &cfg.VMs[i]
		if vmAgent(*v).name != "claude" || v.SessionHome == "" {
			continue
		}
		if isLocal(*v) {
			return v, pick(v)
		}
		if fallback == nil {
			fallback = v
		}
	}
	if fallback != nil {
		return fallback, pick(fallback)
	}
	return nil, ""
}

// memoryRelRe allows a note name optionally nested under owner/repo dirs. The
// explicit ".." reject blocks traversal even though dots are otherwise allowed
// in path components.
var memoryRelRe = regexp.MustCompile(`^([A-Za-z0-9._-]+/)*[A-Za-z0-9._-]+\.md$`)

func validRel(rel string) bool {
	return rel != "" && !strings.Contains(rel, "..") && memoryRelRe.MatchString(rel)
}

// readStoreFile returns the bytes of a note inside the store base. rel is a
// store-relative path (note.md or slug/note.md), guarded against traversal.
func readStoreFile(vm *VMBlock, base, rel string) ([]byte, error) {
	if !validRel(rel) {
		return nil, fmt.Errorf("invalid memory path")
	}
	full := filepath.Join(base, filepath.FromSlash(rel))
	if vm == nil || isLocal(*vm) {
		return os.ReadFile(full)
	}
	out, errStr, err := sshExec(*vm, fmt.Sprintf("cat %q", full))
	if err != nil {
		return nil, fmt.Errorf("read %s: %v: %s", rel, err, strings.TrimSpace(errStr))
	}
	return []byte(out), nil
}

// listStoreNotes lists note files (store-relative, slash-separated) at any depth
// under the store base — notes live under <owner>/<repo>/. MEMORY.md (the
// per-store index) and the .git dir are excluded.
func listStoreNotes(vm *VMBlock, base string) ([]string, error) {
	keep := func(name string) bool { return strings.HasSuffix(name, ".md") && name != "MEMORY.md" }
	if vm == nil || isLocal(*vm) {
		var out []string
		err := filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			if keep(d.Name()) {
				if rel, e := filepath.Rel(base, p); e == nil {
					out = append(out, filepath.ToSlash(rel))
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	o, errStr, err := sshExec(*vm, fmt.Sprintf("cd %q 2>/dev/null && find . -path ./.git -prune -o -name '*.md' -type f -print", base))
	if err != nil {
		return nil, fmt.Errorf("list store: %v: %s", err, strings.TrimSpace(errStr))
	}
	var out []string
	for _, line := range strings.Split(o, "\n") {
		rel := strings.TrimPrefix(strings.TrimSpace(line), "./")
		if rel != "" && keep(path.Base(rel)) {
			out = append(out, rel)
		}
	}
	return out, nil
}

var (
	fmNameRe = regexp.MustCompile(`(?m)^name:\s*(.+?)\s*$`)
	fmDescRe = regexp.MustCompile(`(?m)^description:\s*(.+?)\s*$`)
	wikiRe   = regexp.MustCompile(`\[\[([^\]]+?)\]\]`)
	mdLinkRe = regexp.MustCompile(`\]\(([A-Za-z0-9._/-]+\.md)\)`)
)

func splitFrontmatter(s string) (fm, body string) {
	if !strings.HasPrefix(s, "---") {
		return "", s
	}
	rest := strings.TrimPrefix(s, "---")
	if i := strings.Index(rest, "\n---"); i >= 0 {
		return rest[:i], rest[i+len("\n---"):]
	}
	return "", s
}

// normSlug folds a note slug/filename to a comparison key so wikilink targets
// resolve across `_` vs `-`, case, and any directory prefix agents introduce.
func normSlug(s string) string {
	s = path.Base(strings.TrimSpace(s))
	s = strings.TrimSuffix(strings.ToLower(s), ".md")
	return strings.NewReplacer("_", "-", " ", "-").Replace(s)
}

func linkTargets(body string) []string {
	var out []string
	for _, m := range wikiRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	for _, m := range mdLinkRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// memoryGraph reads every note across all target dirs and returns them with
// frontmatter metadata, resolved outbound links, and computed backlinks — the
// shape the dashboard tree/search/backlinks render from. Links resolve across
// targets (cross-repo references). Returns an empty (non-nil) slice when the
// store is empty/missing.
func memoryGraph(cfg *Config) (nodes []MemoryNode, dir string, err error) {
	vm, base := memoryStore(cfg)
	if base == "" {
		return []MemoryNode{}, "", nil
	}
	files, lerr := listStoreNotes(vm, base)
	if lerr != nil {
		if os.IsNotExist(lerr) {
			return []MemoryNode{}, base, nil
		}
		return nil, base, lerr
	}
	sort.Strings(files)

	rawLinks := map[string][]string{} // rel -> raw targets
	bySlug := map[string]string{}     // normalised name/stem -> rel (cross-target)
	nodes = make([]MemoryNode, 0, len(files))
	for _, rel := range files {
		b, rerr := readStoreFile(vm, base, rel)
		if rerr != nil {
			continue
		}
		fm, body := splitFrontmatter(string(b))
		target := path.Dir(rel) // <owner>/<repo>; "." for top-level (shared)
		if target == "." {
			target = ""
		}
		n := MemoryNode{File: rel, Target: target, Name: strings.TrimSuffix(path.Base(rel), ".md")}
		if m := fmNameRe.FindStringSubmatch(fm); m != nil {
			n.Name = m[1]
		}
		if m := fmDescRe.FindStringSubmatch(fm); m != nil && m[1] != "|" {
			n.Summary = m[1]
		} else {
			n.Summary = firstParagraph(body)
		}
		rawLinks[rel] = linkTargets(body)
		bySlug[normSlug(n.Name)] = rel
		bySlug[normSlug(rel)] = rel
		nodes = append(nodes, n)
	}

	backlinks := map[string]map[string]bool{}
	for i := range nodes {
		rel := nodes[i].File
		seen := map[string]bool{}
		for _, raw := range rawLinks[rel] {
			tgt, ok := bySlug[normSlug(raw)]
			if !ok || tgt == rel || seen[tgt] {
				continue
			}
			seen[tgt] = true
			nodes[i].Links = append(nodes[i].Links, tgt)
			if backlinks[tgt] == nil {
				backlinks[tgt] = map[string]bool{}
			}
			backlinks[tgt][rel] = true
		}
	}
	for i := range nodes {
		for src := range backlinks[nodes[i].File] {
			nodes[i].Backlinks = append(nodes[i].Backlinks, src)
		}
		sort.Strings(nodes[i].Backlinks)
	}
	return nodes, base, nil
}

// firstParagraph returns the first non-empty, non-heading line of a note body,
// used as a summary when frontmatter has no inline description.
func firstParagraph(body string) string {
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "---") {
			continue
		}
		if len(t) > 200 {
			t = t[:200] + "…"
		}
		return t
	}
	return ""
}
