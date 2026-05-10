// Distributed-mode demo. Three short ETL-style schedules that fire on
// the same minute boundary, plus one manually-triggered long-running
// schedule for the cancel/retry/resume walkthrough.
//
// With 2+ workers running, the three ETLs fan out — each worker
// claims a different one. With a single worker, they execute serially
// in claim order. Either way the projection at /v1/runs reflects who
// ran what.
heartbeat = "@every 30s"

schedule "etl_users" {
  cron = "@every 30s"
  task "run" {
    command = ["/bin/sh", "-c", "echo 'etl: users'; sleep 3; echo done"]
    depends = null
    env     = null
  }
}

schedule "etl_orders" {
  cron = "@every 30s"
  task "run" {
    command = ["/bin/sh", "-c", "echo 'etl: orders'; sleep 3; echo done"]
    depends = null
    env     = null
  }
}

schedule "etl_events" {
  cron = "@every 30s"
  task "run" {
    command = ["/bin/sh", "-c", "echo 'etl: events'; sleep 3; echo done"]
    depends = null
    env     = null
  }
}

// Multi-step pipeline for cancel + resume demos. `extract` succeeds
// quickly, `transform` is the slow step (cancel here), `load` is the
// downstream that resume picks up.
schedule "report_pipeline" {
  cron = ""

  task "extract" {
    command = ["/bin/sh", "-c", "echo extracting; sleep 1; echo done"]
    depends = null
    env     = null
  }

  task "transform" {
    command = ["/bin/sh", "-c", "echo transforming; sleep 30; echo done"]
    depends = ["extract"]
    env     = null
  }

  task "load" {
    command = ["/bin/sh", "-c", "echo loading; sleep 1; echo done"]
    depends = ["transform"]
    env     = null
  }
}
