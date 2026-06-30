# {{illust:spray}} Memory

Every session starts fresh — a new worktree, a blank context. Left alone,
agents re-derive the same things over and over: how to build the repo, which
test invocation works, what a maintainer rejected last week. Orchid gives the
swarm a **shared, persistent memory** so that knowledge accumulates instead of
evaporating.

It's built on Claude's own auto-memory — the agent decides what's worth keeping
and writes it as plain markdown — wired so the whole swarm reads and writes one
durable, versioned store.

## How it works

- **Per target repo.** Each claude session's auto-memory is redirected (via the
  `CLAUDE_COWORK_MEMORY_PATH_OVERRIDE` env var the coordinator sets at spawn) into
  a folder for its target repo: `memory/<owner>/<repo>/`. So `denoland/deno` work
  accumulates separately from `denoland/fastwebsockets`, but notes can still
  cross-reference each other. (The override is claude-native; codex agents don't
  get it.)
- **Git-backed.** The store is a clone of a real repo (by default the inbox
  repo, on `main`, under a `memory/` subtree). A background loop commits new
  notes and `git pull --rebase`/pushes them every few minutes. So memory is
  durable, versioned, browsable on GitHub, and shared across every box in the
  swarm — pull brings other machines' notes in.
- **No conflicts by construction.** Memory shares no path with code, so the
  rebase against a moving `main` never collides; one serial committer per box
  avoids self-races.
- **Notes are markdown.** Each note has YAML frontmatter (`name`,
  `description`, `metadata.type`) and a body. Agents link related notes with
  `[[wikilinks]]`; a per-folder `MEMORY.md` acts as the index.

The agents do the writing. You curate by reading, and by letting good notes
ride along in the PRs that produced them.

Browse the store on GitHub — it's a real repo subtree (`<dir>/<owner>/<repo>/…`),
versioned and diffable like any other code. Each folder's `MEMORY.md` is its index.

## Configuration

Enable it with a `memory` block in the config:

```json
"memory": {
  "enabled": true,
  "repo": "denoland/divybot",
  "branch": "main",
  "dir": "memory",
  "interval": "5m"
}
```

| Field | Default | Meaning |
|-------|---------|---------|
| `enabled` | `false` | Turn the git-backed memory store on. |
| `repo` | `inbox` | Repo that holds the memory subtree. |
| `branch` | `main` | Branch the store lives on. |
| `dir` | `memory` | Path within the repo for notes (`<dir>/<owner>/<repo>/…`). |
| `interval` | `5m` | How often the commit/pull-rebase/push loop runs. |

A clean separation worth considering: point `repo` at a dedicated operational repo
(separate from your source), so memory commits don't interleave with code history.
