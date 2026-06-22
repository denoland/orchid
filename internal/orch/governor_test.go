package orch

import (
	"math"
	"testing"
	"time"
)

// govNow is the reference clock for the governor tests.
var govNow = time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC)

// govCfg is a fully-zero-but-GovernorEnabled block: withDefaults fills the
// throttle 92/8/85/30m AND the governor max/min(8/1) + the *Dur defaults.
func govCfg() *ThrottleBlock { return &ThrottleBlock{Enabled: true, GovernorEnabled: true} }

// weeklySamples builds a run of samples whose seven_pct rises at ratePerHour
// (%/h) for n points spaced `step` apart, ending at govNow, all on reset
// sevenReset. five_* are left zeroed unless caller overrides.
func weeklySamples(n int, step time.Duration, endPct, ratePerHour float64, sevenReset int64) []QuotaSample {
	var out []QuotaSample
	for i := n - 1; i >= 0; i-- {
		ts := govNow.Add(-time.Duration(i) * step)
		dtHours := float64(i) * float64(step) / float64(time.Hour)
		pct := endPct - ratePerHour*dtHours
		out = append(out, QuotaSample{
			Ts:         ts.Unix(),
			SevenPct:   pct,
			SevenReset: sevenReset,
		})
	}
	return out
}

func sevenPct(s QuotaSample) float64 { return s.SevenPct }
func sevenReset(s QuotaSample) int64 { return s.SevenReset }

func TestBurnRateBasic(t *testing.T) {
	reset := govNow.Add(48 * time.Hour).Unix()
	// 6 samples, 30m apart, rising 10%/h, ending at 60%.
	s := weeklySamples(6, 30*time.Minute, 60, 10, reset)
	rate, ok := burnRatePerHour(s, govNow, 6*time.Hour, sevenPct, sevenReset, reset)
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if math.Abs(rate-10) > 0.5 {
		t.Errorf("rate=%.3f, want ~10", rate)
	}
}

func TestBurnRateTheilSenRobust(t *testing.T) {
	reset := govNow.Add(48 * time.Hour).Unix()
	s := weeklySamples(7, 20*time.Minute, 50, 6, reset)
	// Corrupt one interior sample upward (a spike that's still monotone so it
	// isn't segmented away) — Theil-Sen median should shrug it off.
	s[3].SevenPct += 8
	rate, ok := burnRatePerHour(s, govNow, 6*time.Hour, sevenPct, sevenReset, reset)
	if !ok {
		t.Fatal("ok=false, want true")
	}
	if math.Abs(rate-6) > 1.5 {
		t.Errorf("rate=%.3f, want ~6 despite one outlier", rate)
	}
}

func TestBurnRateTooFew(t *testing.T) {
	reset := govNow.Add(48 * time.Hour).Unix()
	for _, n := range []int{0, 1, 2} {
		s := weeklySamples(n, 30*time.Minute, 50, 10, reset)
		if _, ok := burnRatePerHour(s, govNow, 6*time.Hour, sevenPct, sevenReset, reset); ok {
			t.Errorf("n=%d: ok=true, want false (too few)", n)
		}
	}
	// 3 samples but span < govMinSpan(10m): 3 points 2m apart => span 4m.
	s := weeklySamples(3, 2*time.Minute, 50, 10, reset)
	if _, ok := burnRatePerHour(s, govNow, 6*time.Hour, sevenPct, sevenReset, reset); ok {
		t.Error("span<10m: ok=true, want false")
	}
}

