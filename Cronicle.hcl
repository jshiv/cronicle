version = "0.0.1alpha"
git     = ""

schedule "schedule A" {
  cron       = "@every 2s"
  start_date = ""
  end_date   = ""

  owner {
    name  = "john"
    email = "smith@gmail.com"
  }

  task "task1" {
    command = ["/bin/echo", "Hello World --date=${date}"]
  }
  task "task2" {
    command = ["/bin/echo", "This is Task2"]
  }
}

schedule "schedule B" {
  cron       = "@every 5s"

  task "dice" {
    command = ["python", "-c", "import random; print(random.randint(1, 6))"]
  }
}
