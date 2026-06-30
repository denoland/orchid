# {{illust:config-knot}} Throttling & pacing

Subscription plans meter usage in two rolling windows — a **5-hour** window and a
**weekly** window, each reported as a used-percent with a reset time. An unthrottled
swarm of 30+ concurrent agents burns the weekly window in a couple of days, then
everything stalls until the reset. The **governor** is how divybot spends the budget
*evenly* — landing near the ceiling at reset instead of exhausting it early — while
keeping the highest-value work running.

The governor reads each account's **real usage meter** (claude's rate-limit headers,
codex's rollout token_count) on a sample interval and runs a burn-rate adaptive cap
**per account** — claude and codex pace independently, so a hot claude window never
throttles codex. It lives in the `governor` block of the config and is slim by
design: pause above the ceiling, throttle toward the floor within the slack band,
otherwise admit up to `max_active`. It fails open — a read error never wedges the
swarm.

## The hard floor

At any moment each account is in one of: **allow**, **throttle**, or **pause**. The
pace target is linear — a fraction `f` through a window should be ≈ `f × 100` used —
checked against **both** windows. New work hard-pauses when an account is at/above
`weekly_ceiling_pct` of its binding window.

Pacing both windows matters when the short window is the binding constraint: a fleet
sharing one account can drain the 5h window to 100% in the first hour while the
weekly window still sits near 10%. Tracking the *binding* window spreads the budget
across the whole 5 hours.

The governor only ever **blocks new admissions** — it never touches in-flight agents.
Neither layer can un-spend what's already gone; the cap bounds inflow.

## The adaptive cap

Instead of running flat-out until it slams into the ceiling, the governor measures
the **actual burn rate** (from quota samples over a lookback window) against the rate
needed to land at the ceiling by reset, and sets an **adaptive concurrency cap**.
Ahead of pace → lower the cap (down to `min_active`); behind → raise it (up to
`max_active`). It always tracks the binding window — whichever of 5h or weekly is
tighter right now.

The cap is driven by the **burn rate**, not the absolute position. The target rate is
recomputed every tick from the live used% and remaining time, so running hot early
shrinks the budget and drops the target (and the cap) further — position error
corrects itself the moment there's burn to measure. The flip side: when burn falls to
~0 (agents idle at their prompts) the governor has nothing to act on, so an account
can sit *above* the linear line yet stable — over pace, but not climbing. The hard
floor keeps new burn out meanwhile. Below the engage floor (very low used%) the cap
is relaxed so a near-idle account isn't needlessly throttled.

## Config

The governor block, with defaults:

```json
"governor": {
  "enabled": true,
  "weekly_ceiling_pct": 92,
  "slack_pct": 8,
  "max_active": 16,
  "min_active": 1,
  "rate_window": "3h",
  "five_rate_window": "45m",
  "sample_interval": "90s"
}
```

- `enabled` — turn the governor on. Off → the swarm runs flat-out, bounded only by
  each host's `capacity`.
- `weekly_ceiling_pct` — hard-pause new work at/above this used%.
- `slack_pct` — floor the cap to `min_active` within this band of the ceiling.
- `max_active` — ceiling for the adaptive per-account cap (raise to scale up).
- `min_active` — never fully stall while there's budget left.
- `rate_window` / `five_rate_window` — burn-rate lookbacks for the weekly / 5h bucket.
- `sample_interval` — how often the live meter is read.

Per-host `capacity` is the real per-box admission ceiling; the governor cap bounds
total inflow per account on top of it.

## Priority

Admission order is set per **target**, not per issue: `priority: N` in a target block
(higher admits first) floats a small high-value target ahead of a large backlog when
spawns are serialized. Within a priority, the lowest issue number goes first.

## A note on codex `prolite`

Credit-gated plans don't pace on a clean percentage. Codex `prolite` reports real
percentages early in a 5h window, then once the budget is spent flips to a
credit-exhausted reading with `null` buckets and `balance = 0`. divybot ignores the
null reading (so a stale blank can't wipe good data), which means the governor
**freezes at the last percentage it saw** and can't see the real "out of credits"
block — the agent's own `try again at HH:MM` message is the true signal. For such
accounts the **5h window is the binding constraint** (weekly can sit near 10% while
the 5h is exhausted), so size codex `capacity` to the 5h budget, not the weekly
headroom.
