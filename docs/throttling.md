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
fraction `f` through a window should be ≈ `f × ceiling` used. Run ahead of that
(plus a slack allowance) and new work is blocked; get near the ceiling and work
is paused outright.

- `weekly_ceiling_pct` (default **92**) — don't blow past this % of the weekly
  window by its reset.
- `slack_pct` (**8**) — how far above linear pace you may run before throttling.
- `five_hour_pause_pct` (**85**) — hard-pause new work when the 5h window is this
  hot (protects against short bursts).

This gate only ever *blocks new admissions* (and optionally pokes); it never
touches in-flight sessions.

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

## 3. Duty-cycling (load shedding)

`duty_cycle = true`. When the cap needs to drop *below* the number of sessions
already running, waiting for them to finish is too slow. Duty-cycling **freezes**
the lowest-priority running sessions with `SIGSTOP` (token burn stops, context is
preserved) and `SIGCONT`s them once there's headroom again. `max_pause_dur`
(default **20m**) force-resumes a paused session so nothing starves.

## Priority

Order the queue with `priority = N` in an issue's toml frontmatter (higher =
sooner). Priority drives **both** admission (high-priority issues spawn first)
and duty-cycle (low-priority sessions are frozen first, high-priority resumed
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

## Reading it on the dashboard

The **Analytics → Accounts** cards show each account's 5h + weekly bars (amber
when over pace, red when paused), the measured **burn vs target** rate, the
adaptive **cap** with active / paused counts, and the **projected end-of-week %**
against the ceiling.

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
`enabled` but `governor_enabled = false` you get just the reactive hard floor.
