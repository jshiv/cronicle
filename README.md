# cronicle
Opinionated workflow scheduler.

## Development Phase: Alpha


Design Doc
----------

Cronicle `will be` a tool for managing and scheduling workflows that leans on the unix philosophy for composition. The features that differentiate cronicle from other similar tools
* focus on tracability and visibility into historical jobs and backfilling
* tight integration with git version control as a mechanisim to track history and changes over time
* no support for data connectors and compute engines, keeping the scope thin. Other tools are better suited for these kinds of tasks


### Why Cronicle
  Other tools I have worked with tend to be bloated in scope and very complicated to setup and use. I want a tool that is easy to comprehend, deploy and use. I basically want distributed cron with job version control. Cronicle will focus on integrating these ideas in a simple way.


## Design Notes:
  Component librarys for POC
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
  * distributed configuration managment codebase: https://github.com/purpleidea/mgmt/
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
A bash job scheulder could look like:
```hcl
#Cronicle.hcl 
version = "0.0.1"

/* The schedule contains the details 
for timing of the job as well as any 
commands and tasks that make up the job. */
schedule "job" "core" {
  owner = "cronicle"
  cron = "every 5 minutes"
  
  start_date = "2015-06-01"
  end_date = "2019-09-09"
  
  retries = 3
  
  git = "github.com/jshiv/cronicle-sample"
  retry_delay = "5 min"
  
  command = ["/bin/echo", "Hello World"]
}

```

The tool will use a Cronicle file to maintain `schedule as code`.
A More complete example might look like:
```hcl
#Cronicle.hcl
version = "0.0.1"

/* git is a list of remote repositorys 
that will be added to the job scheduler. */
git = [
    "github.com/jshiv/cronicle-sample1",
    "github.com/jshiv/cronicle-sample2",
    "github.com/jshiv/cronicle-sample3"
]

schedule "job" "core" {
  owner = "cronicle"
  email = "core@cronicle.com"
  cron = "every 5 minutes"
  
  start_date = "2015-06-01"
  end_date = "2019-09-09"
  
  retries = 3
  
  git = "github.com/jshiv/cronicle-sample"
  retry_delay = "5 min"
  
  task "run" {
    remote = "github.com/jshiv/cronicle-sample"
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
Initilization of the system is managed by the `.cronicle.yaml` file which can be generated by `cronicle init`

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



