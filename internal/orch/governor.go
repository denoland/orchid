package orch

import (
	"math"
	"sort"
	"time"
)

// Proactive pacing governor. Builds on throttle.go's pure pacing math.
//
// The merged throttle (ThrottleDecide) stays the hard binary gate (safety
// floor). The governor is a layer ON TOP that only ever makes work MORE
// restrictive than the gate, never more permissive:
//
//	final admission = min(gate-allowed, governor-allowed)
//
// Where the throttle is reactive+binary (admit everything until a bucket is
// hot, then slam the brake), the governor is proactive: it estimates the live
// burn rate per bucket, computes the burn rate that would land EXACTLY on the
// target ceiling (92% => 8% reserve) at the next reset, and from those two
// derives (a) an adaptive concurrency cap that limits inflow and (b) a
// duty-cycle pause target (SIGSTOP/SIGCONT count) for work already in flight.
// The aim is a ~uniform burn across the whole 7-day week so there is still
// budget on day 7.
//
// Everything here is PURE: no globals, no locks, no I/O. The daemon (tick.go)
// owns the live quota, the persisted samples, the job set, the ordering, and
// the SIGSTOP/SIGCONT; this file owns only the math and returns the numbers.
// That keeps the control law fully unit-testable with synthetic samples and a
// scalar active count.

// Governor tuning constants (§ canonical spec). Fixed, not configurable —
// they describe the estimator/controller numerics, not policy.
const (
	govMinSamples   = 3                // need at least this many kept samples to estimate a slope
	govMinSpan      = 10 * time.Minute // and at least this much wall-time span
	govEpsilon      = 5 * time.Minute  // near-reset clamp on remaining time (div-guard)
	govMinRate      = 0.05             // %/h floor used as a divide-guard on rates
	govCapDeadband  = 0.15             // |normErr| within this => leave the cap unchanged (hysteresis)
	govDutyDeadband = 0.10             // overFrac change within this => don't re-target duty (anti-thrash)
	govSlewPerTick  = 1                // cap may move at most this many slots per tick
)

// QuotaSample is one persisted reading of both rate-limit buckets at a wall
// instant. It is the governor's only time-series input. Stored append-only in
// the quota_samples table (see store.go); the burn-rate estimator consumes a
// window of these. ts and the *Reset fields are unix SECONDS, *Pct are
// used_percentage 0-100.
type QuotaSample struct {
	Ts         int64   `json:"ts"`
	FivePct    float64 `json:"five_pct"`
	FiveReset  int64   `json:"five_reset"`
	SevenPct   float64 `json:"seven_pct"`
	SevenReset int64   `json:"seven_reset"`
}

// GovernorDecision is the per-tick verdict. Its fail-open value (Enabled=false,
// EffectiveCap=math.MaxInt, PausedTarget=0) means "behave exactly like today":
// no admission cap, nothing paused. The daemon turns these numbers into an
// admit list / stop set / resume set using per-job priority ordering.
type GovernorDecision struct {
	Enabled         bool
	EffectiveCap    int     // min over buckets; math.MaxInt when fail-open
	PausedTarget    int     // max over buckets; 0 when fail-open
	OverPace        bool    // true when the binding bucket's burn exceeds its target
	BurnWeekly      float64 // %/h, 0 when that bucket is fail-open/thin
	TargetWeekly    float64 // %/h
	BurnFive        float64 // %/h
	TargetFive      float64 // %/h
	ProjectedEndPct float64 // used_now + burnWeekly * hoursRemaining; weekly bucket
	Binding         string  // "weekly" | "5h" | "" — which bucket produced the tighter cap
}

// failOpenGovernor is the canonical do-nothing decision.
func failOpenGovernor() GovernorDecision {
	return GovernorDecision{Enabled: false, EffectiveCap: math.MaxInt, PausedTarget: 0}
}

// hours converts a duration to fractional hours.
func hours(d time.Duration) float64 { return float64(d) / float64(time.Hour) }

