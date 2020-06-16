# cronicle
git integrated workflow scheduler that provides a pull model for CI/CD and versioning on job execution.

## Development Phase: Alpha

Usage[Vision] 

The tool will use a Cronicle.hcl file to maintain `schedule as code`.
A bash job scheduler could look like:
```hcl
#Cronicle.hcl 
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

The tool will use a Cronicle file to maintain `schedule as code`.
A More complete example might look like:
```hcl
#Cronicle.hcl
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
  
  retries = 3
  
  retry_delay = "5 min"
  
  task "run" {
    repo = "github.com/jshiv/cronicle-sample2"
    path = "scripts/"
    commit = "29lsjlw09lskjglkalkjgoij2lkj"
    command = ["/bin/bash", "run.sh"]
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

The init command sets up a new schedule repository with a sample Conicle.yml file
```bash
cronicle init
tree
.
├── .git
├── .gitignore
├── Cronicle.yml
├── logs
└── repos
```

# Bash Commands
the run command starts the scheduler.
```bash
cronicle run
```


