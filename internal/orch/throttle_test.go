package orch

import (
	"testing"
	"time"
)

// fixedNow is the reference clock for the throttle tests. Reset windows
// are derived from it so the elapsed/target math is explicit.
var fixedNow = time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)

// mkSeven builds a 7-day RateLimit whose window resets resetIn from
// fixedNow (positive = future, negative = past).
func mkSeven(usedPct float64, resetIn time.Duration) RateLimit {
	return RateLimit{UsedPct: usedPct, ResetsAt: fixedNow.Add(resetIn).Unix()}
}

// mkFive builds a 5-hour RateLimit whose window resets resetIn from
// fixedNow.
func mkFive(usedPct float64, resetIn time.Duration) RateLimit {
	return RateLimit{UsedPct: usedPct, ResetsAt: fixedNow.Add(resetIn).Unix()}
}

// enabled is a fully-zero-but-Enabled block: withDefaults fills 92/8/85/30m.
func enabledCfg() *ThrottleBlock { return &ThrottleBlock{Enabled: true} }

func TestThrottleFailOpen(t *testing.T) {
	// Heavily over-pace input that WOULD throttle if the throttle were active.
	hot := mkSeven(99, 2*24*time.Hour) // 99% used, week resets in 2 days
	tests := []struct {
		name string
		five RateLimit
		sevn RateLimit
		ok   bool
		cfg  *ThrottleBlock
	}{
		{name: "nil cfg", five: RateLimit{}, sevn: hot, ok: true, cfg: nil},
		{name: "disabled", five: RateLimit{}, sevn: hot, ok: true, cfg: &ThrottleBlock{Enabled: false}},
		{name: "not ok / no statusline", five: RateLimit{}, sevn: hot, ok: false, cfg: enabledCfg()},
		{name: "seven ResetsAt==0", five: RateLimit{}, sevn: RateLimit{UsedPct: 99, ResetsAt: 0}, ok: true, cfg: enabledCfg()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ThrottleDecide(fixedNow, tc.five, tc.sevn, tc.ok, tc.cfg)
			if d.Mode != ModeAllow {
				t.Errorf("got mode %v, want ModeAllow (fail-open)", d.Mode)
			}
			if d.BlocksNewWork() {
				t.Errorf("BlocksNewWork=true, want false on fail-open")
			}
		})
	}
}

func TestThrottleUnderPaceAllows(t *testing.T) {
	// Week resets in 2 days => weekStart = now-5d => target ~71.4%.
	// used=50 is well under target+slack, so allow.
	d := ThrottleDecide(fixedNow, mkFive(10, 3*time.Hour), mkSeven(50, 2*24*time.Hour), true, enabledCfg())
	if d.Mode != ModeAllow {
		t.Fatalf("got mode %v (%s), want ModeAllow", d.Mode, d.Reason)
	}
	if d.TargetPct < 70 || d.TargetPct > 72 {
		t.Errorf("targetPct=%.2f, want ~71.4", d.TargetPct)
	}
}

func TestThrottleOverPaceThrottles(t *testing.T) {
	// Same window (target ~71.4%), slack 8 => threshold 79.4%. used=85 over.
	d := ThrottleDecide(fixedNow, mkFive(10, 3*time.Hour), mkSeven(85, 2*24*time.Hour), true, enabledCfg())
	if d.Mode != ModeThrottle {
		t.Fatalf("got mode %v (%s), want ModeThrottle", d.Mode, d.Reason)
	}
	if d.Reason == "" {
		t.Errorf("expected a non-empty Reason for throttle")
	}
	if d.BlocksNewWork() != true {
		t.Errorf("BlocksNewWork=false, want true under throttle")
	}
}

