# cronicle
Opinionated workflow scheduler.

## Development Phase: Alpha


Design Doc
----------

Cronicle `will be` a tool for managing and scheduling workflows that leans on the unix philosophy for composition. The features that differentiate cronicle from other similar tools
* focus on traceability and visibility into historical jobs and backfilling
* tight integration with git version control as a mechanism to track history and changes over time
* no support for data connectors and compute engines, keeping the scope thin. Other tools are better suited for these kinds of tasks


### Why Cronicle
  Other tools I have worked with tend to be bloated in scope and very complicated to setup and use. I want a tool that is easy to comprehend, deploy and use. I basically want distributed cron with job version control. Cronicle will focus on integrating these ideas in a simple way.


## Design Notes:
  Component libraries for POC
* Project Structure
  * https://github.com/spf13/cobra
* Configuration File Reading
  * https://github.com/spf13/viper
* git
  * pure go: https://github.com/src-d/go-git
  * git bindings: https://github.com/libgit2/git2go
* Scheduling Cron jobs
  * https://gopkg.in/robfig/cron.v2
* Logging
  * https://github.com/sirupsen/logrus
* Testing
  * https://github.com/onsi/ginkgo
* Possible tool chains for distributed job management
  * distributed scheduler: https://github.com/distribworks/dkron
  * just raft by hashicorp: https://github.com/hashicorp/raft
  * https://github.com/contribsys/faktory_worker_go
  * by uber: https://github.com/uber/cherami-server
  * distributed configuration management codebase: https://github.com/purpleidea/mgmt/
  * raft based key value store:(used as backend by kubernetties) https://github.com/etcd-io/etcd
    *https://github.com/etcd-io/etcd/tree/master/clientv3
  * Simply use git remote for state management.
* Distributed messaging que
  * https://github.com/nsqio/nsq
  * https://github.com/RichardKnop/machinery
* DAG 
  * https://github.com/hashicorp/terraform/tree/master/dag
  * https://github.com/goombaio/dag
* Configuration Language
  * https://github.com/hashicorp/hcl2

## Interesting and potentially useful librarys
  * Code generatoin
    * https://github.com/clipperhouse/gen


Usage[Vision] 

The tool will use a Cronicle file to maintain `schedule as code`.
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
  owner = "cronicle"
  cron = "every 5 minutes"
  
  start_date = "2015-06-01"
  end_date = "2019-09-09"
  
  retries = 3
  retry_delay = "5 min"
  
  command = ["/bin/echo", "Hello World"]
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
  owner = "cronicle"
  email = "root@cronicle.com"
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

# Init
Initialization of the system is managed by the `.cronicle.yaml` file which can be generated by `cronicle init`

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

The history command displays the history of the running scheduler
```bash
cronicle history
```

# Internal Flow
`cronicle init`
1. creates or reads the `Cronicle.hcl` file
2. generate a `config` and validate the hcl file
3. identify remote repositories in the `config` and clone them to the `repos` directory

```flow
cronicle init
if .git exists:
  if Cronicle.hcl not exists:
   -> Read remote git url `git config --get remote.origin.url`
   -> Create Cronicle.hcl file including `git="<remote url>"
   -> git commit "Cronicle Initial Commit" -> git push origin 
   -> Report config validation
  elif Cronicle.hcl exists:
   -> Load config from Cronicle.hcl
   -> Check for `git="<remote url>"
   if .git has remote:
      -> git commit -> git push origin 
   -> Recursively clone any repos specified in Cronicle.hcl to ./repos/
   -> Append new schedules to Config.Schedules
   -> Report config validation
else .git not exists:
  if Cronicle.hcl not exists:
   -> Initialize a local .git repository
   -> Create Cronicle.hcl
   -> git commit "Cronicle Initial Commit"
   -> Report config validation
  elif Cronicle.hcl exists:
   -> Load config from Cronicle.hcl
   -> Recursively clone any repos specified in Cronicle.hcl to ./repos/
   -> Check for `git="<remote url>"
   if .git has remote:
      -> git commit -> git push origin 
   -> Append new schedules to Config.Schedules
   -> Report config validation
```

`cronicle run`
1. create a `dag` from the config, arranging any tasks based on the `depends` flag
2. populate the `dag` with partial bash and git methods
3. loop over the `dag` and add `cron` functions for each `command`
4. start the scheduler 
5. function start, exit status, stdout, stderr, git commit/repo information is passed to the logger
6. `git diff commitA commitB` for the root or any sub repos can be used to investigate changes that happened between different runs of the scheduler for a given job or task. 



## POC requirements
* `cronicle init` should create a Cronicle scheduler directory at `./` with reasonable defaults.
* `cronicle run` should read the local `Cronicle.hcl` file and start executing on the schedule.
* `cronicle run > cronicle.log` will write meaningful logs including timestamp, job/task info, success/failure and commit.


## Open Questions
* Does `cronicle init` put the `root` schedule in the repos folder?
* Does the `root` scheduler commit and push to remote?
* Where do logs go? Are they broken out for each repo?
* Are logs committed and pushed to the root remote?
* Can git client ui tools be used to visualize the scheduler?


## TODO: Items to complete the POC
* Update `internal/config/config.go` with config, schedule, and task struct's
* Stop using bash(use os) to create files and folder in `init`
* Add functionality to `internal/cron/main.go` to add bash, logging functions to cron
* Add internal library that integrates config, git, cron, and bash
* `init` should check for the existence of `Cronicle` file before creating a template
* Architect approach 
  * for populating config/dag with bash and git functions
  * for passing config/dag to the cron scheduler 
  * for passing result data(stderr, commits, ect...) to the logger

## TODO
* Write unit tests and use test driven development
* Use proper go error handling
* Fix poor logic in `internal/git/main.go:L14`
* Fix argument to take []string vs string in `internal/git/main.go:Bash()`
* Add [dag](https://github.com/hashicorp/terraform/tree/master/dag) internal library
* Figure out how we can use the dag in the context of a config
* add dag from dependent tasks to config
* Integrate with distributed message que for resilience