// burnRatePerHour estimates the consumption rate (used_percentage per hour) of
// one bucket from a window of samples. Pure, run independently per bucket: for
// the weekly bucket pass the seven_pct/seven_reset accessors + the current
// seven reset; for the 5h bucket pass the five_* accessors + current five
// reset.
//
// Returns ok=false (=> this bucket contributes NO constraint) on thin/degenerate
// data so a single bucket can never wedge work. Steps (§1a):
//
//  1. Keep samples within [now-window, now] AND whose reset == curReset. The
//     reset-equality filter is the clean rollover handler: after a window
//     resets, curReset changes and used% drops to ~0, so every pre-reset point
//     is dropped and the estimator restarts on the new window — no phantom
//     negative slope.
//  2. used% is monotone non-decreasing within a window; on any negative delta
//     (noise / mid-window glitch) split and keep only the most-recent monotone
//     segment.
//  3. Fail-open if < govMinSamples kept OR span < govMinSpan.
//  4. rate = Theil-Sen median of pairwise slopes (robust to one outlier); fall
//     back to the endpoint slope when there are < 3 pairs. Clamp rate >= 0.
func burnRatePerHour(samples []QuotaSample, now time.Time, window time.Duration,
	pct func(QuotaSample) float64, reset func(QuotaSample) int64,
	curReset int64) (rate float64, ok bool) {

	if curReset == 0 || window <= 0 {
		return 0, false
	}
	cutoff := now.Unix() - int64(window/time.Second)

	// (1) Window + reset-equality filter.
	type pt struct {
		t   float64 // hours since epoch
		pct float64
	}
	var kept []pt
	for _, s := range samples {
		if s.Ts < cutoff || s.Ts > now.Unix() {
			continue
		}
		if reset(s) != curReset {
			continue
		}
		kept = append(kept, pt{t: float64(s.Ts) / 3600.0, pct: pct(s)})
	}
	if len(kept) < govMinSamples {
		return 0, false
	}
	// Samples are loaded ASC by ts, but don't rely on it.
	sort.Slice(kept, func(i, j int) bool { return kept[i].t < kept[j].t })

	// (2) Keep only the most-recent monotone-non-decreasing segment. Walk from
	// the end backward; stop at the first point that is GREATER than its
	// successor (a drop, going forward = noise/glitch boundary).
	start := 0
	for i := len(kept) - 1; i > 0; i-- {
		if kept[i-1].pct > kept[i].pct {
			start = i
			break
		}
	}
	seg := kept[start:]

	// (3) Thin-data fail-open after segmentation.
	if len(seg) < govMinSamples {
		return 0, false
	}
	span := seg[len(seg)-1].t - seg[0].t // hours
	if span < hours(govMinSpan) {
		return 0, false
	}

	// (4) Theil-Sen median pairwise slope; endpoint fallback for < 3 pairs.
	var slopes []float64
	for i := 0; i < len(seg); i++ {
		for j := i + 1; j < len(seg); j++ {
			dt := seg[j].t - seg[i].t
			if dt <= 0 {
				continue
			}
			slopes = append(slopes, (seg[j].pct-seg[i].pct)/dt)
		}
	}
	if len(slopes) < 3 {
		rate = (seg[len(seg)-1].pct - seg[0].pct) / span
	} else {
		sort.Float64s(slopes)
		m := len(slopes)
		if m%2 == 1 {
			rate = slopes[m/2]
		} else {
			rate = (slopes[m/2-1] + slopes[m/2]) / 2
		}
	}
	if rate < 0 {
		rate = 0
	}
	return rate, true
}