func TestBurnRateWindowReset(t *testing.T) {
	oldReset := govNow.Add(-1 * time.Hour).Unix()
	newReset := govNow.Add(7 * 24 * time.Hour).Unix()
	// Pre-reset points at high pct on oldReset, post-reset points low on newReset.
	mk := func(ago time.Duration, pct float64, reset int64) QuotaSample {
		return QuotaSample{Ts: govNow.Add(-ago).Unix(), SevenPct: pct, SevenReset: reset}
	}
	s := []QuotaSample{
		mk(150*time.Minute, 86, oldReset),
		mk(140*time.Minute, 88, oldReset),
		mk(40*time.Minute, 2, newReset),
		mk(20*time.Minute, 4, newReset),
		mk(0, 6, newReset),
	}
	// Estimate the NEW window: reset-equality filter must drop the old points,
	// so no phantom negative slope and rate ~ from the post-reset rise.
	rate, ok := burnRatePerHour(s, govNow, 6*time.Hour, sevenPct, sevenReset, newReset)
	if !ok {
		t.Fatal("ok=false, want true on the new window")
	}
	if rate < 0 {
		t.Errorf("rate=%.3f, want >= 0 (no phantom negative across reset)", rate)
	}
	// 2pct over 40m => ~6%/h... endpoint slope (6-2)/(40m) = 6%/h.
	if math.Abs(rate-6) > 2 {
		t.Errorf("rate=%.3f, want ~6 on the new-window segment", rate)
	}
}

func TestBurnRateClampNonNegative(t *testing.T) {
	reset := govNow.Add(48 * time.Hour).Unix()
	// Declining pct within one window (e.g. provider correction). After
	// monotone segmentation only the last point may survive — if too few,
	// ok=false; otherwise rate must be clamped >= 0. Build a flat-then-decline
	// that keeps >=3 monotone (equal) points at the tail.
	mk := func(ago time.Duration, pct float64) QuotaSample {
		return QuotaSample{Ts: govNow.Add(-ago).Unix(), SevenPct: pct, SevenReset: reset}
	}
	s := []QuotaSample{
		mk(60*time.Minute, 50),
		mk(40*time.Minute, 50),
		mk(20*time.Minute, 50),
		mk(0, 50),
	}
	rate, ok := burnRatePerHour(s, govNow, 6*time.Hour, sevenPct, sevenReset, reset)
	if !ok {
		t.Fatal("ok=false, want true for flat series")
	}
	if rate != 0 {
		t.Errorf("rate=%.3f, want 0 for a flat series", rate)
	}
}

func TestTargetRate(t *testing.T) {
	// used=50, reset 84h out, ceiling 92 => (92-50)/84 = 0.5%/h.
	reset := govNow.Add(84 * time.Hour).Unix()
	tr := targetRatePerHour(govNow, reset, 50, 92)
	if math.Abs(tr-0.5) > 1e-6 {
		t.Errorf("targetRate=%.5f, want 0.5", tr)
	}
	// Near reset: remaining clamps to govEpsilon(5m) so the rate doesn't blow
	// to +inf; remainingBudget>0 over 5m is a large but finite number.
	near := govNow.Add(1 * time.Minute).Unix()
	trNear := targetRatePerHour(govNow, near, 50, 92)
	if math.IsInf(trNear, 0) || trNear <= 0 {
		t.Errorf("near-reset targetRate=%.3f, want finite positive", trNear)
	}
	// Over ceiling already: remainingBudget clamps to 0 => target 0.
	if got := targetRatePerHour(govNow, reset, 95, 92); got != 0 {
		t.Errorf("over-ceiling targetRate=%.3f, want 0", got)
	}
}

func TestGovernorFailOpen(t *testing.T) {
	reset := govNow.Add(48 * time.Hour).Unix()
	good := weeklySamples(6, 30*time.Minute, 60, 10, reset)
	seven := RateLimit{UsedPct: 60, ResetsAt: reset}
	tests := []struct {
		name    string
		ok      bool
		samples []QuotaSample
		seven   RateLimit
		cfg     *ThrottleBlock
	}{
		{name: "nil cfg", ok: true, samples: good, seven: seven, cfg: nil},
		{name: "governor disabled", ok: true, samples: good, seven: seven, cfg: &ThrottleBlock{Enabled: true}},
		{name: "ok=false", ok: false, samples: good, seven: seven, cfg: govCfg()},
		{name: "seven ResetsAt==0", ok: true, samples: good, seven: RateLimit{UsedPct: 60, ResetsAt: 0}, cfg: govCfg()},
		{name: "too few samples", ok: true, samples: good[:2], seven: seven, cfg: govCfg()},
		{name: "no samples", ok: true, samples: nil, seven: seven, cfg: govCfg()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := GovernorDecide(govNow, RateLimit{}, tc.seven, tc.ok, tc.samples, 3, 4, tc.cfg)
			if d.Enabled {
				t.Errorf("Enabled=true, want false (fail-open)")
			}
			if d.EffectiveCap != math.MaxInt {
				t.Errorf("EffectiveCap=%d, want MaxInt (uncapped)", d.EffectiveCap)
			}
			if d.PausedTarget != 0 {
				t.Errorf("PausedTarget=%d, want 0", d.PausedTarget)
			}
		})
	}
}

