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
	"fmt"

	"github.com/jshiv/cronicle/internal/cron"
	"github.com/spf13/cobra"
)

// runCmd represents the run command
var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Cronicle run reads in the Cronicle.hcl schedule and starts running tasks",
	Long: `The cronicle run command starts the cron scheduler for the specified Cronicle.hcl file.
For example:

cronicle init --path cronicle
cronicle run --path cronicle/Cronicle.hcl

The run command will log schedule information to stdout including git commit info.`,
	Args: cobra.MinimumNArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		path, _ := cmd.Flags().GetString("path")

		runWorker, _ := cmd.Flags().GetBool("worker")
		queueType, _ := cmd.Flags().GetString("queue")
		runOptions := cron.RunOptions{RunWorker: runWorker, QueueType: queueType}

		fmt.Println("Reading from: " + path)
		cron.Run(path, runOptions)
	},
}

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().String("path", "./Cronicle.hcl", "Path to a Cronicle.hcl file")
	runCmd.Flags().Bool("worker", true, "start a worker thread to consume tasks in distributed mode")
	queueDesc := `
	message broker technology for distributed schedule execution, 
	Options: 
		redis [distributed on localhost]
		nsq [run on cluster with nsqd]
	Configurable via the queue.type field in Cronicle.hcl
	`
	runCmd.Flags().String("queue", "", queueDesc)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// runCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// runCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
