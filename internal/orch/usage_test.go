package orch

import "testing"

// Real token_count line shape captured from a codex session rollout
// (~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl). primary = 5h window,
// secondary = weekly window. Verifies ingestCodexRollout maps them onto the
// per-agent quota buckets the governor paces against.
func TestIngestCodexRollout(t *testing.T) {
	agentQuotaMu.Lock()
	delete(agentQuota, "codex")
	agentQuotaMu.Unlock()

	line := []byte(`{"timestamp":"2026-05-15T13:26:58.199Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1251265,"total_tokens":1258375}},"rate_limits":{"limit_id":"codex","primary":{"used_percent":12.5,"window_minutes":300,"resets_at":1778868492},"secondary":{"used_percent":6.0,"window_minutes":10080,"resets_at":1779165627},"credits":null,"plan_type":"prolite"}}}`)
	ingestCodexRollout(line, "codex")

	five, seven, ok := latestQuota("codex")
	if !ok {
		t.Fatal("expected a codex quota reading after ingest")
	}
	if five.UsedPct != 12.5 || five.ResetsAt != 1778868492 {
		t.Fatalf("5h bucket wrong: %+v", five)
	}
	if seven.UsedPct != 6.0 || seven.ResetsAt != 1779165627 {
		t.Fatalf("weekly bucket wrong: %+v", seven)
	}
	aq, _ := latestAgentQuota("codex")
	if aq.PlanType != "prolite" {
		t.Fatalf("plan_type = %q, want prolite", aq.PlanType)
	}

	// Non-token_count events must be ignored (no panic, no clobber).
	ingestCodexRollout([]byte(`{"type":"event_msg","payload":{"type":"agent_message","text":"hi"}}`), "codex")
	ingestCodexRollout([]byte(`not json`), "codex")
	if five2, _, ok := latestQuota("codex"); !ok || five2.UsedPct != 12.5 {
		t.Fatal("non-token_count event clobbered the codex reading")
	}
}

// A credit-plan reading (codex on $-credits rather than a subscription) should
// carry the credits balance through to the per-agent state for display.
func TestIngestCodexRolloutCredits(t *testing.T) {
	agentQuotaMu.Lock()
	delete(agentQuota, "codex")
	agentQuotaMu.Unlock()

	line := []byte(`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":0,"window_minutes":300,"resets_at":111},"secondary":{"used_percent":0,"window_minutes":10080,"resets_at":222},"credits":42.5,"plan_type":"credit"}}}`)
	ingestCodexRollout(line, "codex")
	aq, ok := latestAgentQuota("codex")
	if !ok || aq.Credits == nil || *aq.Credits != 42.5 {
		t.Fatalf("credits not carried through: %+v", aq)
	}
}

// Two codex accounts (e.g. a prolite plan and a $20 plan) on one host must
// meter into SEPARATE buckets keyed by account, never collide.
func TestCodexAccountsIsolated(t *testing.T) {
	agentQuotaMu.Lock()
	delete(agentQuota, "codex")
	delete(agentQuota, "codex-mini")
	agentQuotaMu.Unlock()

	pro := []byte(`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":50,"window_minutes":300,"resets_at":111},"secondary":{"used_percent":40,"window_minutes":10080,"resets_at":222},"plan_type":"prolite"}}}`)
	mini := []byte(`{"type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":3,"window_minutes":300,"resets_at":333},"secondary":{"used_percent":2,"window_minutes":10080,"resets_at":444},"plan_type":"plus"}}}`)
	ingestCodexRollout(pro, "codex")
	ingestCodexRollout(mini, "codex-mini")

	if f, _, ok := latestQuota("codex"); !ok || f.UsedPct != 50 {
		t.Fatalf("codex bucket wrong/clobbered: %+v", f)
	}
	if f, _, ok := latestQuota("codex-mini"); !ok || f.UsedPct != 3 {
		t.Fatalf("codex-mini bucket wrong/clobbered: %+v", f)
	}
	if a, _ := latestAgentQuota("codex"); a.PlanType != "prolite" {
		t.Fatalf("codex plan = %q", a.PlanType)
	}
	if a, _ := latestAgentQuota("codex-mini"); a.PlanType != "plus" {
		t.Fatalf("codex-mini plan = %q", a.PlanType)
	}
}

// vmAccount defaults to the agent name and honors an explicit account override.
func TestVMAccount(t *testing.T) {
	if got := vmAccount(VMBlock{Agent: "codex"}); got != "codex" {
		t.Fatalf("default account = %q, want codex", got)
	}
	if got := vmAccount(VMBlock{Agent: "codex", Account: "codex-mini"}); got != "codex-mini" {
		t.Fatalf("override account = %q, want codex-mini", got)
	}
	if got := vmAccount(VMBlock{}); got != "claude" {
		t.Fatalf("empty account = %q, want claude", got)
	}
}
