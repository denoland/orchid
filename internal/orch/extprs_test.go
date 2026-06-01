package orch

import "testing"

func TestExtractExternalRefs(t *testing.T) {
	skip := map[string]bool{"denoland/deno": true, "denoland/divybot": true}
	body := `This fixes the formatting bug.

Depends on the upstream change: https://github.com/dprint/dprint/pull/1137
Also see denoland/deno#34476 (the main PR) and dprint/dprint-plugin-typescript#42.
Closes denoland/divybot#9.`
	got := extractExternalRefs(skip, body)

	want := map[string]int{
		"dprint/dprint":                   1137,
		"dprint/dprint-plugin-typescript": 42,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d refs %+v, want %d", len(got), got, len(want))
	}
	for _, e := range got {
		if want[e.Repo] != e.Number {
			t.Errorf("unexpected ref %s#%d", e.Repo, e.Number)
		}
		if e.Repo == "denoland/deno" || e.Repo == "denoland/divybot" {
			t.Errorf("should have skipped target/inbox repo %s", e.Repo)
		}
	}
}

func TestExtractExternalRefsDedup(t *testing.T) {
	got := extractExternalRefs(nil,
		"see https://github.com/a/b/pull/5 and a/b#5 again")
	if len(got) != 1 || got[0].Repo != "a/b" || got[0].Number != 5 {
		t.Fatalf("expected one deduped a/b#5, got %+v", got)
	}
}

func TestDiscoverExtraPRsIdempotent(t *testing.T) {
	cfg := &Config{}
	cfg.GitHub.InboxRepo = "denoland/divybot"
	j := &Job{TargetRepo: "denoland/deno"}
	v := &PRView{Body: "upstream https://github.com/dprint/dprint/pull/1137"}
	discoverExtraPRs(j, v, cfg)
	discoverExtraPRs(j, v, cfg) // second pass must not duplicate
	if len(j.ExtraPRs) != 1 {
		t.Fatalf("want 1 ExtraPR, got %d: %+v", len(j.ExtraPRs), j.ExtraPRs)
	}
	if j.ExtraPRs[0].Repo != "dprint/dprint" || j.ExtraPRs[0].Number != 1137 {
		t.Fatalf("unexpected: %+v", j.ExtraPRs[0])
	}
}
