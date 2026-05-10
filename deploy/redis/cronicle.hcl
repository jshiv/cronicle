// Distributed-mode demo: a producer (cronicle run --worker=false) pushes
// schedules onto Redis; a worker (cronicle worker) pops them and executes
// the tasks. Exercises shell + agent + skills + MCP all flowing through
// the wire format.
queue {
  type = "redis"
  addr = "127.0.0.1:6379"
}

// Plain shell task — baseline check that the queue plumbing still works.
schedule "ping" {
  cron = "@every 30s"
  task "echo" {
    command = ["/bin/echo", "hello from worker @ ${datetime}"]
  }
}

// Agent task carrying skills + MCP through the queue. The worker must
// have node/npx on PATH (for the filesystem MCP server) and the SKILL.md
// file at the relative path on its filesystem. ANTHROPIC_API_KEY must be
// in the worker's environment.
schedule "brief" {
  cron = "@every 5m"
  task "compose" {
    agent {
      prompt     = "List files in /tmp using fs.list_directory and report the count. End with: BRIEF DONE."
      model      = "claude-haiku-4-5"
      tools      = []
      max_turns  = 6
      wallclock  = "2m"
      budget_usd = 0.05
      max_tokens = 800

      mcp "fs" {
        command = ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
      }
    }
  }
}
