repos    = null
timezone = ""

queue {
  type = ""
  addr = ""
}

schedule "foo" {
  cron       = "@every 5s"
  timezone   = ""
  start_date = ""
  end_date   = ""
  repo       = ""

  task "bar" {
    command = ["/bin/echo", "Hello World", "--date=${date}"]
    depends = null
    repo    = ""
    branch  = ""
    commit  = ""

    retry {
      count   = 0
      seconds = 0
      minutes = 0
      hours   = 0
    }
  }
}
