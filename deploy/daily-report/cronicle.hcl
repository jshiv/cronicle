// Daily report demo: 4 crawler agents (Slack, Discord, email, code) fan
// out into a shared scratch dir; one composer agent fans in, reads them,
// and writes a unified REPORT.md (and optionally posts to Slack via MCP).
//
// This file demonstrates the cross-task context pattern via ${scratch}.
// In a real deployment:
//   - Each crawler's `mcp` block points at the source-of-truth integration:
//       slack:   @modelcontextprotocol/server-slack
//       discord: @modelcontextprotocol/server-discord (or a community one)
//       email:   @modelcontextprotocol/server-gmail (or imap variant)
//       code:    use the built-in git tool against a `repo` block
//   - Each `env` whitelists only the credentials the server needs.
//   - The composer's mcp "slack" reuses the slack MCP for posting.
//
// For this demo the crawlers produce mock data (they just write a fixed
// template to ${scratch}/<source>.md) so the pattern can be smoke-tested
// without real credentials. Swap in real MCP servers + real prompts to
// make it production.

schedule "daily_report" {
  cron     = "0 9 * * *"   // every day at 09:00 local
  timezone = "America/Los_Angeles"

  task "crawl_slack" {
    agent {
      prompt = <<-EOT
        Today is ${date}. (Production: query the slack MCP for messages in
        channels #engineering, #incidents, #product over the last 24h.
        For this demo, compose 3 plausible bullet points.)

        Write the result to ${scratch}/slack.md as markdown bullets.
        Reply: SLACK CRAWLED.
      EOT
      model      = "claude-haiku-4-5"
      tools      = ["text_editor"]
      max_turns  = 5
      wallclock  = "1m"
      budget_usd = 0.05
      max_tokens = 800

      // Production:
      // mcp "slack" {
      //   command = ["npx", "-y", "@modelcontextprotocol/server-slack"]
      //   env     = ["SLACK_BOT_TOKEN", "SLACK_TEAM_ID"]
      // }
    }
  }

  task "crawl_discord" {
    agent {
      prompt = <<-EOT
        Today is ${date}. (Production: query a discord MCP for the team
        server's channel digests over the last 24h. For this demo,
        compose 2 plausible bullet points.)

        Write the result to ${scratch}/discord.md as markdown bullets.
        Reply: DISCORD CRAWLED.
      EOT
      model      = "claude-haiku-4-5"
      tools      = ["text_editor"]
      max_turns  = 5
      wallclock  = "1m"
      budget_usd = 0.05
      max_tokens = 800
    }
  }

  task "crawl_email" {
    agent {
      prompt = <<-EOT
        Today is ${date}. (Production: query the gmail MCP for unread
        threads in label "team-updates" since yesterday. For this demo,
        compose 3 plausible bullet points.)

        Write the result to ${scratch}/email.md as markdown bullets.
        Reply: EMAIL CRAWLED.
      EOT
      model      = "claude-haiku-4-5"
      tools      = ["text_editor"]
      max_turns  = 5
      wallclock  = "1m"
      budget_usd = 0.05
      max_tokens = 800
    }
  }

  task "crawl_code" {
    // Real code crawl: clone the team's repo, summarize commits via the
    // built-in git tool. We point at jshiv/cronicle as a stand-in so the
    // demo runs against a real repo end-to-end.
    repo {
      url = "https://github.com/jshiv/cronicle.git"
    }
    agent {
      prompt = <<-EOT
        Today is ${date}. Use the built-in git tool to look at commits
        merged in the last 7 days. Pick the 3 most interesting and
        summarize each in one sentence focused on user-visible impact.

        Write the result to ${scratch}/code.md as markdown bullets.
        Reply: CODE CRAWLED.
      EOT
      model      = "claude-haiku-4-5"
      tools      = ["git", "text_editor"]
      max_turns  = 12
      wallclock  = "2m"
      budget_usd = 0.10
      max_tokens = 1500
    }
  }

  task "compose" {
    depends = ["crawl_slack", "crawl_discord", "crawl_email", "crawl_code"]
    agent {
      prompt = <<-EOT
        Compose today's daily team report. Today is ${date}. Read the
        crawler outputs in ${scratch}/ and follow the daily-composer
        skill's instructions.

        (Production: post the rendered REPORT.md to #daily-report via
        the slack MCP. For this demo, just leave the file in scratch —
        the orchestrator will pick it up.)
      EOT
      model      = "claude-haiku-4-5"
      tools      = ["text_editor", "bash"]
      skills     = ["skills/daily-composer/SKILL.md"]
      max_turns  = 10
      wallclock  = "2m"
      budget_usd = 0.10
      max_tokens = 2500

      // Production:
      // mcp "slack" {
      //   command = ["npx", "-y", "@modelcontextprotocol/server-slack"]
      //   env     = ["SLACK_BOT_TOKEN", "SLACK_TEAM_ID"]
      // }
    }
  }
}
