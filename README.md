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

## Interesting and potentially useful librarys
  * Code generatoin
    * https://github.com/clipperhouse/gen


Usage[Vision]

The tool will use a Cronicle file to maintain `schedule as code`.
A simple job scheulder could look like:
```yaml
#Cronicle.yaml 
job: 
  name: Schedule A
  owner: cronicle
  start_date: 2015-06-01
  end_date: 2019-09-09
  retries: 1
  retry_delay: 5 min
    task: 
      name: First Task
      repo: github.com/jshiv/cronicle
      commit: 29lsjlw09lskjglkalkjgoij2lkj
      command: bash run.sh

```

Initilization of the system is managed by the `.cronicle.yaml` file