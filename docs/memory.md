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

- **Per target repo.** Each session's auto-memory is redirected (via
  `CLAUDE_COWORK_MEMORY_PATH_OVERRIDE`) into a folder for its target repo:
  `memory/<owner>/<repo>/`. So `denoland/deno` work accumulates separately from
  `denoland/fastwebsockets`, but notes can still cross-reference each other.
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

## The Memory tab

The dashboard's **Memory** tab is a read-only browser over the store — think
`cgit` for the knowledge base:

- A **directory tree** of `owner/repo/note.md`, collapsible, with live search
  across every note.
- Click a note to render its markdown; the frontmatter shows as a metadata card
  and the file links out to its blob on GitHub.
- Click a folder for its `README` (the folder's `MEMORY.md`) or an
  auto-generated table of contents.
- **Backlinks / links** under each note show what references it and what it
  references — across repos.

## Configuration

Enable it with a `memory` block inside `orchestrator`:

```hcl
orchestrator {
  # …
  memory {
    enabled       = true
    repo          = "denoland/orchid"  # default: github.inbox_repo
    branch        = "main"
    dir           = "memory"           # subtree within the repo
    sync_interval = "5m"
  }
}
```

| Field | Default | Meaning |
|-------|---------|---------|
| `enabled` | `false` | Turn the git-backed memory store on. |
| `repo` | `inbox_repo` | Repo that holds the memory subtree. |
| `branch` | `main` | Branch the store lives on. |
| `dir` | `memory` | Path within the repo for notes (`<dir>/<owner>/<repo>/…`). |
| `sync_interval` | `5m` | How often the commit/pull-rebase/push loop runs. |

A clean separation worth considering: point `repo` at a dedicated operational
repo (separate from your source), so memory commits don't interleave with code
history. See [Configuration](/docs/configuration).
