# cronicle
git integrated workflow scheduler that provides a pull model for CI/CD and versioning on job execution.

Usage

The tool will use a cronicle.hcl file to maintain `schedule as code`.
A cronicle schedule could look like:
```hcl
#cronicle.hcl 
version = "0.0.1"
  
// root remote schedule repository (optional)
git = "github.com/jshiv/cronicle-sample"

/* The schedule contains the details 
for timing of the job as well as any 
commands and tasks that make up the job. */
schedule "example" {
  owner = {
    name = "cronicle"
    email = "root@cronicle.com"
  }
  cron = "every 5 minutes"
  
  start_date = "2015-06-01"
  end_date = "2019-09-09"
  
  retries = 3
  retry_delay = "5 min"
  
  task "mytask" = {
    command = ["/bin/echo", "Hello World"]
  }
}

```

A More complete example might look like:
```hcl
#cronicle.hcl
version = "0.0.1"

/* git is a list of remote repositories 
that will be added to the job scheduler. */
repos = [
    "github.com/jshiv/cronicle-sample1",
    "github.com/jshiv/cronicle-sample2",
    "github.com/jshiv/cronicle-sample3"
]

// root remote schedule repository (optional)
git = "github.com/jshiv/cronicle-sample"

schedule "example" {
  owner = {
    name = "cronicle"
    email = "root@cronicle.com"
  }
  cron = "every 5 minutes"
  
  start_date = "2015-06-01"
  end_date = "2019-09-09"
  

  task "run" {
    repo = "github.com/jshiv/cronicle-sample2"
    path = "scripts/"
    commit = "29lsjlw09lskjglkalkjgoij2lkj"
    command = ["/bin/bash", "run.sh"]

    retry {
      count = 3
      delay = 60
    }
  }

  task "echo" {
    command = ["/bin/echo", "Second Task"]
  }
  
  task "finish" {
    command =  ["/bin/echo", "Completed"]
    depends = ["run", "echo"]
  }
}

```

# Bash Commands

The init command sets up a new schedule repository with a sample conicle.hcl file
```bash
cronicle init
tree
.
├── cronicle.hcl
└── .repos
```

the run command starts the scheduler.
```bash
cronicle run
```