func TestCapFromBurn(t *testing.T) {
	// active=4, burn=40%/h (hot), target~10%/h => desiredRaw=4*(10/40)=1.
	// With prevCap=4 the ±1/tick slew limits the move to 3 this tick.
	reset := govNow.Add(48 * time.Hour).Unix()
	// remainingBudget so target ~10: used = 92 - 10*48 = -388 -> clamp issue.
	// Instead pick reset closer: target = (92-used)/hours. Want target 10 with
	// used=60 => hours=(92-60)/10=3.2h => reset 3.2h out.
	reset = govNow.Add(192 * time.Minute).Unix() // 3.2h
	s := weeklySamples(8, 15*time.Minute, 60, 40, reset)
	seven := RateLimit{UsedPct: 60, ResetsAt: reset}
	d := GovernorDecide(govNow, RateLimit{}, seven, true, s, 4, 4, govCfg())
	if !d.Enabled {
		t.Fatal("Enabled=false, want true")
	}
	// burn ~40 > target ~10 => over pace, cap should slew DOWN by 1 from 4.
	if d.EffectiveCap != 3 {
		t.Errorf("EffectiveCap=%d, want 3 (prevCap 4, ±1 slew toward 1)", d.EffectiveCap)
	}
	if !d.OverPace {
		t.Error("OverPace=false, want true (burn >> target)")
	}
	// Re-run with the new cap as prevCap a few times: should keep stepping
	// toward 1 by 1 each tick and never overshoot below minActive(1).
	prev := d.EffectiveCap
	for i := 0; i < 5; i++ {
		d = GovernorDecide(govNow, RateLimit{}, seven, true, s, 4, prev, govCfg())
		if d.EffectiveCap < prev-1 {
			t.Fatalf("cap jumped more than 1/tick: %d -> %d", prev, d.EffectiveCap)
		}
		if d.EffectiveCap < 1 {
			t.Fatalf("cap=%d below minActive", d.EffectiveCap)
		}
		prev = d.EffectiveCap
	}
	if prev != 1 {
		t.Errorf("converged cap=%d, want 1 (hot burn drives to minActive)", prev)
	}
}

func TestCapDeadband(t *testing.T) {
	// burn within ±15% of target => normErr small => cap unchanged.
	// target 10, burn ~10.5 => normErr=0.05 < 0.15.
	reset := govNow.Add(192 * time.Minute).Unix() // target ~10 at used 60
	s := weeklySamples(8, 15*time.Minute, 60, 10.5, reset)
	seven := RateLimit{UsedPct: 60, ResetsAt: reset}
	d := GovernorDecide(govNow, RateLimit{}, seven, true, s, 4, 5, govCfg())
	if d.EffectiveCap != 5 {
		t.Errorf("EffectiveCap=%d, want 5 (deadband => unchanged from prevCap)", d.EffectiveCap)
	}
}