func TestThrottleLinearPaceBoundary(t *testing.T) {
	// target ~71.43%, slack 8 => threshold ~79.43%. Strict '>' boundary.
	const tol = 1e-6
	// Compute the exact target so we can probe the boundary precisely.
	reset := fixedNow.Add(2 * 24 * time.Hour)
	weekStart := reset.Add(-weekWindow)
	target := float64(fixedNow.Sub(weekStart)) / float64(weekWindow) * 100
	thresh := target + 8.0

	tests := []struct {
		name string
		used float64
		want ThrottleMode
	}{
		{name: "well under target", used: target - 10, want: ModeAllow},
		{name: "exactly at threshold (strict >)", used: thresh, want: ModeAllow},
		{name: "just below threshold", used: thresh - tol, want: ModeAllow},
		{name: "just above threshold", used: thresh + 0.1, want: ModeThrottle},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ThrottleDecide(fixedNow, mkFive(0, 0), mkSeven(tc.used, 2*24*time.Hour), true, enabledCfg())
			if d.Mode != tc.want {
				t.Errorf("used=%.4f thresh=%.4f: got %v, want %v", tc.used, thresh, d.Mode, tc.want)
			}
		})
	}
}

func TestThrottleWeeklyCeiling(t *testing.T) {
	tests := []struct {
		name string
		used float64
		want ThrottleMode
	}{
		// Window resets in 6 days => weekStart = now-1d => target ~14.3%.
		// So pace alone would throttle at >22.3%; isolate the ceiling by
		// comparing 91.9% (throttle by pace) vs 92% (ceiling).
		{name: "just below ceiling -> throttle by pace", used: 91.9, want: ModeThrottle},
		{name: "at ceiling -> pause week", used: 92, want: ModePauseWeek},
		{name: "above ceiling -> pause week", used: 95, want: ModePauseWeek},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seven := mkSeven(tc.used, 6*24*time.Hour)
			d := ThrottleDecide(fixedNow, mkFive(0, 0), seven, true, enabledCfg())
			if d.Mode != tc.want {
				t.Fatalf("used=%.1f: got %v (%s), want %v", tc.used, d.Mode, d.Reason, tc.want)
			}
			if tc.want == ModePauseWeek {
				wantUntil := time.Unix(seven.ResetsAt, 0)
				if !d.Until.Equal(wantUntil) {
					t.Errorf("Until=%v, want %v (seven reset)", d.Until, wantUntil)
				}
			}
		})
	}
}

func TestThrottleFiveHourGuard(t *testing.T) {
	// Weekly under pace (used=10, resets in 2 days -> target 71%) so only
	// the 5h guard can fire. Default 5h threshold is 85.
	weekly := mkSeven(10, 2*24*time.Hour)
	tests := []struct {
		name string
		five RateLimit
		want ThrottleMode
	}{
		{name: "5h below threshold", five: mkFive(50, 2*time.Hour), want: ModeAllow},
		{name: "5h at threshold", five: mkFive(85, 2*time.Hour), want: ModePauseFiveHour},
		{name: "5h above threshold", five: mkFive(90, 2*time.Hour), want: ModePauseFiveHour},
		{name: "5h hot but ResetsAt==0 disables guard", five: RateLimit{UsedPct: 99, ResetsAt: 0}, want: ModeAllow},
		{name: "5h hot but reset in the past disables guard", five: mkFive(99, -time.Hour), want: ModeAllow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ThrottleDecide(fixedNow, tc.five, weekly, true, enabledCfg())
			if d.Mode != tc.want {
				t.Fatalf("got %v (%s), want %v", d.Mode, d.Reason, tc.want)
			}
			if tc.want == ModePauseFiveHour {
				wantUntil := time.Unix(tc.five.ResetsAt, 0)
				if !d.Until.Equal(wantUntil) {
					t.Errorf("Until=%v, want %v (five reset)", d.Until, wantUntil)
				}
			}
		})
	}
}

