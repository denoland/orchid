package main

import (
	"testing"
	"time"
)

// Real captured payload shapes from the live fleet (vultr).
const claudeLine = `{"model":{"display_name":"Opus 4.7"},"rate_limits":{"five_hour":{"used_percentage":8,"resets_at":1782711600},"seven_day":{"used_percentage":90,"resets_at":1782900000}}}`

const codexLine = `{"payload":{"type":"token_count","rate_limits":{"limit_id":"codex_bengalfox","primary":{"used_percent":0.0,"window_minutes":300,"resets_at":1782310969},"secondary":{"used_percent":3.0,"window_minutes":10080,"resets_at":1782368376}}}}`

func TestParseHostQuotaClaude(t *testing.T) {
	five, seven, mtime, ok := parseHostQuota("claude", "1782700000 "+claudeLine)
	if !ok {
		t.Fatal("claude: expected ok")
	}
	if mtime != 1782700000 {
		t.Fatalf("mtime = %d", mtime)
	}
	if five.UsedPct != 8 || five.ResetsAt != 1782711600 {
		t.Fatalf("five = %+v", five)
	}
	if seven.UsedPct != 90 || seven.ResetsAt != 1782900000 {
		t.Fatalf("seven = %+v", seven)
	}
}

func TestParseHostQuotaCodex(t *testing.T) {
	// primary (300min) => 5h; secondary (10080min) => weekly, regardless of order.
	five, seven, _, ok := parseHostQuota("codex", "1782300000 "+codexLine)
	if !ok {
		t.Fatal("codex: expected ok")
	}
	if five.UsedPct != 0 || five.ResetsAt != 1782310969 {
		t.Fatalf("five = %+v", five)
	}
	if seven.UsedPct != 3 || seven.ResetsAt != 1782368376 {
		t.Fatalf("seven = %+v", seven)
	}
}

func TestParseHostQuotaEmptyAndJunk(t *testing.T) {
	for _, in := range []string{"", "   ", "noSpaceJunk", "123 {bad json", `123 {"rate_limits":{"seven_day":{"used_percentage":0,"resets_at":0}}}`} {
		if _, _, _, ok := parseHostQuota("claude", in); ok {
			t.Fatalf("expected !ok for %q", in)
		}
	}
}

func govCfg() Gov {
	return Gov{Enabled: true, WeeklyCeiling: 92, Slack: 8, MaxActive: 16, MinActive: 1}
}

func TestDecideFailsOpenWithoutMeter(t *testing.T) {
	d := govCfg().decide(time.Now(), quota{ok: false}, nil, 5, 0)
	if d.cap != 16 {
		t.Fatalf("no meter should fail open to MaxActive, got %d", d.cap)
	}
}

func TestDecideHardGateAtCeiling(t *testing.T) {
	now := time.Now()
	q := quota{ok: true, seven: RateLimit{UsedPct: 92, ResetsAt: now.Add(48 * time.Hour).Unix()}}
	d := govCfg().decide(now, q, nil, 5, 8)
	if d.cap != 0 {
		t.Fatalf("at ceiling cap should be 0, got %d", d.cap)
	}
}

func TestDecideRelaxesDeepUnderBudget(t *testing.T) {
	now := time.Now()
	// 5% weekly used: below the engage floor => uncapped even with burn samples.
	q := quota{ok: true, seven: RateLimit{UsedPct: 5, ResetsAt: now.Add(72 * time.Hour).Unix()}}
	var samples []QuotaSample
	for i := 0; i < 6; i++ {
		ts := now.Add(time.Duration(-(60 - i*10)) * time.Minute).Unix()
		samples = append(samples, QuotaSample{Ts: ts, SevenPct: float64(i), SevenReset: q.seven.ResetsAt})
	}
	d := govCfg().decide(now, q, samples, 10, 16)
	if d.cap != 16 {
		t.Fatalf("deep under budget should stay MaxActive, got %d", d.cap)
	}
}

// At 90% (within the 84-92 slack band) with NO burn samples yet, the static
// floor must clamp to MinActive immediately — the post-start protection gap.
func TestDecideSlackBandFloorWithoutSamples(t *testing.T) {
	now := time.Now()
	q := quota{ok: true, seven: RateLimit{UsedPct: 90, ResetsAt: now.Add(48 * time.Hour).Unix()}}
	d := govCfg().decide(now, q, nil, 8, 16)
	if d.cap != 1 {
		t.Fatalf("in slack band w/o samples should floor to MinActive(1), got %d", d.cap)
	}
	if d.binding != "weekly" {
		t.Fatalf("binding = %q", d.binding)
	}
}

// Below the band with no samples => fail open (no protection needed yet).
func TestDecideBelowBandNoSamplesFailsOpen(t *testing.T) {
	now := time.Now()
	q := quota{ok: true, seven: RateLimit{UsedPct: 50, ResetsAt: now.Add(48 * time.Hour).Unix()}}
	d := govCfg().decide(now, q, nil, 8, 16)
	if d.cap != 16 {
		t.Fatalf("below band w/o burn estimate should fail open to MaxActive, got %d", d.cap)
	}
}

// Over-pace: high used% near reset with rising burn => cap brakes below prevCap.
func TestDecideBrakesWhenOverPace(t *testing.T) {
	now := time.Now()
	reset := now.Add(24 * time.Hour).Unix()
	q := quota{ok: true, seven: RateLimit{UsedPct: 80, ResetsAt: reset}}
	var samples []QuotaSample
	// Burn ~ +10%/h over the last hour: way above the (92-80)/24 ≈ 0.5%/h target.
	for i := 0; i < 7; i++ {
		ts := now.Add(time.Duration(-(60 - i*10)) * time.Minute).Unix()
		samples = append(samples, QuotaSample{Ts: ts, SevenPct: 70 + float64(i)*1.6, SevenReset: reset})
	}
	d := govCfg().decide(now, q, samples, 8, 8)
	if d.cap >= 8 {
		t.Fatalf("over pace should brake below prevCap(8), got %d (burn %.2f target %.2f)", d.cap, d.burnWeekly, d.targetWeekly)
	}
	if !d.overPace {
		t.Fatal("expected overPace true")
	}
}