func TestConvergence(t *testing.T) {
	// Closed-loop sim of the full controller (cap admission AND duty-cycle
	// pausing). The plant: each effectively-running session burns k %/h, so
	// effective burn = k * (cap - pausedTarget). The cap law's fixed point is
	// A* = target/k; the duty layer provides the sub-integer trim that
	// admission quantization can't. Assert: (a) the cap moves at most ±1/tick
	// (slew honored, no oscillation), and (b) used% paces up toward the 92
	// ceiling and lands close to it at reset without blowing past it. k is
	// chosen so A* sits at a healthy integer the cap can hold.
	const k = 1.0 // each effectively-running session burns 1%/h
	const ceiling = 92.0
	reset := govNow.Add(72 * time.Hour)
	resetU := reset.Unix()
	cfg := govCfg()

	used := 10.0
	A := 6 // current running count (the daemon will steer the cap)
	prevCap := 8
	tickStep := 15 * time.Minute
	now := reset.Add(-72 * time.Hour)

	// Seed a RISING history consistent with A sessions already burning k*A so
	// the estimator reads real burn on the first decision (a flat seed would
	// read burn~0 and snap the cap to maxActive — an unrealistic cold start).
	var hist []QuotaSample
	seedBurn := k * float64(A)
	for i := 6; i >= 1; i-- {
		ts := now.Add(-time.Duration(i) * tickStep)
		pct := used - seedBurn*(float64(i)*float64(tickStep)/float64(time.Hour))
		hist = append(hist, QuotaSample{Ts: ts.Unix(), SevenPct: pct, SevenReset: resetU})
	}

	prevCapSeen := prevCap
	for now.Before(reset.Add(-1 * time.Hour)) {
		// effective running = cap minus the duty-cycle paused count.
		burn := k * float64(A)
		now = now.Add(tickStep)
		used += burn * (float64(tickStep) / float64(time.Hour))
		if used > 100 {
			used = 100
		}
		hist = append(hist, QuotaSample{Ts: now.Unix(), SevenPct: used, SevenReset: resetU})
		cut := now.Add(-3 * time.Hour).Unix()
		var win []QuotaSample
		for _, h := range hist {
			if h.Ts >= cut {
				win = append(win, h)
			}
		}
		seven := RateLimit{UsedPct: used, ResetsAt: resetU}
		d := GovernorDecide(now, RateLimit{}, seven, true, win, A, prevCap, cfg)
		if !d.Enabled {
			continue
		}
		// (a) cap honors the ±1/tick slew (no oscillation / no jumps).
		if d.EffectiveCap > prevCapSeen+1 || d.EffectiveCap < prevCapSeen-1 {
			t.Fatalf("cap jumped >1/tick: %d -> %d (used=%.1f)", prevCapSeen, d.EffectiveCap, used)
		}
		prevCapSeen = d.EffectiveCap
		prevCap = d.EffectiveCap
		// Daemon applies admission + duty-cycle: effective running count is the
		// cap minus the duty-cycle paused target.
		A = d.EffectiveCap - d.PausedTarget
		if A < 0 {
			A = 0
		}
	}
	// Paces up toward the ceiling and lands close to it, not far past (lockout
	// risk), not far under (the proactive uniform-burn goal: budget left, but
	// mostly used).
	if used > ceiling+3 {
		t.Errorf("used=%.1f overshot ceiling %.0f", used, ceiling)
	}
	if used < ceiling-8 {
		t.Errorf("used=%.1f far under ceiling %.0f — governor over-throttled (wasted budget)", used, ceiling)
	}
}

