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

	"github.com/jshiv/cronicle/internal/create"

	"github.com/spf13/cobra"
)

// initCmd represents the init command
var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Creates a Cronicle git repo including a default Cronicle.hcl file and repos folder.",
	Long: `The cronicle init command will instantiate a Cronicle repository in the current directory
For example:

cronicle init --path ./cronicle
tree -a cronicle
├── .git
├── Cronicle.hcl
└── repos

This directory will contain the root Cronicle.hcl file and git repository. This is where
the main schedule will be defined and run. Subsequent schedules will be cloned into the 
repos folder.
`,
	Run: func(cmd *cobra.Command, args []string) {
		croniclePath, _ := cmd.Flags().GetString("path")
		fmt.Println("Initialize Cronicle: " + croniclePath)
		create.Init(croniclePath)
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
	initCmd.Flags().String("path", "./", "new cronicle path")

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// initCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// initCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
