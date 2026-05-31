package orch

import "testing"

func TestClaudeFlagsFromFrontmatter(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"none", "just a plain issue body", ""},
		{"model+effort", "```toml\npriority = 5\nmodel = \"sonnet\"\neffort = \"medium\"\n```\n\nBody text", " --model sonnet --effort medium"},
		{"model only", "```toml\nmodel = \"opus\"\n```", " --model opus"},
		{"effort only unquoted", "```toml\neffort = high\n```", " --effort high"},
		{"full model name", "```toml\nmodel = \"claude-sonnet-4-6\"\n```", " --model claude-sonnet-4-6"},
		{"invalid model dropped", "```toml\nmodel = \"gpt-4\"\n```", ""},
		{"invalid effort dropped", "```toml\neffort = \"turbo\"\n```", ""},
		{"case insensitive", "```toml\nMODEL = \"Sonnet\"\nEFFORT = \"MEDIUM\"\n```", " --model sonnet --effort medium"},
		{"no toml block ignored", "model = sonnet\neffort = medium", ""},
		{"leading blank lines", "\n\n```toml\nmodel = \"haiku\"\n```", " --model haiku"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := claudeFlagsFromFrontmatter(c.body); got != c.want {
				t.Errorf("claudeFlagsFromFrontmatter()\n got=%q\nwant=%q", got, c.want)
			}
		})
	}
}