// TestConvergenceHighK guards CRITICAL #2 (the minActive burn-floor overshoot):
// when a SINGLE running session burns far hotter than the uniform target, the
// admission cap alone cannot help (it floors at minActive=1), so duty-cycle MUST
// pause the lone session to avoid blowing past the ceiling. With the old
// minActive-floored pause this sim lands at 100%; with the fix it stays bounded.
func TestConvergenceHighK(t *testing.T) {
	const k = 4.0 // a single running session burns 4%/h — hotter than the ~1%/h target
	const ceiling = 92.0
	reset := govNow.Add(72 * time.Hour)
	resetU := reset.Unix()
	cfg := govCfg()
	maxPause := cfg.withDefaults().maxPauseDur()

	used := 10.0
	prevCap := 4
	// Production-realistic tick resolution (poll interval ~minutes, maxPause
	// 20m): fine enough that the bang-bang duty-cycle can pace well below 50%.
	tickStep := 2 * time.Minute
	now := reset.Add(-72 * time.Hour)

	// Seed a rising history consistent with the lone session burning k.
	var hist []QuotaSample
	for i := 6; i >= 1; i-- {
		ts := now.Add(-time.Duration(i) * tickStep)
		pct := used - k*(float64(i)*float64(tickStep)/float64(time.Hour))
		hist = append(hist, QuotaSample{Ts: ts.Unix(), SevenPct: pct, SevenReset: resetU})
	}

	// Persistent single-session state, mirroring the daemon's applyDutyCycle:
	// resume when !OverPace (windowed burn decayed back under target) or forced
	// by maxPause; pause when the decision asks for it.
	paused := false
	var pausedAt time.Time
	sawPause := false
	for now.Before(reset.Add(-1 * time.Hour)) {
		now = now.Add(tickStep)
		if !paused {
			used += k * (float64(tickStep) / float64(time.Hour))
			if used > 100 {
				used = 100
			}
		}
		hist = append(hist, QuotaSample{Ts: now.Unix(), SevenPct: used, SevenReset: resetU})
		cut := now.Add(-3 * time.Hour).Unix()
		var win []QuotaSample
		for _, h := range hist {
			if h.Ts >= cut {
				win = append(win, h)
			}
		}
		active := 0
		if !paused {
			active = 1
		}
		d := GovernorDecide(now, RateLimit{}, RateLimit{UsedPct: used, ResetsAt: resetU}, true, win, active, prevCap, cfg)
		if !d.Enabled {
			// Fail-open (e.g. samples went thin during a long pause): resume.
			paused = false
			continue
		}
		prevCap = d.EffectiveCap
		if paused {
			forced := !pausedAt.IsZero() && now.Sub(pausedAt) > maxPause
			if forced || !d.OverPace {
				paused = false
			}
		} else if d.PausedTarget >= 1 {
			paused = true
			pausedAt = now
			sawPause = true
		}
	}
	if !sawPause {
		t.Error("duty-cycle never paused the lone hot session")
	}
	// The key guard: a single session far hotter than target must be held by
	// duty-cycle, not allowed to run flat-out to 100%. (Old minActive-floored
	// pause => 100%; correct duty-cycle keeps it bounded near/under the ceiling.)
	if used > ceiling+5 {
		t.Errorf("used=%.1f overshot ceiling %.0f — single hot session not held by duty-cycle", used, ceiling)
	}
}

// TestParsePriorityFrontmatter covers the toml `priority = N` parse the
// admission/duty ordering depends on.
func TestParsePriorityFrontmatter(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"absent", "just a normal issue body", 0},
		{"basic", "```toml\npriority = 5\n```\nbody text", 5},
		{"quoted", "```toml\npriority = \"3\"\n```", 3},
		{"negative", "```toml\npriority = -2\n```", -2},
		{"alongside schedule", "```toml\nschedule = \"1h\"\npriority = 7\n```", 7},
		{"malformed value", "```toml\npriority = high\n```", 0},
		{"leading blank lines", "\n\n```toml\npriority = 4\n```", 4},
		{"toml not first block", "intro paragraph\n```toml\npriority = 9\n```", 0},
	}
	for _, c := range cases {
		if got := parsePriorityFrontmatter(c.body); got != c.want {
			t.Errorf("%s: parsePriorityFrontmatter=%d, want %d", c.name, got, c.want)
		}
	}
}

// TestJobsByPriority covers the admission/pause-victim/resume base ordering:
// priority (DESC or ASC) with a stable issue-number tiebreak.
func TestJobsByPriority(t *testing.T) {
	st := &State{Jobs: map[int]*Job{
		10: {Priority: 1, Tmux: "claude-10"},
		11: {Priority: 5, Tmux: "claude-11"},
		12: {Priority: 5, Tmux: "claude-12"},
		13: {Priority: 0, Tmux: "claude-13"},
	}}
	all := func(n int, j *Job) bool { return true }

	// DESC: priority high first, issue-number ASC tiebreak.
	if got, want := jobsByPriority(st, all, false), []int{11, 12, 10, 13}; !intsEq(got, want) {
		t.Errorf("DESC got %v, want %v", got, want)
	}
	// ASC: priority low first.
	if got, want := jobsByPriority(st, all, true), []int{13, 10, 11, 12}; !intsEq(got, want) {
		t.Errorf("ASC got %v, want %v", got, want)
	}
	// Filter is honored.
	onlyHi := jobsByPriority(st, func(n int, j *Job) bool { return j.Priority >= 5 }, false)
	if !intsEq(onlyHi, []int{11, 12}) {
		t.Errorf("filtered got %v, want [11 12]", onlyHi)
	}
}

