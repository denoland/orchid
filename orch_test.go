package main

import (
	"testing"
	"time"
)

func TestParseCronFrontmatter(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantNil     bool
		wantSched   time.Duration
		wantSchedSt string
		wantTimeout time.Duration
	}{
		{
			name:        "issue-3 shape (timeout defaults to schedule/2)",
			body:        "```toml\ntype = \"cron\"\nschedule = \"30m\"\ntarget_repo = \"denoland/fresh\"\n```\n\n# Maintain denoland/fresh\nbody...",
			wantSched:   30 * time.Minute,
			wantSchedSt: "30m",
			wantTimeout: 15 * time.Minute,
		},
		{
			name:        "leading blank lines tolerated",
			body:        "\n\n```toml\nschedule = \"15s\"\n```\n",
			wantSched:   15 * time.Second,
			wantSchedSt: "15s",
			wantTimeout: 7500 * time.Millisecond,
		},
		{
			name:        "single-quoted value",
			body:        "```toml\nschedule = '2h'\n```",
			wantSched:   2 * time.Hour,
			wantSchedSt: "2h",
			wantTimeout: 1 * time.Hour,
		},
		{
			name:        "explicit timeout overrides default",
			body:        "```toml\nschedule = \"30m\"\ntimeout = \"90s\"\n```",
			wantSched:   30 * time.Minute,
			wantSchedSt: "30m",
			wantTimeout: 90 * time.Second,
		},
		{name: "no fence at top", body: "# Some heading\n```toml\nschedule = \"30m\"\n```", wantNil: true},
		{name: "fence but no schedule key", body: "```toml\ntype = \"cron\"\n```", wantNil: true},
		{name: "malformed duration", body: "```toml\nschedule = \"banana\"\n```", wantNil: true},
		{name: "empty body", body: "", wantNil: true},
		{name: "no fence at all", body: "schedule = \"30m\"", wantNil: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCronFrontmatter(tc.body)
			if tc.wantNil {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil CronConfig, got nil")
			}
			if got.Schedule != tc.wantSched || got.ScheduleStr != tc.wantSchedSt {
				t.Errorf("schedule: got (%v, %q), want (%v, %q)", got.Schedule, got.ScheduleStr, tc.wantSched, tc.wantSchedSt)
			}
			if got.Timeout != tc.wantTimeout {
				t.Errorf("timeout: got %v, want %v", got.Timeout, tc.wantTimeout)
			}
		})
	}
}

func TestIssueHasLabel(t *testing.T) {
	is := Issue{Labels: []string{"cron", "fresh"}}
	if !is.hasLabel("cron") {
		t.Error("expected hasLabel(cron) = true")
	}
	if !is.hasLabel("fresh") {
		t.Error("expected hasLabel(fresh) = true")
	}
	if is.hasLabel("deno") {
		t.Error("expected hasLabel(deno) = false")
	}
	if (Issue{}).hasLabel("anything") {
		t.Error("expected zero-value issue to have no labels")
	}
}
