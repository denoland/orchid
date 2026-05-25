package main

import (
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
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

func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
		why  string
	}{
		{"127.0.0.1", true, "loopback"},
		{"::1", true, "loopback v6"},
		{"10.0.0.1", true, "rfc1918"},
		{"172.16.0.1", true, "rfc1918"},
		{"192.168.1.1", true, "rfc1918"},
		{"169.254.169.254", true, "link-local / cloud IMDS"},
		{"100.64.0.1", true, "carrier-grade NAT"},
		{"100.127.255.254", true, "carrier-grade NAT upper"},
		{"0.0.0.0", true, "unspecified"},
		{"255.255.255.255", true, "broadcast"},
		{"224.0.0.1", true, "multicast"},
		{"fe80::1", true, "link-local v6"},
		{"ff02::1", true, "multicast v6"},
		{"8.8.8.8", false, "public dns"},
		{"1.1.1.1", false, "public dns"},
		{"100.63.255.255", false, "just below CGNAT range"},
		{"100.128.0.0", false, "just above CGNAT range"},
	}
	for _, tc := range cases {
		t.Run(tc.ip+"/"+tc.why, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("ParseIP(%q) returned nil", tc.ip)
			}
			got := isPrivateIP(ip)
			if got != tc.want {
				t.Errorf("isPrivateIP(%q): got %v, want %v (%s)", tc.ip, got, tc.want, tc.why)
			}
		})
	}
}

func TestIsPrivateHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", true},
		{"localhost", true},
		{"127.0.0.1", true},
		{"169.254.169.254", true},
		{"10.0.0.1", true},
		// public DNS resolution would be flaky in a sandbox, so the
		// non-private side of the table is covered indirectly by
		// TestIsPrivateIP. We assert the bypass attempts we know don't
		// need DNS.
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			got := isPrivateHost(tc.host)
			if got != tc.want {
				t.Errorf("isPrivateHost(%q): got %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

// TestResolveDraftTargetEnforcesAllowList pins down the rule that a Draft
// can only target the configured default repo OR a repo explicitly listed
// in capture.allowed_repos. Any other request-supplied repo is rejected
// so a leaked capture token can't be turned into "spam any repo this PAT
// can write to".
func TestResolveDraftTargetEnforcesAllowList(t *testing.T) {
	cfg := &Config{
		GitHub: GitHubBlock{InboxRepo: "owner/inbox"},
		Orch: OrchBlock{
			Capture: &CaptureBlock{
				DefaultRepo:  "owner/default",
				AllowedRepos: []string{"owner/extra"},
			},
		},
	}

	t.Run("no target uses default", func(t *testing.T) {
		repo, _, err := resolveDraftTarget(cfg, &DraftPayload{})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if repo != "owner/default" {
			t.Errorf("repo: got %q, want %q", repo, "owner/default")
		}
	})

	t.Run("explicit allow-listed target", func(t *testing.T) {
		repo, _, err := resolveDraftTarget(cfg, &DraftPayload{Target: &DraftTarget{Repo: "owner/extra"}})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if repo != "owner/extra" {
			t.Errorf("repo: got %q, want %q", repo, "owner/extra")
		}
	})

	t.Run("explicit default still allowed", func(t *testing.T) {
		_, _, err := resolveDraftTarget(cfg, &DraftPayload{Target: &DraftTarget{Repo: "owner/default"}})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	t.Run("rejects unlisted repo", func(t *testing.T) {
		_, _, err := resolveDraftTarget(cfg, &DraftPayload{Target: &DraftTarget{Repo: "attacker/private"}})
		if err == nil {
			t.Fatal("expected error for unlisted repo, got nil")
		}
	})

	t.Run("strips flag-shaped labels", func(t *testing.T) {
		_, labels, err := resolveDraftTarget(cfg, &DraftPayload{Target: &DraftTarget{
			Repo:   "owner/extra",
			Labels: []string{"good", "--repo=attacker/repo", "-x", " "},
		}})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		if len(labels) != 1 || labels[0] != "good" {
			t.Errorf("labels: got %+v, want [good]", labels)
		}
	})
}

// TestValidRepoRejectsArgvInjection ensures the regex used to gate
// `gh issue create --repo <repo>` doesn't admit values that could break
// out into an extra argv element or carry shell metacharacters.
func TestValidRepoRejectsArgvInjection(t *testing.T) {
	good := []string{"denoland/orchid", "user/repo-name", "x.y/z_1", "ORG/REPO"}
	for _, r := range good {
		if !validRepo.MatchString(r) {
			t.Errorf("validRepo rejected good value %q", r)
		}
	}
	bad := []string{
		"", "noslash", "/leading", "trailing/", "owner//repo",
		"owner/repo extra", "owner/repo;rm -rf /", "owner/repo\nattack",
		"-foo/repo", "owner/--repo", "owner/repo/extra",
		"a b/c", "owner/repo\x00x",
	}
	for _, r := range bad {
		if validRepo.MatchString(r) {
			t.Errorf("validRepo accepted bad value %q", r)
		}
	}
}

func TestSlugifyHostname(t *testing.T) {
	cases := []struct{ in, want string }{
		{"worker01.dc.example.com", "worker01"},
		{"127.0.0.1", "vm-127"},
		{"hyphen-host", "hyphen-host"},
		{"UPPER", "upper"},
		{"with:port:1234", "with"},
		{"  ", ""},
		{"!!!", ""},
		{"1234", "vm-1234"},
	}
	for _, tc := range cases {
		got := slugifyHostname(tc.in)
		if got != tc.want {
			t.Errorf("slugifyHostname(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAllocVMName(t *testing.T) {
	cfg := &Config{VMs: []VMBlock{{Name: "local"}, {Name: "worker01"}, {Name: "vm-1"}}}
	if got := allocVMName(cfg, "worker01.dc.example.com"); got != "vm-2" {
		t.Errorf("allocVMName when hostname collides: got %q, want vm-2 (vm-1 already taken)", got)
	}
	if got := allocVMName(cfg, "worker02"); got != "worker02" {
		t.Errorf("allocVMName fresh hostname: got %q, want worker02", got)
	}
	if got := allocVMName(cfg, ""); got != "vm-2" {
		t.Errorf("allocVMName no hostname falls back to vm-N: got %q, want vm-2", got)
	}
}

func TestValidVMName(t *testing.T) {
	good := []string{"local", "vm-1", "worker_01", "a", "A1"}
	bad := []string{"", "-leading", "_leading", "has space", "x.y", "../etc/passwd", strings.Repeat("a", 64)}
	for _, n := range good {
		if !validVMName(n) {
			t.Errorf("validVMName(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if validVMName(n) {
			t.Errorf("validVMName(%q) = true, want false", n)
		}
	}
}

func TestAppendAuthorizedKeyIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/authorized_keys"
	// Pre-existing other entries must survive.
	if err := os.WriteFile(path, []byte("ssh-rsa AAA existing-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const marker = "# orchid-central:worker01"
	key1 := "ssh-ed25519 AAAFIRST orchid"
	key2 := "ssh-ed25519 AAASECOND orchid"
	for i := 0; i < 3; i++ {
		if err := appendAuthorizedKey(path, marker, key1); err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
	}
	b, _ := os.ReadFile(path)
	if got := strings.Count(string(b), marker); got != 1 {
		t.Errorf("after 3 writes of same key, marker appears %d times, want 1\n%s", got, b)
	}
	if got := strings.Count(string(b), key1); got != 1 {
		t.Errorf("key1 appears %d times, want 1", got)
	}
	if !strings.Contains(string(b), "ssh-rsa AAA existing-user") {
		t.Errorf("pre-existing entry was lost:\n%s", b)
	}
	// Rotation: same marker, different key replaces the old line.
	if err := appendAuthorizedKey(path, marker, key2); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if strings.Contains(string(b), "AAAFIRST") {
		t.Errorf("rotation kept stale key:\n%s", b)
	}
	if !strings.Contains(string(b), key2) {
		t.Errorf("rotation missing new key:\n%s", b)
	}
}

func TestStripMarkerBlock(t *testing.T) {
	src := []byte("a line\n# m\nsecond line\nthird line\n")
	got := string(stripMarkerBlock(src, "# m"))
	want := "a line\nthird line\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Marker present but no line follows.
	src2 := []byte("a\n# m")
	got2 := string(stripMarkerBlock(src2, "# m"))
	if got2 != "a" {
		t.Errorf("trailing-marker case: got %q, want %q", got2, "a")
	}
	// Empty input.
	if got := string(stripMarkerBlock(nil, "# m")); got != "" {
		t.Errorf("nil input: got %q, want empty", got)
	}
}

func TestPatchHCLAddsVMBlock(t *testing.T) {
	src := []byte(`github {
  inbox_repo = "denoland/orchid"
}

orchestrator {
  poll_interval = "30s"
  state_file    = "/root/orch/state.json"
  branch_prefix = "orch/"
  workdir_root  = "/home/orchid/orch-work"
}

# Local VM (untouched)
vm "local" {
  host = "localhost"
  user = "orchid"
}

bootstrap_prompt = ""
`)
	patch := map[string]map[string]any{
		"vm.worker01": {
			"host":         "10.0.0.5",
			"user":         "orchid",
			"key":          "/root/orch/vm-keys/worker01",
			"capacity":     float64(4),
			"join_managed": true,
		},
	}
	out, err := patchHCL(src, patch)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, `vm "worker01"`) {
		t.Errorf("missing new vm block:\n%s", s)
	}
	// hclwrite aligns the `=` columns, so match on the value only.
	if !strings.Contains(s, `"10.0.0.5"`) {
		t.Errorf("missing host value:\n%s", s)
	}
	if !strings.Contains(s, `join_managed`) || !strings.Contains(s, `true`) {
		t.Errorf("missing join_managed attr:\n%s", s)
	}
	// Untouched local block + comment survive.
	if !strings.Contains(s, `vm "local"`) {
		t.Errorf("local vm was removed:\n%s", s)
	}
	if !strings.Contains(s, `# Local VM (untouched)`) {
		t.Errorf("comment was stripped:\n%s", s)
	}
	// HCL still parses.
	tmp := t.TempDir() + "/swarm.hcl"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		t.Fatal(err)
	}
	var trial Config
	if err := hclsimple.DecodeFile(tmp, nil, &trial); err != nil {
		t.Fatalf("patched hcl fails to decode: %v\n%s", err, s)
	}
	var found bool
	for _, v := range trial.VMs {
		if v.Name == "worker01" {
			found = true
			if v.Host != "10.0.0.5" || v.Key != "/root/orch/vm-keys/worker01" || !v.JoinManaged {
				t.Errorf("decoded block wrong: %+v", v)
			}
		}
	}
	if !found {
		t.Errorf("decoded config missing worker01")
	}
}

// TestMergeableTransition pins down the rules for when a mergeable state
// change is worth poking the worker. UNKNOWN is GitHub's "still computing"
// state and must never trigger notifications — otherwise we'd spam the
// worker on every PR open while GitHub catches up. The first observation
// of MERGEABLE is a silent baseline so we don't re-notify after restart.
// The first observation of CONFLICTING is loud — we may have just attached
// to a PR that became conflicted before orch was watching.
func TestMergeableTransition(t *testing.T) {
	cases := []struct {
		name string
		prev string
		cur  string
		want string
	}{
		{"first MERGEABLE is silent baseline", "", "MERGEABLE", ""},
		{"first CONFLICTING is loud", "", "CONFLICTING", "CONFLICTING"},
		{"first UNKNOWN never notifies", "", "UNKNOWN", ""},
		{"empty current never notifies", "MERGEABLE", "", ""},
		{"current UNKNOWN never notifies", "MERGEABLE", "UNKNOWN", ""},
		{"current UNKNOWN from CONFLICTING is silent", "CONFLICTING", "UNKNOWN", ""},
		{"MERGEABLE → CONFLICTING (base moved)", "MERGEABLE", "CONFLICTING", "CONFLICTING"},
		{"CONFLICTING → MERGEABLE (resolved)", "CONFLICTING", "MERGEABLE", "MERGEABLE"},
		{"no change MERGEABLE", "MERGEABLE", "MERGEABLE", ""},
		{"no change CONFLICTING (no re-spam)", "CONFLICTING", "CONFLICTING", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeableTransition(tc.prev, tc.cur)
			if got != tc.want {
				t.Errorf("mergeableTransition(%q,%q) = %q, want %q", tc.prev, tc.cur, got, tc.want)
			}
		})
	}
}

// TestSummarizeMergeable verifies the worker-facing message embeds clear
// resolution instructions when a PR is conflicting, and stays quiet when
// nothing changed. The worker has no other UI — this string is the entire
// signal — so the exact phrasing matters.
func TestSummarizeMergeable(t *testing.T) {
	v := &PRView{}

	t.Run("conflicting includes git rebase guidance", func(t *testing.T) {
		out := summarize(v, nil, nil, nil, false, nil, "CONFLICTING")
		for _, want := range []string{
			"CONFLICTS with the base branch",
			"git fetch origin",
			"git rebase origin/<base>",
			"git push --force-with-lease",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("summarize CONFLICTING missing %q in:\n%s", want, out)
			}
		}
	})

	t.Run("mergeable confirms resolution", func(t *testing.T) {
		out := summarize(v, nil, nil, nil, false, nil, "MERGEABLE")
		if !strings.Contains(out, "conflicts resolved") {
			t.Errorf("summarize MERGEABLE missing resolution note:\n%s", out)
		}
	})

	t.Run("empty mergeable: no conflict copy at all", func(t *testing.T) {
		out := summarize(v, nil, nil, nil, false, nil, "")
		if strings.Contains(out, "CONFLICT") || strings.Contains(out, "conflicts resolved") {
			t.Errorf("summarize without mergeable transition leaked conflict copy:\n%s", out)
		}
	})
}

// TestDiffPRMergeable exercises the full diffPR seam: it must thread the
// mergeable transition out alongside the existing review/comment/check
// diffs, using the stored LastMergeable on the Job as the previous state.
func TestDiffPRMergeable(t *testing.T) {
	t.Run("MERGEABLE → CONFLICTING surfaced", func(t *testing.T) {
		j := &Job{LastMergeable: "MERGEABLE"}
		v := &PRView{Mergeable: "CONFLICTING"}
		_, _, _, _, _, _, _, _, m := diffPR(j, v, "")
		if m != "CONFLICTING" {
			t.Errorf("got mergeable=%q, want CONFLICTING", m)
		}
	})
	t.Run("UNKNOWN never bubbles up", func(t *testing.T) {
		j := &Job{LastMergeable: "MERGEABLE"}
		v := &PRView{Mergeable: "UNKNOWN"}
		_, _, _, _, _, _, _, _, m := diffPR(j, v, "")
		if m != "" {
			t.Errorf("got mergeable=%q, want \"\" (UNKNOWN is transient)", m)
		}
	})
	t.Run("stable MERGEABLE is silent", func(t *testing.T) {
		j := &Job{LastMergeable: "MERGEABLE"}
		v := &PRView{Mergeable: "MERGEABLE"}
		_, _, _, _, _, _, _, _, m := diffPR(j, v, "")
		if m != "" {
			t.Errorf("got mergeable=%q, want \"\" (no transition)", m)
		}
	})
}

// TestIsActionableCheck pins down which CI conclusions are worth waking
// the worker for. The whole point of denoland/orchid#224 is that a green
// check is not an interrupt-worthy event — the worker has nothing to do
// about it, and the round-trip ("nothing to address, stopping") burns a
// session turn. Unknown conclusions are deliberately treated as
// actionable: better to over-notify on a new GitHub value than swallow
// what might be a real failure mode.
func TestIsActionableCheck(t *testing.T) {
	silent := []string{"SUCCESS", "NEUTRAL", "SKIPPED", "STALE"}
	actionable := []string{
		"FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED",
		"STARTUP_FAILURE",
		"", // empty conclusion shouldn't reach here, but if it does, surface it
		"SOMETHING_NEW", // forward-compat: unknown values are loud, not silent
	}
	for _, c := range silent {
		if isActionableCheck(c) {
			t.Errorf("isActionableCheck(%q) = true, want false (no fix for the worker to push)", c)
		}
	}
	for _, c := range actionable {
		if !isActionableCheck(c) {
			t.Errorf("isActionableCheck(%q) = false, want true (worker may need to react)", c)
		}
	}
}

// TestDiffPRChecksFilterSuccess locks in the wake-up gate: a tick in
// which every CI transition is green must not register as a check change
// at all, so the poll loop's "no diff → silent" branch takes effect and
// the worker isn't poked.
func TestDiffPRChecksFilterSuccess(t *testing.T) {
	t.Run("all-SUCCESS tick is silent", func(t *testing.T) {
		j := &Job{LastCheckConclusions: map[string]string{}}
		v := &PRView{StatusCheckRollup: []StatusCheck{
			{Name: "test linux", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-05-25T00:00:00Z"},
			{Name: "test mac", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-05-25T00:00:00Z"},
			{Name: "lint", Status: "COMPLETED", Conclusion: "NEUTRAL", CompletedAt: "2026-05-25T00:00:00Z"},
			{Name: "docs only", Status: "COMPLETED", Conclusion: "SKIPPED", CompletedAt: "2026-05-25T00:00:00Z"},
		}}
		_, _, _, _, _, _, _, checks, _ := diffPR(j, v, "")
		if len(checks) != 0 {
			t.Errorf("all-green tick produced check changes, want none: %v", checks)
		}
	})

	t.Run("FAILURE is surfaced even when other checks are green", func(t *testing.T) {
		j := &Job{LastCheckConclusions: map[string]string{}}
		v := &PRView{StatusCheckRollup: []StatusCheck{
			{Name: "test linux", Status: "COMPLETED", Conclusion: "SUCCESS", CompletedAt: "2026-05-25T00:00:00Z"},
			{Name: "test mac", Status: "COMPLETED", Conclusion: "FAILURE", CompletedAt: "2026-05-25T00:00:00Z"},
		}}
		_, _, _, _, _, _, _, checks, _ := diffPR(j, v, "")
		if len(checks) != 1 {
			t.Fatalf("expected only the FAILURE to surface, got %d: %v", len(checks), checks)
		}
		if !strings.Contains(checks[0], "test mac") || !strings.Contains(checks[0], "FAILURE") {
			t.Errorf("surfaced wrong change: %q", checks[0])
		}
	})

	t.Run("regression detected even across silently-absorbed SUCCESS", func(t *testing.T) {
		// Prior state recorded the most recent persisted conclusion as
		// SUCCESS (e.g., the wake-path saved it on a previous tick that
		// fired for some other reason). Now the same check has flipped
		// red — we must surface it. This is the SUCCESS→FAILURE
		// regression case.
		j := &Job{LastCheckConclusions: map[string]string{"test linux": "SUCCESS"}}
		v := &PRView{StatusCheckRollup: []StatusCheck{
			{Name: "test linux", Status: "COMPLETED", Conclusion: "FAILURE", CompletedAt: "2026-05-25T00:01:00Z"},
		}}
		_, _, _, _, _, _, _, checks, _ := diffPR(j, v, "")
		if len(checks) != 1 || !strings.Contains(checks[0], "FAILURE") {
			t.Errorf("SUCCESS→FAILURE regression not surfaced: %v", checks)
		}
	})

	t.Run("in-progress checks are ignored", func(t *testing.T) {
		j := &Job{LastCheckConclusions: map[string]string{}}
		v := &PRView{StatusCheckRollup: []StatusCheck{
			{Name: "test linux", Status: "IN_PROGRESS", Conclusion: "", CompletedAt: ""},
		}}
		_, _, _, _, _, _, _, checks, _ := diffPR(j, v, "")
		if len(checks) != 0 {
			t.Errorf("in-progress check leaked into diff: %v", checks)
		}
	})
}

// TestDiffPRBotSelfFilter verifies the bot's own activity is partitioned
// into the silent buckets (still tracked for "seen" advancement, but
// never surfaced to the worker) while third-party activity flows through
// the visible buckets unchanged. Regression: orchid used to wake the
// worker about its own comments and commits.
func TestDiffPRBotSelfFilter(t *testing.T) {
	bot := "divybot"
	mkView := func() *PRView {
		v := &PRView{}
		v.Reviews = append(v.Reviews, struct {
			ID     string                 `json:"id"`
			Author struct{ Login string } `json:"author"`
			State  string                 `json:"state"`
			Body   string                 `json:"body"`
		}{ID: "rev-bot", Author: struct{ Login string }{Login: bot}, State: "COMMENTED", Body: "self"})
		v.Reviews = append(v.Reviews, struct {
			ID     string                 `json:"id"`
			Author struct{ Login string } `json:"author"`
			State  string                 `json:"state"`
			Body   string                 `json:"body"`
		}{ID: "rev-human", Author: struct{ Login string }{Login: "alice"}, State: "APPROVED", Body: "lgtm"})
		v.Comments = append(v.Comments, struct {
			ID     string                 `json:"id"`
			Author struct{ Login string } `json:"author"`
			Body   string                 `json:"body"`
		}{ID: "c-bot", Author: struct{ Login string }{Login: bot}, Body: "pushed follow-up"})
		v.Comments = append(v.Comments, struct {
			ID     string                 `json:"id"`
			Author struct{ Login string } `json:"author"`
			Body   string                 `json:"body"`
		}{ID: "c-human", Author: struct{ Login string }{Login: "bob"}, Body: "nit"})
		return v
	}

	t.Run("bot reviews/comments go silent, human ones visible", func(t *testing.T) {
		j := &Job{}
		v := mkView()
		vr, _, vi, sr, _, si, _, _, _ := diffPR(j, v, bot)
		if len(vr) != 1 || vr[0] != "rev-human" {
			t.Errorf("visibleReviews = %v, want [rev-human]", vr)
		}
		if len(sr) != 1 || sr[0] != "rev-bot" {
			t.Errorf("silentReviews = %v, want [rev-bot]", sr)
		}
		if len(vi) != 1 || vi[0] != "c-human" {
			t.Errorf("visibleIssue = %v, want [c-human]", vi)
		}
		if len(si) != 1 || si[0] != "c-bot" {
			t.Errorf("silentIssue = %v, want [c-bot]", si)
		}
	})

	t.Run("bot-authored HEAD commit suppresses pushed", func(t *testing.T) {
		j := &Job{LastHeadOID: "old-sha"}
		v := &PRView{HeadRefOid: "new-sha"}
		v.Commits = append(v.Commits, struct {
			Oid     string `json:"oid"`
			Authors []struct {
				Login string `json:"login"`
			} `json:"authors"`
		}{Oid: "new-sha", Authors: []struct {
			Login string `json:"login"`
		}{{Login: bot}}})
		_, _, _, _, _, _, pushed, _, _ := diffPR(j, v, bot)
		if pushed {
			t.Errorf("pushed = true for bot-authored HEAD, want false")
		}
	})

	t.Run("human HEAD commit still surfaces pushed", func(t *testing.T) {
		j := &Job{LastHeadOID: "old-sha"}
		v := &PRView{HeadRefOid: "new-sha"}
		v.Commits = append(v.Commits, struct {
			Oid     string `json:"oid"`
			Authors []struct {
				Login string `json:"login"`
			} `json:"authors"`
		}{Oid: "new-sha", Authors: []struct {
			Login string `json:"login"`
		}{{Login: "alice"}}})
		_, _, _, _, _, _, pushed, _, _ := diffPR(j, v, bot)
		if !pushed {
			t.Errorf("pushed = false for human-authored HEAD, want true")
		}
	})

	t.Run("co-authored bot+human HEAD still surfaces pushed", func(t *testing.T) {
		j := &Job{LastHeadOID: "old-sha"}
		v := &PRView{HeadRefOid: "new-sha"}
		v.Commits = append(v.Commits, struct {
			Oid     string `json:"oid"`
			Authors []struct {
				Login string `json:"login"`
			} `json:"authors"`
		}{Oid: "new-sha", Authors: []struct {
			Login string `json:"login"`
		}{{Login: bot}, {Login: "alice"}}})
		_, _, _, _, _, _, pushed, _, _ := diffPR(j, v, bot)
		if !pushed {
			t.Errorf("pushed = false for co-authored HEAD with a human, want true")
		}
	})

	t.Run("missing HEAD in commits list falls back to notify", func(t *testing.T) {
		j := &Job{LastHeadOID: "old-sha"}
		v := &PRView{HeadRefOid: "new-sha"} // no commits payload at all
		_, _, _, _, _, _, pushed, _, _ := diffPR(j, v, bot)
		if !pushed {
			t.Errorf("pushed = false when HEAD missing from commits, want true (conservative)")
		}
	})

	t.Run("empty botLogin disables filtering", func(t *testing.T) {
		j := &Job{}
		v := mkView()
		vr, _, vi, sr, _, si, _, _, _ := diffPR(j, v, "")
		if len(sr) != 0 || len(si) != 0 {
			t.Errorf("silent buckets should be empty when botLogin unset: sr=%v si=%v", sr, si)
		}
		if len(vr) != 2 || len(vi) != 2 {
			t.Errorf("all items should be visible when botLogin unset: vr=%v vi=%v", vr, vi)
		}
	})

	t.Run("seen IDs are not re-classified", func(t *testing.T) {
		j := &Job{SeenIssueCommentIDs: []string{"c-bot", "c-human"}}
		v := mkView()
		_, _, vi, _, _, si, _, _, _ := diffPR(j, v, bot)
		if len(vi) != 0 || len(si) != 0 {
			t.Errorf("seen comments should not appear in either bucket: vi=%v si=%v", vi, si)
		}
	})
}

func TestPanePrompted(t *testing.T) {
	claude := agentSpecs["claude"]
	codex := agentSpecs["codex"]

	cases := []struct {
		name string
		spec agentSpec
		pane string
		want bool
	}{
		{
			name: "claude permission dialog (issue #227 sample)",
			spec: claude,
			pane: ` Bash command

   rm tests/specs/check/brotli_compression_stream/*
   Clear test dir

 Dangerous rm operation on critical path: /home/orchid/orch-work/issue-220/tests/specs/check/brotli_compression_stream/*

 Do you want to proceed?
 ❯ 1. Yes
   2. No

 Esc to cancel · Tab to amend · ctrl+e to explain`,
			want: true,
		},
		{
			name: "claude plan-mode approval",
			spec: claude,
			pane: `Plan ready.

❯ 1. Yes, proceed
  2. No, keep refining

 Esc to cancel · Shift+Tab to switch modes`,
			want: true,
		},
		{
			name: "claude idle prompt (no dialog)",
			spec: claude,
			pane: `>

? for shortcuts                                bypass permissions on`,
			want: false,
		},
		{
			name: "claude busy (esc to interrupt — distinct from prompt footer)",
			spec: claude,
			pane: `✱ Thinking… (esc to interrupt)

>

? for shortcuts                                bypass permissions on`,
			want: false,
		},
		{
			name: "claude busy Bash tool: even if a later redraw flashed prompt-looking text, busyMarker wins",
			spec: claude,
			pane: `⏺ Bash
 ▶ Running rm Esc to cancel staging/foo

 (esc to interrupt)`,
			want: false,
		},
		{
			name: "codex with no prompt markers configured stays not-prompted",
			spec: codex,
			pane: `Codex still chewing… (esc to interrupt)

gpt-5.6 default · /workdir/issue-12`,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := panePrompted(tc.pane, tc.spec); got != tc.want {
				t.Errorf("panePrompted = %v, want %v\npane=%q", got, tc.want, tc.pane)
			}
		})
	}
}

func TestPaneNeedsInputTransitions(t *testing.T) {
	// Same tmux name across calls — paneNeedsInputSet must report a
	// transition only when the prompted state actually flips.
	const k = "claude-test-pneeds-1"
	t.Cleanup(func() { paneNeedsInputSet(k, false) })

	if changed := paneNeedsInputSet(k, false); changed {
		t.Fatalf("first call with needs=false should not report a change")
	}
	if changed := paneNeedsInputSet(k, true); !changed {
		t.Fatalf("false→true should report a change")
	}
	if !paneNeedsInputSnapshot(k) {
		t.Fatalf("snapshot should be true after needs=true")
	}
	if changed := paneNeedsInputSet(k, true); changed {
		t.Fatalf("true→true should not report a change")
	}
	if changed := paneNeedsInputSet(k, false); !changed {
		t.Fatalf("true→false should report a change")
	}
	if paneNeedsInputSnapshot(k) {
		t.Fatalf("snapshot should be false after needs=false")
	}

	// Prune drops sessions absent from the live set.
	paneNeedsInputSet(k, true)
	paneNeedsInputPrune(map[string]bool{}) // empty live set
	if paneNeedsInputSnapshot(k) {
		t.Fatalf("prune should have evicted absent session")
	}
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