func intsEq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTighterOf(t *testing.T) {
	// Weekly under pace (cold), 5h hot => cap=min(weekly,5h)=5h's cap,
	// paused=max, Binding=="5h".
	weeklyReset := govNow.Add(120 * time.Hour).Unix() // far out => target high => cold
	fiveReset := govNow.Add(150 * time.Minute).Unix() // 2.5h out

	// weekly: used 20, rising slowly (0.3%/h) vs target (92-20)/120=0.6%/h =>
	// genuinely under pace, so its cap slews UP, leaving 5h as the tighter.
	wk := weeklySamples(8, 15*time.Minute, 20, 0.3, weeklyReset)
	// 5h: used 70, rising fast (30%/h). Build five_* on the same timestamps.
	for i := range wk {
		dtHours := float64(wk[len(wk)-1].Ts-wk[i].Ts) / 3600.0
		wk[i].FivePct = 70 - 30*dtHours
		wk[i].FiveReset = fiveReset
	}
	seven := RateLimit{UsedPct: 20, ResetsAt: weeklyReset}
	five := RateLimit{UsedPct: 70, ResetsAt: fiveReset}
	d := GovernorDecide(govNow, five, seven, true, wk, 4, 4, govCfg())
	if !d.Enabled {
		t.Fatal("Enabled=false, want true")
	}
	if d.Binding != "5h" {
		t.Errorf("Binding=%q, want \"5h\" (5h is the tighter, hot bucket)", d.Binding)
	}
	if !d.OverPace {
		t.Error("OverPace=false, want true (5h hot)")
	}
	if d.EffectiveCap > 4 {
		t.Errorf("EffectiveCap=%d, want <=4 (slewed down by hot 5h)", d.EffectiveCap)
	}

	// Reverse: weekly hot, 5h cold => Binding weekly.
	weeklyReset2 := govNow.Add(192 * time.Minute).Unix() // target ~10 at used 60
	fiveReset2 := govNow.Add(4 * time.Hour).Unix()
	wk2 := weeklySamples(8, 15*time.Minute, 60, 40, weeklyReset2)
	for i := range wk2 {
		dtHours := float64(wk2[len(wk2)-1].Ts-wk2[i].Ts) / 3600.0
		wk2[i].FivePct = 10 - 1*dtHours // cold 5h
		wk2[i].FiveReset = fiveReset2
	}
	seven2 := RateLimit{UsedPct: 60, ResetsAt: weeklyReset2}
	five2 := RateLimit{UsedPct: 10, ResetsAt: fiveReset2}
	d2 := GovernorDecide(govNow, five2, seven2, true, wk2, 4, 4, govCfg())
	if d2.Binding != "weekly" {
		t.Errorf("Binding=%q, want \"weekly\"", d2.Binding)
	}
}

func TestFiveBucketSkippedWhenNoReset(t *testing.T) {
	// five.ResetsAt==0 => 5h bucket skipped; weekly alone decides.
	reset := govNow.Add(48 * time.Hour).Unix()
	s := weeklySamples(6, 30*time.Minute, 60, 10, reset)
	seven := RateLimit{UsedPct: 60, ResetsAt: reset}
	d := GovernorDecide(govNow, RateLimit{ResetsAt: 0}, seven, true, s, 3, 4, govCfg())
	if !d.Enabled {
		t.Fatal("Enabled=false, want true (weekly readable)")
	}
	if d.BurnFive != 0 || d.TargetFive != 0 {
		t.Errorf("five telemetry nonzero (%.2f/%.2f), want 0 when 5h skipped", d.BurnFive, d.TargetFive)
	}
	if d.Binding != "weekly" {
		t.Errorf("Binding=%q, want weekly", d.Binding)
	}
}

