version = ""
git     = ""

schedule "example" {
  cron       = "@every 5s"
  repo       = ""
  start_date = ""
  end_date   = ""

  task "hello" {
    command = ["/bin/echo", "Hello World", "--date=${date}"]
    depends = null
    repo    = ""
    branch  = ""
    commit  = ""
  }
}

repos = null