func TestThrottleFiveHourLinearPace(t *testing.T) {
	// Weekly cold (used=10, target ~71) so only the 5h linear pacer can fire,
	// and 5h used stays below the hard 85 guard so we isolate the soft pace.
	// 5h resets in 2.5h => fiveStart = now-2.5h => elapsed frac 50% =>
	// target 50, +slack 8 => threshold 58 (strict '>').
	weekly := mkSeven(10, 2*24*time.Hour)
	tests := []struct {
		name string
		used float64
		want ThrottleMode
	}{
		{name: "5h under linear pace", used: 40, want: ModeAllow},
		{name: "5h exactly at threshold (strict >)", used: 58, want: ModeAllow},
		{name: "5h over linear pace -> soft throttle", used: 70, want: ModeThrottle},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ThrottleDecide(fixedNow, mkFive(tc.used, 150*time.Minute), weekly, true, enabledCfg())
			if d.Mode != tc.want {
				t.Fatalf("5h used=%.0f: got %v (%s), want %v", tc.used, d.Mode, d.Reason, tc.want)
			}
			if tc.want == ModeThrottle && d.Reason == "" {
				t.Errorf("expected non-empty Reason for 5h throttle")
			}
		})
	}
}

func TestThrottlePrecedence(t *testing.T) {
	// Weekly >= ceiling AND 5h >= pause simultaneously: ceiling wins.
	seven := mkSeven(95, 3*24*time.Hour)
	five := mkFive(99, 2*time.Hour)
	d := ThrottleDecide(fixedNow, five, seven, true, enabledCfg())
	if d.Mode != ModePauseWeek {
		t.Fatalf("got %v (%s), want ModePauseWeek (most conservative wins)", d.Mode, d.Reason)
	}
	if !d.Until.Equal(time.Unix(seven.ResetsAt, 0)) {
		t.Errorf("Until=%v, want weekly reset %v", d.Until, time.Unix(seven.ResetsAt, 0))
	}
}

func TestThrottleProjectedExhaustEquivalence(t *testing.T) {
	// At slack 0, Mode==ModeThrottle iff ProjectedExhaust.Before(reset),
	// when no pause mode fires and used>0.
	cfg := &ThrottleBlock{Enabled: true, SlackPct: 0.001} // ~0 but withDefaults won't override (>0)
	resetIns := []time.Duration{24 * time.Hour, 3 * 24 * time.Hour, 5 * 24 * time.Hour}
	useds := []float64{5, 25, 50, 70, 88} // keep below 92 ceiling & cold 5h
	for _, ri := range resetIns {
		for _, u := range useds {
			seven := mkSeven(u, ri)
			d := ThrottleDecide(fixedNow, RateLimit{}, seven, true, cfg)
			if d.Mode == ModePauseWeek || d.Mode == ModePauseFiveHour {
				t.Fatalf("unexpected pause mode for used=%.0f resetIn=%v", u, ri)
			}
			reset := time.Unix(seven.ResetsAt, 0)
			throttled := d.Mode == ModeThrottle
			beforeReset := !d.ProjectedExhaust.IsZero() && d.ProjectedExhaust.Before(reset)
			if throttled != beforeReset {
				t.Errorf("used=%.0f resetIn=%v: throttled=%v but projectedExhaust(%v).Before(reset %v)=%v",
					u, ri, throttled, d.ProjectedExhaust, reset, beforeReset)
			}
		}
	}
}

func TestThrottleZeroUsageGuards(t *testing.T) {
	// usedPct==0 with a valid future reset and elapsed>0: no panic, no
	// projected exhaustion, allow (assuming 5h cold).
	d := ThrottleDecide(fixedNow, RateLimit{}, mkSeven(0, 3*24*time.Hour), true, enabledCfg())
	if d.Mode != ModeAllow {
		t.Fatalf("got %v (%s), want ModeAllow for 0%% usage", d.Mode, d.Reason)
	}
	if !d.ProjectedExhaust.IsZero() {
		t.Errorf("ProjectedExhaust=%v, want zero at 0%% burn", d.ProjectedExhaust)
	}
}

