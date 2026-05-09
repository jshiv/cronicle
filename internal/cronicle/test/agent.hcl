schedule "review" {
  cron = "@every 1h"

  task "summarize" {
    agent {
      prompt     = "Summarize what changed today: ${date}"
      model      = "claude-opus-4-7"
      system     = "You are a concise release-notes writer."
      max_tokens = 2048
      budget_usd = 0.50
    }
  }
}
