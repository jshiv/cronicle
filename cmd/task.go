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

	"time"
)

// taskCmd represents the task command
var taskCmd = &cobra.Command{
	Use:   "task",
	Short: "cronicle task executes a specified task",
	Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println("task called")
		path, _ := cmd.Flags().GetString("path")
		name, _ := cmd.Flags().GetString("name")
		schedule, _ := cmd.Flags().GetString("schedule")

		timeFlag, _ := cmd.Flags().GetString("time")
		var now time.Time
		var end time.Time
		if timeFlag == "" {
			now = time.Now().In(time.Local)
		} else {
			//TODO: Add flags for timeFlag format and timezone
			if n, err := time.Parse(time.RFC3339, timeFlag); err != nil {
				fmt.Println(err)
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
				fmt.Println(err)
			} else {
				end = n.Local()
			}
		}
		for t := now; t.After(end) == false; t = t.AddDate(0, 0, 1) {
			cron.ExecTasks(path, name, schedule, t)
		}
		fmt.Println("Reading from: " + path)
	},
}

func init() {
	rootCmd.AddCommand(taskCmd)
	taskCmd.Flags().String("path", "./Cronicle.hcl", "Path to a Cronicle.hcl file")
	taskCmd.Flags().String("name", "", "Name of the task to execute (required)")
	taskCmd.Flags().String("schedule", "", "Name of the schedule that contains the task to execute")
	taskCmd.Flags().String("time", "", "Timestamp to execute task [2006-01-02T15:04:05-08:00]")
	taskCmd.Flags().String("end", "", "date range end Timestamp [2006-01-02T15:04:05-08:00]")

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// taskCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// taskCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