func TestThrottleStale(t *testing.T) {
	// Reset > StaleAfter (30m) in the past => stale => allow even at 99%.
	stale := mkSeven(99, -time.Hour)
	d := ThrottleDecide(fixedNow, RateLimit{}, stale, true, enabledCfg())
	if d.Mode != ModeAllow {
		t.Errorf("stale window: got %v (%s), want ModeAllow", d.Mode, d.Reason)
	}

	// Reset in the past but within StaleAfter => NOT stale, still evaluated
	// (elapsed clamps so target=100). But the weekly window has rolled over
	// (reset is behind now), so the ceiling guard's reset.After(now) gate
	// declines to pause on a frozen near-ceiling reading: => allow. This
	// avoids a bounded stall after rollover when statuslines freeze; it
	// self-heals as soon as any in-flight session emits a fresh reading.
	fresh := mkSeven(99, -10*time.Minute)
	d2 := ThrottleDecide(fixedNow, RateLimit{}, fresh, true, enabledCfg())
	if d2.Mode != ModeAllow {
		t.Errorf("rolled-over-but-not-stale window: got %v (%s), want ModeAllow", d2.Mode, d2.Reason)
	}
	if d2.TargetPct != 100 {
		t.Errorf("targetPct=%.2f, want 100 (clamped)", d2.TargetPct)
	}
}

func TestThrottleSelfCorrecting(t *testing.T) {
	// Fixed usage = 60%; advance `now` forward and watch a throttle
	// decision become allow once target overtakes used-slack. Anchor the
	// reset to a fixed absolute instant so weekStart is constant as now moves.
	resetAbs := fixedNow.Add(2 * 24 * time.Hour) // resets 2 days after fixedNow
	seven := RateLimit{UsedPct: 60, ResetsAt: resetAbs.Unix()}
	weekStart := resetAbs.Add(-weekWindow)

	// Early in the week: target low, 60% over pace => throttle.
	early := weekStart.Add(2 * 24 * time.Hour) // ~28.5% target
	dEarly := ThrottleDecide(early, RateLimit{}, seven, true, enabledCfg())
	if dEarly.Mode != ModeThrottle {
		t.Fatalf("early: got %v (%s), want ModeThrottle (target %.1f)", dEarly.Mode, dEarly.Reason, dEarly.TargetPct)
	}

	// Later, no extra burn: target rises past 60-8=52 => allow.
	later := weekStart.Add(5 * 24 * time.Hour) // ~71.4% target
	dLater := ThrottleDecide(later, RateLimit{}, seven, true, enabledCfg())
	if dLater.Mode != ModeAllow {
		t.Fatalf("later: got %v (%s), want ModeAllow (target %.1f)", dLater.Mode, dLater.Reason, dLater.TargetPct)
	}
}

func TestThrottleClampSkew(t *testing.T) {
	// Clock skew: reset 8 days out (> weekWindow) => weekStart in the
	// future => elapsedFrac clamps to 0 => target 0 => any used>slack throttles.
	d := ThrottleDecide(fixedNow, RateLimit{}, mkSeven(10, 8*24*time.Hour), true, enabledCfg())
	if d.TargetPct != 0 {
		t.Errorf("targetPct=%.2f, want 0 (clamped on future weekStart)", d.TargetPct)
	}
	if d.Mode != ModeThrottle {
		t.Errorf("got %v (%s), want ModeThrottle at target 0 + used 10", d.Mode, d.Reason)
	}
	// No panic / no negative projected exhaustion blowup is implicit by
	// reaching here.
}