// targetRatePerHour is the burn rate (%/h) that lands EXACTLY on ceilingPct at
// reset, computed from the CURRENT used% and remaining time (§1b). Recomputing
// it every tick from live state folds position error into the setpoint — ran
// hot early => budget shrinks => target drops => cap drops further. This is
// integral-like correction with no separate integrator and no wind-up.
func targetRatePerHour(now time.Time, curReset int64, usedNow, ceilingPct float64) float64 {
	reset := time.Unix(curReset, 0)
	remaining := reset.Sub(now)
	if remaining <= govEpsilon {
		remaining = govEpsilon // near-reset div-guard
	}
	remainingBudget := ceilingPct - usedNow
	if remainingBudget < 0 {
		remainingBudget = 0
	}
	return remainingBudget / hours(remaining)
}

// bucketControl is the per-bucket controller output.
type bucketControl struct {
	ok           bool
	burn         float64 // %/h
	target       float64 // %/h
	cap          int
	pausedTarget int
}

// controlBucket runs the cap + duty-cycle control law (§1c, §1d) for one
// bucket given its estimated burn and computed target rates. active is the
// current running (non-paused) session count; prevCap is last tick's cap (for
// the deadband + slew). minActive/maxActive bound the cap.
func controlBucket(burn, target float64, active, prevCap, minActive, maxActive int) bucketControl {
	bc := bucketControl{ok: true, burn: burn, target: target}

	// (1c) Effective cap with ASYMMETRIC deadband + ±slew. Growth (under pace) is
	// NOT suppressed by the deadband: the controller must be free to use real
	// headroom, and the duty-cycle layer backstops any transient overshoot from
	// admitting one too many — so a cap stuck low (e.g. a stale persisted govCap
	// after a prior over-pace episode) always recovers. Braking (over pace) keeps
	// the deadband as hysteresis so burn-estimate noise doesn't cause needless
	// cap churn.
	desiredRaw := float64(active) * (target / math.Max(burn, govMinRate))
	normErr := (burn - target) / math.Max(target, govMinRate)
	lo := float64(prevCap - govSlewPerTick)
	hi := float64(prevCap + govSlewPerTick)
	var newCap float64
	switch {
	case normErr < 0:
		// Under pace: grow toward desiredRaw (>= active) by at most +slew.
		newCap = math.Max(lo, math.Min(hi, desiredRaw))
	case normErr > govCapDeadband:
		// Clearly over pace: brake toward desiredRaw by at most -slew.
		newCap = math.Max(lo, math.Min(hi, desiredRaw))
	default:
		// Slightly over but within the deadband: hold (anti-noise hysteresis).
		newCap = float64(prevCap)
	}
	capInt := int(math.Round(newCap))
	if capInt < minActive {
		capInt = minActive
	}
	if capInt > maxActive {
		capInt = maxActive
	}
	bc.cap = capInt

	// (1d) Over-pace => duty-cycle pause target. The cap throttles inflow;
	// duty-cycle is the faster brake on work already in flight (active <= cap
	// yet still burning hot — the single-hot-session case admission can't fix).
	overFrac := (burn - target) / math.Max(burn, govMinRate)
	if overFrac < 0 {
		overFrac = 0
	} else if overFrac > 1 {
		overFrac = 1
	}
	// Engage duty-cycle only past govDutyDeadband so small over-pace noise can't
	// thrash STOP/CONT. The pause floor is 0, NOT minActive: minActive bounds the
	// ADMISSION cap (how many sessions stay alive while under budget), but
	// duty-cycle must be free to freeze ALL running sessions transiently —
	// otherwise a lone hot session (active==1) is pausable by neither admission
	// nor duty and burns flat-out past the ceiling. The fleet stays minActive-
	// sized; duty modulates how many of them run at once, and maxPauseDur +
	// resume-when-no-longer-over (governor_loop.go) bound how long any pause lasts.
	if overFrac > govDutyDeadband {
		pt := int(math.Ceil(float64(active) * overFrac))
		if pt > active {
			pt = active
		}
		bc.pausedTarget = pt
	}
	return bc
}

