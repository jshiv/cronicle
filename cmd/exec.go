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
	"github.com/jshiv/cronicle/internal/cronicle"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"time"
)

// execCmd represents the exec command
var execCmd = &cobra.Command{
	Use:   "exec",
	Short: "cronicle exec executes a specified task or schedule",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	Run: func(cmd *cobra.Command, args []string) {
		path, _ := cmd.Flags().GetString("path")
		task, _ := cmd.Flags().GetString("task")
		schedule, _ := cmd.Flags().GetString("schedule")

		timeFlag, _ := cmd.Flags().GetString("time")
		var now time.Time
		var end time.Time
		if timeFlag == "" {
			now = time.Now().In(time.Local)
		} else {
			//TODO: Add flags for timeFlag format and timezone
			if n, err := time.Parse(time.RFC3339, timeFlag); err != nil {
				log.Error(err)
			} else {
				now = n.Local()
			}

		}

		endFlag, _ := cmd.Flags().GetString("end")
		if endFlag == "" {
			end = now
		} else {
			//TODO: Add flags for endFlag format and timezone
			if n, err := time.Parse(time.RFC3339, endFlag); err != nil {
				log.Error(err)
			} else {
				end = n.Local()
			}
		}
		for t := now; t.After(end) == false; t = t.AddDate(0, 0, 1) {
			cronicle.ExecTasks(path, task, schedule, t)
		}
		log.Info("Reading from: " + path)
	},
}

func init() {
	rootCmd.AddCommand(execCmd)
	execCmd.Flags().String("path", "./cronicle.hcl", "Path to a cronicle.hcl file")
	execCmd.Flags().String("task", "", "Name of the task to execute (required)")
	execCmd.Flags().String("schedule", "", "Name of the schedule that contains the task to execute")
	execCmd.Flags().String("time", "", "Timestamp to execute task [2006-01-02T15:04:05-08:00]")
	execCmd.Flags().String("end", "", "date range end Timestamp [2006-01-02T15:04:05-08:00]")

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// execCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// execCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