func TestThrottleUnitsSeconds(t *testing.T) {
	// Guard against the seconds-vs-nanoseconds footgun: a realistic 2026
	// unix-seconds reset should yield a pause Until within ~1s of the
	// intended reset, not a nanosecond-mangled time.
	resetIn := 2 * 24 * time.Hour
	seven := mkSeven(95, resetIn) // ceiling -> pause_week, Until = reset
	d := ThrottleDecide(fixedNow, RateLimit{}, seven, true, enabledCfg())
	if d.Mode != ModePauseWeek {
		t.Fatalf("got %v, want ModePauseWeek", d.Mode)
	}
	want := fixedNow.Add(resetIn)
	if diff := d.Until.Sub(want); diff > time.Second || diff < -time.Second {
		t.Errorf("Until=%v, want ~%v (diff %v) — units mangled?", d.Until, want, diff)
	}
	// weekStart should land roughly 5 days before now, in seconds.
	gotWeekStart := time.Unix(seven.ResetsAt, 0).Add(-weekWindow)
	wantWeekStart := fixedNow.Add(resetIn - weekWindow)
	if diff := gotWeekStart.Sub(wantWeekStart); diff > time.Second || diff < -time.Second {
		t.Errorf("weekStart=%v, want ~%v", gotWeekStart, wantWeekStart)
	}
}

func TestThrottleDefaults(t *testing.T) {
	// All numeric fields zero + Enabled => withDefaults yields 92/8/85/30m.
	c := (ThrottleBlock{Enabled: true}).withDefaults()
	if c.WeeklyCeilingPct != defaultWeeklyCeilingPct {
		t.Errorf("WeeklyCeilingPct=%.1f, want %.1f", c.WeeklyCeilingPct, defaultWeeklyCeilingPct)
	}
	if c.SlackPct != defaultSlackPct {
		t.Errorf("SlackPct=%.1f, want %.1f", c.SlackPct, defaultSlackPct)
	}
	if c.FiveHourPausePct != defaultFiveHourPausePct {
		t.Errorf("FiveHourPausePct=%.1f, want %.1f", c.FiveHourPausePct, defaultFiveHourPausePct)
	}
	if c.staleDur() != defaultStaleAfter {
		t.Errorf("staleDur()=%v, want %v", c.staleDur(), defaultStaleAfter)
	}
	// Bad StaleAfter string => default, no error.
	bad := ThrottleBlock{Enabled: true, StaleAfter: "banana"}
	if bad.staleDur() != defaultStaleAfter {
		t.Errorf("bad staleDur()=%v, want %v", bad.staleDur(), defaultStaleAfter)
	}
}

func TestThrottlePokeGate(t *testing.T) {
	tests := []struct {
		name  string
		mode  ThrottleMode
		pokes bool
		want  bool
	}{
		{name: "pokes off, pause_week", mode: ModePauseWeek, pokes: false, want: false},
		{name: "pokes off, pause_5h", mode: ModePauseFiveHour, pokes: false, want: false},
		{name: "pokes off, throttle", mode: ModeThrottle, pokes: false, want: false},
		{name: "pokes on, pause_week", mode: ModePauseWeek, pokes: true, want: true},
		{name: "pokes on, pause_5h", mode: ModePauseFiveHour, pokes: true, want: true},
		{name: "pokes on, throttle (soft) not gated", mode: ModeThrottle, pokes: true, want: false},
		{name: "pokes on, allow not gated", mode: ModeAllow, pokes: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := ThrottleDecision{Mode: tc.mode}
			cfg := &ThrottleBlock{ThrottlePokes: tc.pokes}
			if got := d.BlocksPokes(cfg); got != tc.want {
				t.Errorf("BlocksPokes=%v, want %v", got, tc.want)
			}
		})
	}
	// nil cfg never blocks pokes.
	if (ThrottleDecision{Mode: ModePauseWeek}).BlocksPokes(nil) {
		t.Errorf("BlocksPokes(nil)=true, want false")
	}
}

func TestThrottleModeString(t *testing.T) {
	cases := map[ThrottleMode]string{
		ModeAllow:         "allow",
		ModeThrottle:      "throttle",
		ModePauseFiveHour: "pause_5h",
		ModePauseWeek:     "pause_week",
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("ThrottleMode(%d).String()=%q, want %q", m, got, want)
		}
	}
}
