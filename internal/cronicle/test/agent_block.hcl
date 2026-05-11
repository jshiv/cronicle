// First-class schedule-level `agent "name" { ... }` block — sugar for
// the legacy `task "name" { agent { ... } }` form. Identical semantics
// post-parse; just a more readable surface for agent-heavy configs.
schedule "review" {
  cron = "@every 1h"

  agent "summarize" {
    prompt     = "Summarize what changed today: ${date}"
    model      = "claude-opus-4-7"
    system     = "You are a concise release-notes writer."
    max_tokens = 2048
    budget_usd = 0.50
  }
}
