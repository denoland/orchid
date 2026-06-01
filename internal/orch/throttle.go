package orch

import (
	"fmt"
	"time"
)

// weekWindow is the length of Claude's 7-day subscription bucket. All
// pacing math derives weekStart from the server-authoritative reset
// timestamp minus this window, so the throttle is self-anchoring and
// minimally dependent on the local clock.
const weekWindow = 7 * 24 * time.Hour

// fiveHourWindow is the length of the short (primary) rate-limit bucket that
// both Claude and codex report. Used to pace the 5h window linearly, the same
// way weekWindow paces the weekly bucket — so a fleet sharing one account
// (e.g. 18 codex prolite sessions) can't drain the 5h budget in the first hour
// and then idle until reset.
const fiveHourWindow = 5 * time.Hour

// Throttle defaults, applied lazily inside ThrottleDecide via
// withDefaults so ThrottleDecide stays a pure function and there is no
// startup normalization step to forget to call.
const (
	defaultWeeklyCeilingPct = 92.0
	defaultSlackPct         = 8.0
	defaultFiveHourPausePct = 85.0
	defaultStaleAfter       = 30 * time.Minute
)

// ThrottleBlock paces consumption of Claude's 7-day subscription bucket
// so it lasts the whole week instead of being burned in a day or two.
// A nil block (unconfigured) or Enabled=false means disabled, which is
// today's behavior exactly (fail-open). All non-Enabled fields are
// optional and default lazily.
type ThrottleBlock struct {
	Enabled          bool    `hcl:"enabled,optional"             json:"enabled,omitempty"`
	WeeklyCeilingPct float64 `hcl:"weekly_ceiling_pct,optional"  json:"weekly_ceiling_pct,omitempty"`
	SlackPct         float64 `hcl:"slack_pct,optional"           json:"slack_pct,omitempty"`
	FiveHourPausePct float64 `hcl:"five_hour_pause_pct,optional" json:"five_hour_pause_pct,omitempty"`
	StaleAfter       string  `hcl:"stale_after,optional"         json:"stale_after,omitempty"`
	ThrottlePokes    bool    `hcl:"throttle_pokes,optional"      json:"throttle_pokes,omitempty"`

	// Proactive pacing governor knobs (see governor.go). All optional;
	// GovernorEnabled=false (the zero value) means the governor is off and
	// behavior is exactly today's reactive throttle (fail-open). Defaults are
	// applied lazily in withDefaults; duration strings are parsed by the
	// *Dur() helpers below, all of which fall back to a sane default on a bad
	// or empty string (never fatal).
	GovernorEnabled  bool    `hcl:"governor_enabled,optional"   json:"governor_enabled,omitempty"`
	TargetCeilingPct float64 `hcl:"target_ceiling_pct,optional" json:"target_ceiling_pct,omitempty"` // default = WeeklyCeilingPct (92)
	MaxActive        int     `hcl:"max_active,optional"         json:"max_active,omitempty"`         // hard ceiling for the adaptive cap; default 8
	MinActive        int     `hcl:"min_active,optional"         json:"min_active,omitempty"`         // never fully stall under budget; default 1
	DutyCycle        bool    `hcl:"duty_cycle,optional"         json:"duty_cycle,omitempty"`         // enable SIGSTOP/SIGCONT duty-cycling
	SampleInterval   string  `hcl:"sample_interval,optional"    json:"sample_interval,omitempty"`    // quota sampling cadence; default 90s
	RateWindow       string  `hcl:"rate_window,optional"        json:"rate_window,omitempty"`        // weekly burn-rate lookback; default 3h
	FiveRateWindow   string  `hcl:"five_rate_window,optional"   json:"five_rate_window,omitempty"`   // 5h burn-rate lookback; default 45m
	MaxPauseDur      string  `hcl:"max_pause_dur,optional"      json:"max_pause_dur,omitempty"`      // force-resume after this; default 20m
	DefaultPriority  int     `hcl:"default_priority,optional"   json:"default_priority,omitempty"`   // priority for issues with no frontmatter; default 0

	// Token-saving knobs (see tick.go). The dominant token sink is cache_read:
	// every turn re-reads the whole conversation context, so a long-lived
	// session whose context has grown re-reads a huge context on every turn.
	// These cap that. Defaults applied lazily; a session is cycled (/clear +
	// re-orient) when idle and over either limit. PokeMinInterval debounces the
	// review/CI pokes (each poke is a turn = a full context re-read).
	PokeMinInterval  string `hcl:"poke_min_interval,optional"   json:"poke_min_interval,omitempty"`  // min gap between pokes; default 5m
	MaxContextTokens int    `hcl:"max_context_tokens,optional"  json:"max_context_tokens,omitempty"` // /clear when ctx exceeds this; default 500000 (0=off)
	MaxSessionAge    string `hcl:"max_session_age,optional"     json:"max_session_age,omitempty"`    // /clear when session older than this; default 12h
}

