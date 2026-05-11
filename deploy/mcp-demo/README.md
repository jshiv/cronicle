# MCP + skills + scratch (one task, all three features)

A minimal working example of cronicle's three agent-side composables
in one task:

| Feature | What it does here |
|---------|-------------------|
| `mcp`   | Launches `@modelcontextprotocol/server-filesystem` as a subprocess; agent gets `fs__list_directory`, `fs__read_text_file`, etc. scoped to `data/` |
| `skills` | `file-summarizer/SKILL.md` defines the one-paragraph digest style; loaded on demand via `load_skill` (progressive disclosure) |
| `${scratch}` | Schedule-scoped output dir the agent writes `SUMMARY.md` to |

No credentials. The filesystem MCP server runs without auth and is
restricted to the path passed as its argument — here `${path}/data`,
the `data/` directory next to `cronicle.hcl`.

## Layout

```
deploy/mcp-demo/
├── cronicle.hcl
├── data/                          # what the MCP server can see
│   ├── architecture-note.md
│   ├── operator-checklist.md
│   └── release-notes.md
└── skills/
    └── file-summarizer/
        └── SKILL.md
```

## Prereqs

- **`ANTHROPIC_API_KEY`** in env (agent tasks need it).
- **Node 18+** with `npx` on PATH. `npx -y @modelcontextprotocol/server-filesystem`
  installs the server lazily on the first run; subsequent runs use the
  cached version. If you want to pre-warm: `npx -y @modelcontextprotocol/server-filesystem --version`.
- **cronicle v0.5.0+**.

## Run it

Foreground (single-process, in-memory queue, run completes when the
agent task completes):

```bash
cd deploy/mcp-demo
export ANTHROPIC_API_KEY=...

cronicle exec \
  --path ./cronicle.hcl \
  --schedule mcp_demo \
  --task summarize
```

You'll see the agent boot the MCP server, list and read files via
`fs__*` tools, and finally write to scratch via `text_editor`. The
final block looks like this:

```
agent run · schedule=mcp_demo · task=summarize · model=claude-haiku-4-5 · skills=[file-summarizer]
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

→ fs.list_directory: /Users/.../deploy/mcp-demo/data
← exit=0 12ms
→ fs.read_text_file: release-notes.md
← exit=0 8ms
→ fs.read_text_file: architecture-note.md
← exit=0 7ms
→ fs.read_text_file: operator-checklist.md
← exit=0 6ms
→ editor: create .cronicle/scratch/mcp_demo/.../SUMMARY.md
← exit=0 4ms

SUMMARY WRITTEN.

[1234 in / 280 out tokens · $0.001823 · 4521ms · stop=end_turn]
```

The `SUMMARY.md` lands under `.cronicle/scratch/mcp_demo/<run-id>/`.

## What's actually being demonstrated

- **MCP wiring is real.** The agent's tool universe is cronicle's
  built-in `text_editor` + `load_skill` PLUS the MCP server's tools
  (`fs__list_directory`, `fs__read_text_file`, `fs__read_multiple_files`,
  `fs__search_files`, `fs__write_file`, `fs__create_directory`,
  `fs__directory_tree`, etc.). The server is launched at run time, the
  schemas are translated from MCP's JSON-Schema into Anthropic's
  schema format, and the agent calls them like any built-in tool.
- **Skills load on demand.** The frontmatter (name + description)
  is appended to the system prompt up front. The body (the
  `# When to use`, `## How to summarize`, `## Output format` sections
  of `file-summarizer/SKILL.md`) only loads when the agent calls
  `load_skill`. That's the progressive-disclosure pattern from
  Anthropic's Skills standard.
- **`${scratch}` resolves at execution time.** The HCL `prompt`
  contains the literal `${scratch}`; cronicle substitutes it with
  the per-run directory before sending the request. The
  schedule-scoped scratch dir is created automatically; if you add
  multiple tasks to this schedule, they all share the same dir.

## Comparing with the other examples

- [**`deploy/distributed/`**](../distributed/README.md) — same control
  surface (cancel, retry, resume, workers), but pure shell tasks.
  No agents, no MCP. Use this for "how does the broker-less queue
  work?"
- [**`deploy/daily-report/`**](../daily-report/README.md) — fully
  agentic workflow (4 crawler agents + a composer), uses `${scratch}`
  and `skills` extensively, references MCP servers in commented
  blocks (slack, gmail, discord — they need credentials so they're
  documentation-only). Use this for "how do I compose a multi-agent
  pipeline?"
- **This example** — the smallest possible working demonstration of
  a *live* MCP server. Start here, then layer it into one of the
  others.

## Running this in distributed mode

The HCL is the same. Add `--listen + --listen-token` to make the
producer expose the API, run a worker, and trigger the schedule via
the listener:

```bash
# terminal 1: producer
export ANTHROPIC_API_KEY=...
export CRONICLE_LISTEN_TOKEN=demo-token
cronicle run \
  --path ./cronicle.hcl \
  --listen :8765 --listen-token "$CRONICLE_LISTEN_TOKEN" \
  --worker=false

# terminal 2: worker (also needs ANTHROPIC_API_KEY for the agent)
export ANTHROPIC_API_KEY=...
cronicle worker \
  --path ./ \
  --producer http://localhost:8765 \
  --producer-token "$CRONICLE_LISTEN_TOKEN" \
  --worker-id w1

# terminal 3: fire the trigger
curl -X POST -H "Authorization: Bearer $CRONICLE_LISTEN_TOKEN" \
  http://localhost:8765/v1/schedules/mcp_demo/trigger
```

The worker boots the MCP server in its own process; the producer
just dispatches the job.
