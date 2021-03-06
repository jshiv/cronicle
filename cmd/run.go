/*
Copyright © 2019 NAME HERE <EMAIL ADDRESS>

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cmd

import (
	"path/filepath"
	"strings"

	"github.com/jshiv/cronicle/internal/cronicle"
	"github.com/spf13/cobra"
)

// runCmd represents the run command
var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Cronicle run reads in the cronicle.hcl schedule and starts running tasks",
	Long: `The cronicle run command starts the cron scheduler for the specified cronicle.hcl file.
For example:

cronicle init --path cronicle
cronicle run --path cronicle/cronicle.hcl

The run command will log schedule information to stdout including git commit info.`,
	Args: cobra.MinimumNArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		path, _ := cmd.Flags().GetString("path")

		runWorker, _ := cmd.Flags().GetBool("worker")
		queueType, _ := cmd.Flags().GetString("queue")
		queueName, _ := cmd.Flags().GetString("queue-name")
		addr, _ := cmd.Flags().GetString("addr")
		cron, _ := cmd.Flags().GetString("cron")
		command, _ := cmd.Flags().GetString("command")
		logToFile, _ := cmd.Flags().GetBool("log-to-file")

		runOptions := cronicle.RunOptions{RunWorker: runWorker, QueueType: queueType, QueueName: queueName, Addr: addr, LogToFile: logToFile}

		if cron != "" && command != "" {
			conf := cronicle.Default()
			conf.Schedules[0].Name = "schedule1"
			conf.Schedules[0].Cron = cron
			conf.Schedules[0].Tasks[0].Name = "task1"
			conf.Schedules[0].Tasks[0].Command = strings.Split(command, " ")
			cronicle.Init(filepath.Dir(path), "", "", conf)
		}

		cronicle.Run(path, runOptions)

	},
}

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().String("path", "./cronicle.hcl", "Path to a cronicle.hcl file")
	runCmd.Flags().Bool("worker", true, "start a worker thread to consume tasks in distributed mode")
	queueDesc := `
	message broker technology for distributed schedule execution, 
	Options: 
		redis [distributed on localhost:6379]
		nsq [run on cluster with nsqd:4150]
	Configurable via the queue.type field in cronicle.hcl
	`
	runCmd.Flags().String("queue", "", queueDesc)
	runCmd.Flags().String("queue-name", "cronicle", "Name of the queue to message schedules over.")

	addrDesc := `
	host:port of the queue service leader, 
	Options: 
		redis server[default: 127.0.0.1:6379]
		nsq   NSQLookupd service [default: localhost:4150 nsqd dameon]
	Configurable via the queue.addr field in cronicle.hcl
	`
	runCmd.Flags().String("addr", "", addrDesc)
	runCmd.Flags().String("cron", "", "crontab expression for running a command e.g. @every 1h")
	runCmd.Flags().String("command", "", "command to run on the given cron [/bin/echo cronicle]")
	runCmd.Flags().Bool("log-to-file", false, "log to path/.cronicle/log/cronicle.log")

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// runCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// runCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