// GovernorDecide is the one pure entry point (§5). It estimates burn per
// bucket, computes per-bucket targets/caps/duty, then takes the tighter-of the
// weekly and 5h buckets. It fails open (Enabled=false, cap=MaxInt, paused=0)
// on: cfg nil / !GovernorEnabled / !ok / seven.ResetsAt==0 / both buckets
// thin-data. A fail-open bucket contributes +inf to the cap min and 0 to the
// paused max, so the other bucket can still bind on its own.
func GovernorDecide(now time.Time, five, seven RateLimit, ok bool,
	samples []QuotaSample, active, prevCap int, cfg *ThrottleBlock) GovernorDecision {

	if cfg == nil || !cfg.GovernorEnabled || !ok || seven.ResetsAt == 0 {
		return failOpenGovernor()
	}
	c := cfg.withDefaults()
	minActive, maxActive := c.MinActive, c.MaxActive
	ceiling := c.targetCeiling()

	// prevCap may be unset (0) on first tick / fresh restart => start permissive
	// at maxActive so the slew has a sane anchor and we don't snap to 1.
	if prevCap <= 0 || prevCap == math.MaxInt {
		prevCap = maxActive
	}

	// Weekly bucket.
	weekly := bucketControl{}
	if rate, bok := burnRatePerHour(samples, now, c.rateWindowDur(),
		func(s QuotaSample) float64 { return s.SevenPct },
		func(s QuotaSample) int64 { return s.SevenReset },
		seven.ResetsAt); bok {
		tr := targetRatePerHour(now, seven.ResetsAt, seven.UsedPct, ceiling)
		weekly = controlBucket(rate, tr, active, prevCap, minActive, maxActive)
	}

	// 5h bucket — skipped entirely when there is no live 5h reset.
	fiveB := bucketControl{}
	if five.ResetsAt != 0 {
		if rate, bok := burnRatePerHour(samples, now, c.fiveRateWindowDur(),
			func(s QuotaSample) float64 { return s.FivePct },
			func(s QuotaSample) int64 { return s.FiveReset },
			five.ResetsAt); bok {
			tr := targetRatePerHour(now, five.ResetsAt, five.UsedPct, ceiling)
			fiveB = controlBucket(rate, tr, active, prevCap, minActive, maxActive)
		}
	}

	// Both buckets thin/unreadable => fully fail-open.
	if !weekly.ok && !fiveB.ok {
		return failOpenGovernor()
	}

	d := GovernorDecision{
		Enabled:      true,
		EffectiveCap: math.MaxInt,
		PausedTarget: 0,
		BurnWeekly:   weekly.burn,
		TargetWeekly: weekly.target,
		BurnFive:     fiveB.burn,
		TargetFive:   fiveB.target,
	}

	// (1e) Tighter-of: cap = min, pausedTarget = max, Binding = tighter cap.
	if weekly.ok {
		if weekly.cap < d.EffectiveCap {
			d.EffectiveCap = weekly.cap
			d.Binding = "weekly"
		}
		if weekly.pausedTarget > d.PausedTarget {
			d.PausedTarget = weekly.pausedTarget
		}
	}
	if fiveB.ok {
		if fiveB.cap < d.EffectiveCap {
			d.EffectiveCap = fiveB.cap
			d.Binding = "5h"
		}
		if fiveB.pausedTarget > d.PausedTarget {
			d.PausedTarget = fiveB.pausedTarget
		}
	}

	// OverPace = the binding bucket is burning above its target.
	switch d.Binding {
	case "weekly":
		d.OverPace = weekly.burn > weekly.target
	case "5h":
		d.OverPace = fiveB.burn > fiveB.target
	}

	// Projected end-of-week % at the current weekly burn rate.
	if weekly.ok {
		remaining := time.Unix(seven.ResetsAt, 0).Sub(now)
		if remaining < 0 {
			remaining = 0
		}
		d.ProjectedEndPct = seven.UsedPct + weekly.burn*hours(remaining)
	} else {
		d.ProjectedEndPct = seven.UsedPct
	}

	return d
}
