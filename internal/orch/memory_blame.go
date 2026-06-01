package orch

import (
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// BlameLine is one line of a memory note with the commit that introduced it —
// the provenance anchor. The commit links back to the agent session or human
// that wrote the fact (answers "why does it believe this?").
type BlameLine struct {
	N       int    `json:"n"`
	Text    string `json:"text"`
	Commit  string `json:"commit"`
	Short   string `json:"short"`
	Author  string `json:"author"`
	Email   string `json:"email"`
	Date    string `json:"date"` // YYYY-MM-DD (author time, UTC)
	Summary string `json:"summary"`
}

// gitInStore runs a git subcommand in the memory repo clone. Local store (the
// central box) runs it directly; a remote fallback store goes over ssh.
func gitInStore(vm *VMBlock, root string, args ...string) (string, string, error) {
	full := append([]string{"-C", root}, args...)
	if vm == nil || isLocal(*vm) {
		return run("git", full...)
	}
	quoted := make([]string, len(full))
	for i, a := range full {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	return sshExec(*vm, "git "+strings.Join(quoted, " "))
}

// memoryBlame returns per-line git-blame provenance for a memory note, plus the
// repo (owner/repo) so the dashboard can link each commit on GitHub.
func memoryBlame(cfg *Config, rel string) ([]BlameLine, string, error) {
	if !validRel(rel) {
		return nil, "", fmt.Errorf("invalid memory path")
	}
	vm, base := memoryStore(cfg)
	if base == "" {
		return nil, "", fmt.Errorf("no memory store")
	}
	root := filepath.Dir(base) // base = <repo clone>/<memDir>; root = the clone
	fileInRepo := path.Join(memDir(cfg), rel)
	out, errStr, err := gitInStore(vm, root, "blame", "--line-porcelain", "--", fileInRepo)
	if err != nil {
		return nil, "", fmt.Errorf("git blame %s: %v: %s", rel, err, strings.TrimSpace(errStr))
	}
	return parseBlamePorcelain(out), memRepo(cfg), nil
}

// LogEntry is one commit touching a note — the cgit-style log view.
type LogEntry struct {
	Commit  string `json:"commit"`
	Short   string `json:"short"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

// memoryLog returns the commit history of a memory note (git log --follow).
func memoryLog(cfg *Config, rel string) ([]LogEntry, string, error) {
	if !validRel(rel) {
		return nil, "", fmt.Errorf("invalid memory path")
	}
	vm, base := memoryStore(cfg)
	if base == "" {
		return nil, "", fmt.Errorf("no memory store")
	}
	root := filepath.Dir(base)
	fileInRepo := path.Join(memDir(cfg), rel)
	out, errStr, err := gitInStore(vm, root, "log", "--follow", "--date=short",
		"--format=%H%x09%h%x09%an%x09%ad%x09%s", "--", fileInRepo)
	if err != nil {
		return nil, "", fmt.Errorf("git log %s: %v: %s", rel, err, strings.TrimSpace(errStr))
	}
	var out2 []LogEntry
	for _, line := range strings.Split(out, "\n") {
		f := strings.SplitN(line, "\t", 5)
		if len(f) < 5 {
			continue
		}
		out2 = append(out2, LogEntry{Commit: f[0], Short: f[1], Author: f[2], Date: f[3], Subject: f[4]})
	}
	return out2, memRepo(cfg), nil
}

// parseBlamePorcelain turns `git blame --line-porcelain` output into BlameLines.
// Each line is a header block (sha + author/summary fields) followed by a
// tab-prefixed content line.
func parseBlamePorcelain(out string) []BlameLine {
	var lines []BlameLine
	var cur BlameLine
	n := 0
	for _, raw := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(raw, "\t"):
			n++
			cur.N = n
			cur.Text = raw[1:]
			lines = append(lines, cur)
			cur = BlameLine{}
		case raw == "":
			// blank — ignore
		default:
			f := strings.SplitN(raw, " ", 2)
			head := f[0]
			rest := ""
			if len(f) > 1 {
				rest = f[1]
			}
			switch head {
			case "author":
				cur.Author = rest
			case "author-mail":
				cur.Email = strings.Trim(rest, "<>")
			case "author-time":
				if ts, e := strconv.ParseInt(rest, 10, 64); e == nil {
					cur.Date = time.Unix(ts, 0).UTC().Format("2006-01-02")
				}
			case "summary":
				cur.Summary = rest
			case "filename", "previous", "boundary", "committer", "committer-mail",
				"committer-time", "committer-tz", "author-tz":
				// ignored header fields
			default:
				// a 40-hex sha line: "<sha> <orig> <final> [count]"
				if len(head) == 40 && isHex(head) {
					cur.Commit = head
					cur.Short = head[:7]
				}
			}
		}
	}
	return lines
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