// Governor default durations + numeric defaults. Kept next to the throttle
// defaults so all policy defaults live in one place.
const (
	defaultGovMaxActive      = 8
	defaultGovMinActive      = 1
	defaultGovSampleInterval = 90 * time.Second
	defaultGovRateWindow     = 3 * time.Hour
	defaultGovFiveRateWindow = 45 * time.Minute
	defaultGovMaxPauseDur    = 20 * time.Minute
	defaultPokeMinInterval   = 5 * time.Minute
	defaultMaxContextTokens  = 500000
	defaultMaxSessionAge     = 12 * time.Hour
)

func (t ThrottleBlock) pokeMinDur() time.Duration {
	return durOr(t.PokeMinInterval, defaultPokeMinInterval)
}
func (t ThrottleBlock) maxSessionAgeDur() time.Duration {
	return durOr(t.MaxSessionAge, defaultMaxSessionAge)
}

// maxCtxTokens returns the context-size ceiling above which a session is
// cycled. Default 500000; a negative value disables context-based cycling.
func (t ThrottleBlock) maxCtxTokens() int {
	if t.MaxContextTokens < 0 {
		return 0 // disabled
	}
	if t.MaxContextTokens == 0 {
		return defaultMaxContextTokens
	}
	return t.MaxContextTokens
}

// targetCeiling returns the used% the governor paces toward (lands ~here at
// reset). Defaults to WeeklyCeilingPct so the governor and the hard gate share
// the same 92% / 8%-reserve line. Call on a withDefaults'd receiver.
func (t ThrottleBlock) targetCeiling() float64 {
	if t.TargetCeilingPct > 0 {
		return t.TargetCeilingPct
	}
	return t.WeeklyCeilingPct
}

// durOr parses a duration string, falling back to def on empty/invalid input.
func durOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

func (t ThrottleBlock) sampleIntervalDur() time.Duration {
	return durOr(t.SampleInterval, defaultGovSampleInterval)
}
func (t ThrottleBlock) rateWindowDur() time.Duration {
	return durOr(t.RateWindow, defaultGovRateWindow)
}
func (t ThrottleBlock) fiveRateWindowDur() time.Duration {
	return durOr(t.FiveRateWindow, defaultGovFiveRateWindow)
}
func (t ThrottleBlock) maxPauseDur() time.Duration {
	return durOr(t.MaxPauseDur, defaultGovMaxPauseDur)
}

// withDefaults returns a value copy with zero-valued fields filled. Only
// sensible when the receiver is non-nil; ThrottleDecide calls it after
// the nil/!Enabled guard so the zero ThrottleBlock never leaks defaults.
func (t ThrottleBlock) withDefaults() ThrottleBlock {
	if t.WeeklyCeilingPct <= 0 {
		t.WeeklyCeilingPct = defaultWeeklyCeilingPct
	}
	if t.SlackPct <= 0 {
		t.SlackPct = defaultSlackPct
	}
	if t.FiveHourPausePct <= 0 {
		t.FiveHourPausePct = defaultFiveHourPausePct
	}
	if t.StaleAfter == "" {
		t.StaleAfter = defaultStaleAfter.String()
	}
	// Governor defaults. TargetCeilingPct is left as the explicit accessor
	// (targetCeiling) since its fallback is WeeklyCeilingPct, which is itself
	// only defaulted above.
	if t.MaxActive <= 0 {
		t.MaxActive = defaultGovMaxActive
	}
	if t.MinActive <= 0 {
		t.MinActive = defaultGovMinActive
	}
	return t
}

