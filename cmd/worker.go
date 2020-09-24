/*
Copyright © 2020 NAME HERE <EMAIL ADDRESS>

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
	"fmt"

	"github.com/jshiv/cronicle/internal/cron"
	"github.com/spf13/cobra"
)

// workerCmd represents the worker command
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Start a schedule consumer thread on a given distributed message queue.",
	Long: `Cronicle runs a centralized cron job that submits shedules to a message queue 
	for consumtion by the worker nodes which will execute all tasks in a given schedule. 
	To start a local distbuted cronicle flow with redis as the message broker:

	# Start a redis broker
	sudo docker run --name redis-cronicle -p 6379:6379 -d redis

	# Setup a cronicle repo
	cronicle init --path=./demo

	# In a seperate shell, start a worker to consume the schedules queue.
	cronicle worker --path ./demo/Cronicle.hcl --queue redis

	# Start cron, in distributed mode "cronicle run" will start a consumer thread by default
	# Note --worker=false will prevent the scheduler from starting a worker thread.
	cronicle run --path ./demo/Cronicle.hcl --worker=false --queue redis 


Multipule workers can be started, they will take turns consuming from the queue.
`,
	Args: cobra.MinimumNArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		path, _ := cmd.Flags().GetString("path")
		queueType, _ := cmd.Flags().GetString("queue")
		fmt.Println("Starting Worker from: " + path)
		runOptions := cron.RunOptions{RunWorker: true, QueueType: queueType}
		cron.StartWorker(path, runOptions)
	},
}

func init() {
	rootCmd.AddCommand(workerCmd)
	workerCmd.Flags().String("path", "./", "Path to git pull schedule repos.")
	queueDesc := `
	message broker technology for distributed schedule execution, 
	Options: 
		redis [distributed on localhost]
		nsq   [distributed on cluster running nsqd]
	Configurable via the queue.type field in Cronicle.hcl
	`
	workerCmd.Flags().String("queue", "", queueDesc)
	cobra.MarkFlagRequired(workerCmd.Flags(), "queue")

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// workerCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// workerCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