func TestDeepHeadroomRelax(t *testing.T) {
	// Live regression: a near-empty weekly bucket (6% used, 99h to reset) with a
	// MILD over-pace burn must NOT be soft-paced — the old behavior pinned the
	// cap and paused sessions off a linear projection (6% + ~1%/h*99h => ~109%)
	// despite 94% headroom. Below govEngageFloorPct the soft governor relaxes:
	// no cap, no pauses. The hard gate + 5h bucket remain the real ceiling guard.
	reset := govNow.Add(99 * time.Hour).Unix()
	// Rising history at ~1%/h ending at 6% — a real (if mild) over-target burn.
	s := weeklySamples(8, 15*time.Minute, 6, 1.0, reset)
	seven := RateLimit{UsedPct: 6, ResetsAt: reset}
	d := GovernorDecide(govNow, RateLimit{}, seven, true, s, 13, 11, govCfg())
	if d.PausedTarget != 0 {
		t.Errorf("PausedTarget=%d, want 0 (deep headroom => no duty-cycle pause)", d.PausedTarget)
	}
	// Relaxed bucket contributes the full configured allowance, not a braked cap:
	// EffectiveCap == maxActive (>= active), so admission is unconstrained.
	maxA := govCfg().withDefaults().MaxActive
	if d.EffectiveCap != maxA {
		t.Errorf("EffectiveCap=%d, want maxActive(%d) (deep headroom => no brake below max)", d.EffectiveCap, maxA)
	}

	// Just above the floor the soft governor re-engages and paces a hot burn.
	reset2 := govNow.Add(48 * time.Hour).Unix()
	s2 := weeklySamples(8, 15*time.Minute, 60, 40, reset2)
	seven2 := RateLimit{UsedPct: 60, ResetsAt: reset2}
	d2 := GovernorDecide(govNow, RateLimit{}, seven2, true, s2, 4, 4, govCfg())
	if d2.PausedTarget == 0 {
		t.Error("PausedTarget=0 at 60% used with a hot burn, want >0 (governor still engaged above the floor)")
	}
}

func TestDutyTarget(t *testing.T) {
	// controlBucket directly: active=4, burn=2x target => overFrac=0.5 =>
	// pausedTarget=ceil(4*0.5)=2, and active-2=2 >= minActive(1) ok.
	bc := controlBucket(20, 10, 4, 4, 1, 8)
	if bc.pausedTarget != 2 {
		t.Errorf("pausedTarget=%d, want 2 (overFrac 0.5 of 4)", bc.pausedTarget)
	}
	// Heavily over: burn=100, target=1 => overFrac~0.99 => ceil(4*0.99)=4. The
	// pause floor is 0 (NOT minActive), so duty-cycle can freeze all 4 — the
	// fleet stays admitted, duty just modulates how many run at once.
	bc2 := controlBucket(100, 1, 4, 4, 1, 8)
	if bc2.pausedTarget != 4 {
		t.Errorf("pausedTarget=%d, want 4 (pause floor 0, all freezable)", bc2.pausedTarget)
	}
	// Under pace: burn < target => overFrac 0 => no pausing.
	bc3 := controlBucket(5, 10, 4, 4, 1, 8)
	if bc3.pausedTarget != 0 {
		t.Errorf("pausedTarget=%d, want 0 (under pace)", bc3.pausedTarget)
	}
	// Just barely over within the duty deadband: burn=10.5,target=10 =>
	// overFrac=0.5/10.5=0.048 < govDutyDeadband(0.10) => no pause (anti-thrash).
	bc4 := controlBucket(10.5, 10, 4, 4, 1, 8)
	if bc4.pausedTarget != 0 {
		t.Errorf("pausedTarget=%d, want 0 (overFrac inside duty deadband)", bc4.pausedTarget)
	}
}

