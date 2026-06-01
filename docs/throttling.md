# {{illust:config-knot}} Throttling & pacing

Subscription plans meter usage in two rolling windows — a **5-hour** window and a
**weekly** window, each reported as a used-percent with a reset time. An
unthrottled swarm of 30+ concurrent sessions burns the weekly window in a couple
of days, then everything stalls until the reset. Throttling is how orchid spends
the budget *evenly* — landing near the ceiling at reset instead of exhausting it
early — while keeping the highest-value work running.

It's three layers, from a hard floor to proactive shaping. All live in one
`throttle` block inside `orchestrator`, and each account paces independently.

## 1. The throttle gate (hard floor)

The binary safety net. At any moment each account is in one of: **allow**,
**throttle**, **pause (5h)**, or **pause (week)**. The pace target is linear — a
fraction `f` through a window should be ≈ `f × 100` used — and is checked against
**both** windows: run ahead of *either* the weekly or the 5-hour line (plus a
slack allowance) and new work is blocked; get near a ceiling and work is paused
outright.

- `weekly_ceiling_pct` (default **92**) — don't blow past this % of the weekly
  window by its reset.
- `slack_pct` (**8**) — how far above linear pace you may run before throttling.
  Applies to both the weekly and the 5h linear-pace checks.
- `five_hour_pause_pct` (**85**) — hard-*pause* new work when the 5h window is
  this hot (a harder stop than the 5h linear throttle above, for short bursts).

Pacing both windows matters when the short window is the binding constraint: a
fleet sharing one account can drain the 5h window to 100% in the first hour while
the weekly window still sits near 10%. The 5h linear check spreads that budget
across the whole 5 hours.

This gate only ever *blocks new admissions* (and optionally pokes); it never
touches in-flight sessions — that's what duty-cycling (§3) is for.

## 2. The governor (adaptive cap)

`governor_enabled = true` turns the hard floor into proactive shaping. Instead of
running flat-out until it slams into the ceiling, the governor measures the
**actual burn rate** (from quota samples over a lookback window) against the rate
needed to land at the ceiling by reset, and sets an **adaptive concurrency cap**.
Ahead of pace → lower the cap; behind → raise it (up to `max_active`). It always
tracks the *binding* window — whichever of 5h or weekly is the tighter
constraint right now — and reports a **projected end-of-week %** so you can see
where you'll land.

- `target_ceiling_pct` (default = `weekly_ceiling_pct`, **92**) — the number it
  aims to land at.
- `max_active` (**8**) — hard ceiling for the adaptive cap.
- `min_active` (**1**) — never fully stall while there's budget left.
- `rate_window` (**3h**) / `five_rate_window` (**45m**) — burn-rate lookbacks.
- `sample_interval` (**90s**) — how often quota is sampled.

The cap is driven by the **burn rate**, not the absolute position. The target
rate is recomputed every tick from the live used% and remaining time, so running
hot early shrinks the budget and drops the target (and the cap) further — position
error corrects itself the moment there's burn to measure. The flip side: when
burn falls to ~0 (sessions idle at their prompts) the governor has nothing to act
on, so an account can sit *above* the linear line yet stable — over pace, but not
climbing, and it won't blow the window unless burn resumes. The gate (§1) keeps
new burn out meanwhile; neither layer can un-spend what's already gone.

## 3. Duty-cycling (load shedding)

`duty_cycle = true`. When the burn needs to drop *below* what the sessions already
running will spend, waiting for them to finish is too slow. Duty-cycling **kills**
the lowest-priority running sessions (process gone, RAM freed, token burn stops —
the worktree is kept) and brings them back later with `--resume`, reloading the
conversation from the transcript. Lowest-priority, most-recently-started sessions
are shed first; highest-priority, longest-paused resume first. A session resumes
once its account is no longer over pace, or after `max_pause_dur` (default **20m**)
force-resumes it so nothing starves. Only sessions with an open PR are shed —
`--resume` recovery needs one.

Because the cap (§2) bounds *inflow* but per-VM `capacity` is the real admission
ceiling, duty-cycling is the only thing that reduces *in-flight* burn. During a
burst the gate blocks new spawns while the running fleet keeps burning, so used%
can overshoot the line before the burn-rate estimate is confident enough to shed.

## Priority

Order the queue with `priority = N` in an issue's toml frontmatter (higher =
sooner). Priority drives **both** admission (high-priority issues spawn first)
and duty-cycle (low-priority sessions are shed first, high-priority resumed
first). `default_priority` sets the value for issues with no frontmatter
(default **0**). Re-editing the frontmatter retunes the order live, no restart.

```toml
priority = 100
```

…at the top of an issue body floats a security fix ahead of routine work.

## Pokes & token-saving

Every review comment or CI result is relayed by **poking** the session — and
each poke is a full turn that re-reads the entire conversation context. Two knobs
keep that cheap:

- `poke_min_interval` (**5m**) — debounce pokes so a chatty PR doesn't re-read a
  huge context every few seconds.
- `throttle_pokes` — let the hard-pause gate also defer pokes when paused.

Long sessions are the dominant token sink: every turn re-reads the whole context
(`cache_read`), so a session whose context has grown toward the 1M window re-reads
~1M tokens per turn. Orchid resets these when idle and over budget — `/clear` plus
a concise re-orient — capped by:

- `max_context_tokens` (**500000**, 0 = off) — `/clear` when context exceeds this.
- `max_session_age` (**12h**) — `/clear` when a session is older than this.

## A note on codex `prolite`

Credit-gated plans don't pace on a clean percentage. Codex `prolite` reports real
percentages early in a 5h window, then once the budget is spent flips to a
credit-exhausted reading with `null` buckets and `balance = 0`. Orchid ignores
the null reading (so a stale blank can't wipe good data), which means the
dashboard **freezes at the last percentage it saw** and can't show the real "out
of credits" block — the session's own `try again at HH:MM` message is the true
signal. For such accounts the **5h window is the binding constraint** (weekly can
sit near 10% while the 5h is exhausted), so size codex `capacity` to the 5h
budget, not the weekly headroom.

## Reading it on the dashboard

The **Analytics → Accounts** cards show each account's 5h + weekly bars (amber
when over pace, red when paused), the measured **burn vs target** rate, the
adaptive **cap** with active / paused counts, and the **projected end-of-week %**
against the ceiling. An account can read amber/over-pace yet be stable at zero
burn (§2), and codex `prolite` percentages go stale once credit-gated (above).

## Full config

```hcl
orchestrator {
  # …
  throttle {
    enabled            = true
    governor_enabled   = true
    duty_cycle         = true

    weekly_ceiling_pct = 92
    slack_pct          = 8
    five_hour_pause_pct = 85

    max_active         = 20
    min_active         = 1
    rate_window        = "3h"
    five_rate_window   = "45m"
    sample_interval    = "90s"
    max_pause_dur      = "20m"
    default_priority   = 0

    poke_min_interval  = "15m"
    max_context_tokens = 500000
    max_session_age    = "12h"
  }
}
```

Everything is optional: with no `throttle` block the swarm runs flat-out; with
`enabled` but `governor_enabled = false` you get just the reactive hard floor;
with `governor_enabled` but `duty_cycle = false` the cap shapes inflow but never
sheds in-flight work.