// staleDur parses StaleAfter, falling back to the default. Never fatal —
// a bad duration string just means we use 30m.
func (t ThrottleBlock) staleDur() time.Duration {
	if d, err := time.ParseDuration(t.StaleAfter); err == nil && d > 0 {
		return d
	}
	return defaultStaleAfter
}

// ThrottleMode is the per-tick verdict. The zero value (ModeAllow) means
// behave exactly like today.
type ThrottleMode int

const (
	ModeAllow         ThrottleMode = iota // fail-open / under budget: behave as today
	ModeThrottle                          // soft brake: block NEW oneshot spawns + cron fires this tick
	ModePauseFiveHour                     // 5h burst guard: block new spawns until five.ResetsAt
	ModePauseWeek                         // hard ceiling: block all new work until seven.ResetsAt
)

func (m ThrottleMode) String() string {
	switch m {
	case ModeThrottle:
		return "throttle"
	case ModePauseFiveHour:
		return "pause_5h"
	case ModePauseWeek:
		return "pause_week"
	default:
		return "allow"
	}
}

// ThrottleDecision is the result of one pacing evaluation. Its zero value
// is ModeAllow with no pause window.
type ThrottleDecision struct {
	Mode             ThrottleMode
	Reason           string    // short human string for logs + dashboard tooltip
	Until            time.Time // zero unless a Pause mode; the binding ResetsAt
	TargetPct        float64   // elapsedFrac*100 (linear pace target), 0 when not computed
	UsedPct          float64   // seven.UsedPct echoed for the dashboard
	ProjectedExhaust time.Time // weekStart + elapsed*100/usedPct; zero when usedPct==0 or fail-open
}

// BlocksNewWork is the single predicate the scheduler acts on for new
// oneshot spawns and cron fires.
func (d ThrottleDecision) BlocksNewWork() bool { return d.Mode != ModeAllow }

// BlocksPokes is opt-in (default off): only the two hard Pause modes can
// ever gate pokes, and only when the operator set throttle_pokes=true.
func (d ThrottleDecision) BlocksPokes(cfg *ThrottleBlock) bool {
	return cfg != nil && cfg.ThrottlePokes &&
		(d.Mode == ModePauseWeek || d.Mode == ModePauseFiveHour)
}

