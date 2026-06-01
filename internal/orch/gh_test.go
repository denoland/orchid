package orch

import "testing"

func TestCleanIssueTitle(t *testing.T) {
	tests := []struct{ in, want string }{
		{"[denoland/deno#29339] Deno LSP duplicate imports", "Deno LSP duplicate imports"},
		{"[denoland/orchid#350] something", "something"},
		{"fix(lsp): already clean", "fix(lsp): already clean"},
		{"no bracket here", "no bracket here"},
		{"[malformed without close", "[malformed without close"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := cleanIssueTitle(tc.in); got != tc.want {
			t.Errorf("cleanIssueTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
