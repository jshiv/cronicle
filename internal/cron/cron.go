package cron

import (
	"fmt"
	"runtime"

	"github.com/jshiv/cronicle/internal/bash"
	"github.com/jshiv/cronicle/internal/config"
	"github.com/jshiv/cronicle/internal/git"

	"github.com/fatih/color"

	"path/filepath"

	log "github.com/sirupsen/logrus"
	"gopkg.in/robfig/cron.v2"
)

// Run is the main function of the cron package
func Run(cronicleFile string) {

	cronicleFileAbs, err := filepath.Abs(cronicleFile)
	if err != nil {
		log.Fatal(err)
	}
	// croniclePath := filepath.Dir(cronicleFileAbs)

	conf, _ := config.GetConfig(cronicleFileAbs)
	hcl := config.GetHcl(*conf)
	slantyedCyan := color.New(color.FgCyan, color.Italic).SprintFunc()
	fmt.Printf("%s", slantyedCyan(string(hcl.Bytes())))

	RunConfig(*conf)
}

//RunConfig starts cron
func RunConfig(conf config.Config) {
	log.WithFields(log.Fields{"cronicle": "start"}).Info("Starting Scheduler...")

	c := cron.New()
	c.AddFunc("@every 10s", func() { log.WithFields(log.Fields{"cronicle": "heartbeat"}).Info("Running...") })
	for _, schedule := range conf.Schedules {
		_, err := c.AddFunc(schedule.Cron, AddSchedule(schedule))
		if err != nil {
			fmt.Printf("\x1b[31;1m%s\x1b[0m\n", fmt.Sprintf("schedule cron format error: %s", schedule.Name))
			log.Fatal(err)
		}
	}
	c.Start()
	runtime.Goexit()
}

// AddSchedule retuns a function primed with the given schedules commands
func AddSchedule(schedule config.Schedule) func() {
	log.WithFields(log.Fields{"schedule": schedule.Name}).Info("Running...")

	return func() {
		for _, task := range schedule.Tasks {
			log.WithFields(log.Fields{"task": task.Name}).Info(task.Command)
			git.Pull(task.Path)
			result := bash.Bash(task.Command, task.Path)
			commit, err := git.GetCommit(task.Path)
			if err != nil {
				log.WithFields(log.Fields{
					"task": task.Name,
					"exit": result.ExitStatus,
				}).Error(err)
			} else if result.ExitStatus == 0 {
				log.WithFields(log.Fields{
					"task":   task.Name,
					"exit":   result.ExitStatus,
					"commit": commit.Hash.String()[:11],
					"email":  commit.Author.Email,
				}).Info(result.Stdout)
			} else if result.ExitStatus == 1 {
				log.WithFields(log.Fields{
					"task":   task.Name,
					"exit":   result.ExitStatus,
					"commit": commit.Hash.String()[:11],
					"email":  commit.Author.Email,
				}).Error(result.Stderr)
			} else {
				log.WithFields(log.Fields{
					"task":   task.Name,
					"exit":   result.ExitStatus,
					"commit": commit.Hash.String()[:11],
					"email":  commit.Author.Email,
				}).Error(result.Stderr)
			}
		}
	}
}

func Dummy(in string) string {
	return in
}