// ThrottleDecide is the canonical pure pacing function. No globals, no
// locks, no I/O — fully deterministic in its arguments and the unit-test
// target. It fails open (ModeAllow) on any unreadable/disabled/stale
// input so a throttle bug can never deadlock all work.
func ThrottleDecide(now time.Time, five, seven RateLimit, ok bool, cfg *ThrottleBlock) ThrottleDecision {
	// Fail-open guards: any of these and we behave exactly like today.
	if cfg == nil || !cfg.Enabled || !ok || seven.ResetsAt == 0 {
		return ThrottleDecision{}
	}
	c := cfg.withDefaults()

	reset := time.Unix(seven.ResetsAt, 0)
	// Stale: the 7-day window already reset more than StaleAfter ago, so
	// this reading is from a finished window. A reset slightly in the
	// past but within StaleAfter is NOT stale — the elapsed clamp below
	// handles it.
	if now.Sub(reset) > c.staleDur() {
		return ThrottleDecision{}
	}

	weekStart := reset.Add(-weekWindow)
	elapsed := now.Sub(weekStart)
	elapsedFrac := float64(elapsed) / float64(weekWindow)
	if elapsedFrac < 0 {
		elapsedFrac = 0
	} else if elapsedFrac > 1 {
		elapsedFrac = 1
	}
	targetPct := elapsedFrac * 100

	// Projected exhaustion: at the current burn rate, when does the 7-day
	// bucket reach 100%? Zero when usedPct==0 (cannot exhaust at 0% burn)
	// or when elapsed is non-positive. Computed for display + the
	// projected-exhaustion equivalence; the decision predicate below uses
	// the cleaner targetPct+slack comparison.
	var projectedExhaust time.Time
	if seven.UsedPct > 0 && elapsed > 0 {
		projectedExhaust = weekStart.Add(time.Duration(float64(elapsed) * 100 / seven.UsedPct))
	}

	d := ThrottleDecision{
		TargetPct:        targetPct,
		UsedPct:          seven.UsedPct,
		ProjectedExhaust: projectedExhaust,
	}

	// (1) Hard weekly ceiling — most conservative, checked first. Stops
	// opening NEW burn with headroom so the account is never hard-locked
	// mid-week. In-flight sessions are untouched (the scheduler only gates
	// the START of new work). Gated on reset.After(now) (symmetric with the
	// 5h guard below): a reset already in the past means the week rolled
	// over, so a frozen near-ceiling reading must not keep pausing new work
	// until it ages out via staleDur.
	if reset.After(now) && seven.UsedPct >= c.WeeklyCeilingPct {
		d.Mode = ModePauseWeek
		d.Until = reset
		d.Reason = fmt.Sprintf("weekly ceiling %.0f%% reached (used %.0f%%)", c.WeeklyCeilingPct, seven.UsedPct)
		return d
	}

	// (2) 5-hour burst guard — independent of the weekly math. A zero or
	// past five.ResetsAt disables the guard so a stale 5h reading never
	// wedges spawns.
	if five.ResetsAt != 0 {
		fiveReset := time.Unix(five.ResetsAt, 0)
		if fiveReset.After(now) && five.UsedPct >= c.FiveHourPausePct {
			d.Mode = ModePauseFiveHour
			d.Until = fiveReset
			d.Reason = fmt.Sprintf("5h burst guard: %.0f%% >= %.0f%%", five.UsedPct, c.FiveHourPausePct)
			return d
		}
	}

	// (3) Linear pace / projected exhaustion — only when there is burn to
	// pace. overPace at slack 0 is equivalent to projectedExhaust.Before(reset).
	if seven.UsedPct > 0 && seven.UsedPct > targetPct+c.SlackPct {
		d.Mode = ModeThrottle
		d.Reason = fmt.Sprintf("over pace: used %.0f%% > target %.0f%% + slack %.0f%%", seven.UsedPct, targetPct, c.SlackPct)
		return d
	}

	// (3b) 5-hour linear pace — the same linear pacer applied to the SHORT
	// window. The weekly bucket can sit at 10% while the 5h window blows to 100%
	// in the first hour (a fleet sharing one account drains the short bucket far
	// faster than the weekly one); this is the gate that was missing. Soft
	// (ModeThrottle): re-evaluated every tick and self-clears as the window
	// elapses, so it brakes new spawns just enough to make the 5h budget last
	// the 5 hours instead of pausing-until-reset. Independent of the 5h HARD
	// pause (five_hour_pause_pct), which the operator may disable. Gated on
	// fiveReset.After(now) so a stale/rolled-over reading never wedges spawns.
	if five.ResetsAt != 0 {
		fiveReset := time.Unix(five.ResetsAt, 0)
		if fiveReset.After(now) {
			fiveStart := fiveReset.Add(-fiveHourWindow)
			fFrac := float64(now.Sub(fiveStart)) / float64(fiveHourWindow)
			if fFrac < 0 {
				fFrac = 0
			} else if fFrac > 1 {
				fFrac = 1
			}
			fTarget := fFrac * 100
			if five.UsedPct > 0 && five.UsedPct > fTarget+c.SlackPct {
				d.Mode = ModeThrottle
				d.Reason = fmt.Sprintf("5h over pace: used %.0f%% > target %.0f%% + slack %.0f%%", five.UsedPct, fTarget, c.SlackPct)
				return d
			}
		}
	}

	// (4) Otherwise allow.
	return d
}

// currentThrottle is the thin daemon-facing wrapper, called once per tick
// (and reused by the /api/state builder via ThrottleDecide directly). It
// reads the most-recent live quota and fails open via latestQuota's ok
// flag — when no statusline has been seen the decision is ModeAllow.
func currentThrottle(cfg *Config, agent string, now time.Time) ThrottleDecision {
	five, seven, ok := latestQuota(agent)
	return ThrottleDecide(now, five, seven, ok, cfg.Orch.Throttle)
}
