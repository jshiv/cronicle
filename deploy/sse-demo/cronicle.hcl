// Multi-schedule SSE demo. Three schedules exercise the three filter
// scopes of /v1/runs/{id}/events, /v1/schedules/{name}/events, and
// /v1/events/stream:
//
//   heartbeat  — short shell task, fires every 10s. Drives the firehose
//                with regular background activity.
//   long-build — longer shell task (6s of stdout). Manual trigger. Good
//                for "subscribe before trigger" demos against the
//                per-schedule endpoint.
//   story      — agent task (text_delta streaming). Manual trigger.
//                Shows token-by-token wire frames.

schedule "heartbeat" {
  cron = "@every 10s"

  task "tick" {
    command = ["sh", "-c", "echo 'tick from heartbeat'; sleep 1; echo 'second line'"]
  }
}

schedule "long-build" {
  cron = "" // manual trigger only

  task "compile" {
    command = ["sh", "-c", "for s in 'fetching sources' 'compiling foo.c' 'compiling bar.c' 'linking' 'running tests' 'done'; do echo \"[step] $s\"; sleep 1; done"]
  }
}

schedule "story" {
  cron = "" // manual trigger only

  task "tell-a-story" {
    agent {
      prompt     = "Tell a very short, two-sentence story about an octopus debugging Go code."
      model      = "claude-haiku-4-5"
      max_turns  = 1
      max_tokens = 200
      budget_usd = 0.05
      wallclock  = "30s"
    }
  }
}
