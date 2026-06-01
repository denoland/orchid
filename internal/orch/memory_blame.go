package orch

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

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