func TestSingleHotSession(t *testing.T) {
	// active=1, burn >> target. The cap can't drop below minActive(1) (the fleet
	// stays one session alive), but duty-cycle IS free to freeze that lone
	// session — its pause floor is 0, decoupled from minActive — so a single hot
	// session is held to its target AVERAGE burn (SIGSTOP/CONT cycling) instead
	// of running flat-out past the ceiling.
	reset := govNow.Add(192 * time.Minute).Unix()
	s := weeklySamples(8, 15*time.Minute, 60, 40, reset)
	seven := RateLimit{UsedPct: 60, ResetsAt: reset}
	d := GovernorDecide(govNow, RateLimit{}, seven, true, s, 1, 4, govCfg())
	if d.EffectiveCap < 1 {
		t.Errorf("EffectiveCap=%d, want >=minActive(1)", d.EffectiveCap)
	}
	if !d.OverPace {
		t.Error("OverPace=false, want true for a single hot session")
	}
	// burn~40, target~10 => overFrac=0.75 > deadband => pausedTarget=ceil(1*0.75)=1.
	// The daemon SIGSTOPs it; maxPauseDur + resume-when-no-longer-over cycle it.
	if d.PausedTarget != 1 {
		t.Errorf("PausedTarget=%d, want 1 (duty-cycle freezes the lone hot session)", d.PausedTarget)
	}
	// controlBucket directly: minActive no longer floors the duty pause.
	bc := controlBucket(40, 10, 1, 4, 1, 8)
	if bc.pausedTarget != 1 {
		t.Errorf("controlBucket pausedTarget=%d, want 1 (lone hot session pausable)", bc.pausedTarget)
	}
}

func TestProjectedEndPct(t *testing.T) {
	// used=60, burn ~10%/h, reset 4h out => projected = 60 + 10*4 = 100ish.
	reset := govNow.Add(240 * time.Minute).Unix() // 4h; target=(92-60)/4=8
	s := weeklySamples(8, 15*time.Minute, 60, 10, reset)
	seven := RateLimit{UsedPct: 60, ResetsAt: reset}
	d := GovernorDecide(govNow, RateLimit{}, seven, true, s, 4, 4, govCfg())
	want := 60 + d.BurnWeekly*4
	if math.Abs(d.ProjectedEndPct-want) > 1.0 {
		t.Errorf("ProjectedEndPct=%.2f, want ~%.2f (used + burn*hoursRemaining)", d.ProjectedEndPct, want)
	}
	if d.ProjectedEndPct < 90 {
		t.Errorf("ProjectedEndPct=%.2f, expected to project past ceiling at 10%%/h burn", d.ProjectedEndPct)
	}
}

func TestGovernorDefaults(t *testing.T) {
	c := (ThrottleBlock{Enabled: true, GovernorEnabled: true}).withDefaults()
	if c.MaxActive != defaultGovMaxActive {
		t.Errorf("MaxActive=%d, want %d", c.MaxActive, defaultGovMaxActive)
	}
	if c.MinActive != defaultGovMinActive {
		t.Errorf("MinActive=%d, want %d", c.MinActive, defaultGovMinActive)
	}
	if c.targetCeiling() != defaultWeeklyCeilingPct {
		t.Errorf("targetCeiling=%.1f, want %.1f (falls back to WeeklyCeilingPct)", c.targetCeiling(), defaultWeeklyCeilingPct)
	}
	if c.sampleIntervalDur() != defaultGovSampleInterval {
		t.Errorf("sampleIntervalDur=%v, want %v", c.sampleIntervalDur(), defaultGovSampleInterval)
	}
	if c.rateWindowDur() != defaultGovRateWindow {
		t.Errorf("rateWindowDur=%v, want %v", c.rateWindowDur(), defaultGovRateWindow)
	}
	if c.fiveRateWindowDur() != defaultGovFiveRateWindow {
		t.Errorf("fiveRateWindowDur=%v, want %v", c.fiveRateWindowDur(), defaultGovFiveRateWindow)
	}
	if c.maxPauseDur() != defaultGovMaxPauseDur {
		t.Errorf("maxPauseDur=%v, want %v", c.maxPauseDur(), defaultGovMaxPauseDur)
	}
	// explicit override wins
	c2 := (ThrottleBlock{Enabled: true, GovernorEnabled: true, TargetCeilingPct: 80}).withDefaults()
	if c2.targetCeiling() != 80 {
		t.Errorf("targetCeiling=%.1f, want 80 (explicit override)", c2.targetCeiling())
	}
	// bad duration strings fall back
	c3 := (ThrottleBlock{RateWindow: "banana"}).withDefaults()
	if c3.rateWindowDur() != defaultGovRateWindow {
		t.Errorf("bad rateWindowDur=%v, want default %v", c3.rateWindowDur(), defaultGovRateWindow)
	}
}
