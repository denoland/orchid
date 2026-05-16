package main

import (
	"strings"
	"sync"
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

func TestResolveIncludeAPI(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		ref     string
		inbox   string
		want    string
		wantErr bool
	}{
		{
			name:  "relative prompt",
			kind:  "prompt",
			ref:   "review-pr.md",
			inbox: "bartlomieju/agent-job-board",
			want:  "repos/bartlomieju/agent-job-board/contents/prompts/review-pr.md",
		},
		{
			name:  "relative skill",
			kind:  "skill",
			ref:   "lint.md",
			inbox: "bartlomieju/agent-job-board",
			want:  "repos/bartlomieju/agent-job-board/contents/skills/lint.md",
		},
		{
			name:  "nested relative path",
			kind:  "prompt",
			ref:   "shared/style.md",
			inbox: "bartlomieju/agent-job-board",
			want:  "repos/bartlomieju/agent-job-board/contents/prompts/shared/style.md",
		},
		{
			name:  "absolute github URL on main",
			kind:  "skill",
			ref:   "https://github.com/denoland/deno/blob/main/skills/triage.md",
			inbox: "bartlomieju/agent-job-board",
			want:  "repos/denoland/deno/contents/skills/triage.md?ref=main",
		},
		{
			name:  "absolute URL with tag ref",
			kind:  "prompt",
			ref:   "https://github.com/owner/repo/blob/v1.2.3/prompts/x.md",
			inbox: "ignored",
			want:  "repos/owner/repo/contents/prompts/x.md?ref=v1.2.3",
		},
		{
			name:    "URL missing /blob/ segment",
			kind:    "prompt",
			ref:     "https://github.com/owner/repo/main/file.md",
			inbox:   "ignored",
			wantErr: true,
		},
		{
			name:    "URL too short",
			kind:    "prompt",
			ref:     "https://github.com/owner/repo",
			inbox:   "ignored",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveIncludeAPI(tc.kind, tc.ref, tc.inbox)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestKillBudget(t *testing.T) {
	t.Run("max=2 allows two kills then refuses", func(t *testing.T) {
		b := killBudget{max: 2}
		if !b.tryUse() {
			t.Fatal("first tryUse should succeed")
		}
		if !b.tryUse() {
			t.Fatal("second tryUse should succeed")
		}
		if b.tryUse() {
			t.Fatal("third tryUse should be refused (budget exhausted)")
		}
		if b.tryUse() {
			t.Fatal("fourth tryUse should still be refused")
		}
		if b.used != 2 {
			t.Errorf("used: got %d, want 2 (refused calls must not increment)", b.used)
		}
	})

	t.Run("staggers a herd across ticks", func(t *testing.T) {
		// Simulate 6 simultaneously dead sessions across successive ticks.
		// With maxKillsPerTick=2, we expect 3 ticks to drain the herd.
		dead := 6
		ticks := 0
		for dead > 0 {
			b := killBudget{max: maxKillsPerTick}
			killedThisTick := 0
			for dead > 0 && b.tryUse() {
				dead--
				killedThisTick++
			}
			if killedThisTick > maxKillsPerTick {
				t.Fatalf("tick %d killed %d > cap %d", ticks, killedThisTick, maxKillsPerTick)
			}
			ticks++
		}
		if want := 3; ticks != want {
			t.Errorf("ticks to drain 6 dead with cap %d: got %d, want %d", maxKillsPerTick, ticks, want)
		}
	})

	t.Run("max=0 refuses everything", func(t *testing.T) {
		b := killBudget{max: 0}
		if b.tryUse() {
			t.Fatal("max=0 must refuse all calls")
		}
	})
}

// TestTmuxPasteBufUnique exercises the regression behind denoland/orchid#101:
// when two concurrent goroutines were bootstrapping codex sessions they
// raced on the shared "orch" tmux buffer, causing each session to receive
// both prompts. tmuxPasteBuf must hand out a fresh name for every call so
// load-buffer can't stomp another in-flight paste.
func TestTmuxPasteBufUnique(t *testing.T) {
	t.Run("sequential calls produce distinct names", func(t *testing.T) {
		seen := map[string]bool{}
		for i := 0; i < 1000; i++ {
			b := tmuxPasteBuf()
			if !strings.HasPrefix(b, "orch-") {
				t.Fatalf("buffer name %q missing orch- prefix", b)
			}
			if seen[b] {
				t.Fatalf("duplicate buffer name %q at iteration %d", b, i)
			}
			seen[b] = true
		}
	})

	t.Run("concurrent callers do not collide", func(t *testing.T) {
		const workers = 16
		const perWorker = 100
		var mu sync.Mutex
		seen := map[string]bool{}
		var wg sync.WaitGroup
		dup := ""
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perWorker; i++ {
					b := tmuxPasteBuf()
					mu.Lock()
					if seen[b] && dup == "" {
						dup = b
					}
					seen[b] = true
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if dup != "" {
			t.Fatalf("concurrent tmuxPasteBuf collided on %q", dup)
		}
		if got := len(seen); got != workers*perWorker {
			t.Fatalf("unique names: got %d, want %d", got, workers*perWorker)
		}
	})
}

func TestIncludePatternMatches(t *testing.T) {
	body := `Plain text.

[prompt:review-pr.md]

Some other text. [skill:lint.md] inline.

A URL form: [prompt:https://github.com/denoland/deno/blob/main/skills/triage.md]

Not a match: [other:foo.md] or [prompt foo.md] or text [prompt:].
`
	matches := includePattern.FindAllStringSubmatch(body, -1)
	want := [][2]string{
		{"prompt", "review-pr.md"},
		{"skill", "lint.md"},
		{"prompt", "https://github.com/denoland/deno/blob/main/skills/triage.md"},
	}
	if len(matches) != len(want) {
		t.Fatalf("got %d matches, want %d: %+v", len(matches), len(want), matches)
	}
	for i, m := range matches {
		if m[1] != want[i][0] || m[2] != want[i][1] {
			t.Errorf("match %d: got (%q, %q), want (%q, %q)", i, m[1], m[2], want[i][0], want[i][1])
		}
	}
}
