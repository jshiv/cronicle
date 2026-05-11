// Live MCP demo. One agent task using cronicle's three composable
// features together:
//   - mcp     — a real Model Context Protocol server launched at run time
//   - skills  — Anthropic Agent Skill that tells the model how to work
//   - scratch — shared output dir written by the agent
//
// The MCP server is `@modelcontextprotocol/server-filesystem`, the
// official no-auth filesystem server. It's installed lazily by `npx -y`
// on the first run, so no manual setup. The agent gets list/read/search
// capabilities scoped to `data/` (passed as an arg to the server) and
// summarizes the files it finds.
//
// Prereqs:
//   - ANTHROPIC_API_KEY in env (`cronicle exec` and `cronicle run` both
//     need it for agent tasks)
//   - npx on PATH (Node 18+; comes with node, available in most
//     Linux/macOS environments)
//
// Run it foreground for the simplest demo:
//
//   export ANTHROPIC_API_KEY=...
//   cronicle exec --path ./cronicle.hcl --task summarize --schedule mcp_demo

schedule "mcp_demo" {
  cron = ""   // manual trigger only

  task "summarize" {
    agent {
      // The skill defines the *style* of the summary the agent
      // produces. Skills are discovered via their YAML frontmatter
      // (name + description) which is appended to the system prompt;
      // bodies load on demand via the load_skill tool.
      skills = ["skills/file-summarizer/SKILL.md"]

      prompt = <<-EOT
        You have read-only access to a `data/` directory via the
        `fs` MCP server. Use fs__list_directory and
        fs__read_text_file (or read_multiple_files) to inspect the
        files there.

        Follow the file-summarizer skill to produce a one-paragraph
        digest. Write the result to ${scratch}/SUMMARY.md using the
        text_editor tool. Reply: SUMMARY WRITTEN.
      EOT

      model = "claude-haiku-4-5"

      // Tools allowlist (Anthropic-defined tools only).
      //   - text_editor: writes the SUMMARY.md output to scratch
      //
      // load_skill is auto-registered when `skills` is non-empty;
      // it doesn't appear here. MCP tools auto-register too — they
      // arrive namespaced as <server-name>__<tool>, e.g.
      // fs__list_directory, fs__read_text_file.
      tools = ["text_editor"]

      // Live MCP server. cronicle launches it as a subprocess at run
      // time, speaks JSON-RPC over stdin/stdout, lists tools, and
      // exposes them to the agent as `fs__*`. The server shuts down
      // cleanly when the run ends.
      //
      // Note the second arg: the directory the server is allowed to
      // touch. Use `${path}` so it resolves to this schedule's working
      // directory (where data/ lives next to cronicle.hcl).
      mcp "fs" {
        command = ["npx", "-y", "@modelcontextprotocol/server-filesystem", "${path}/data"]
      }

      max_turns  = 8
      wallclock  = "2m"
      budget_usd = 0.05
      max_tokens = 2000
    }
  }
}
