repos    = "null"
timezone  ""

schedule {
  cron        "@every 5s"
  timezone   = ""
  start_date = ""
  end_date   = ""

  task "bar" {
    command = ["/bin/echo", "Hello World", "--date=${date}"
    depends = [null]
  }
}
