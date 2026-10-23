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
	"github.com/jshiv/cronicle/internal/cronicle"

	"github.com/spf13/cobra"
)

// initCmd represents the init command
var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create a cronicle project: a starter cronicle.hcl, AGENTS.md guide, and .cronicle/",
	Long: `init sets up a cronicle project in --path:

  cronicle.hcl   a starter schedule: a small DAG of inline commands
  AGENTS.md      the guide to writing schedules (also: cronicle guide)
  CLAUDE.md      imports AGENTS.md, for Claude Code
  .cronicle/     local state, secrets, scratch and repo clones (gitignored)

Files that already exist are left as they are.`,
	Example: `  cronicle init
  cronicle init --path ./ops
  cronicle init --path ./ops --clone git@github.com:org/ops.git --key ~/.ssh/deploy_key`,
	Run: func(cmd *cobra.Command, args []string) {
		croniclePath, _ := cmd.Flags().GetString("path")
		clone, _ := cmd.Flags().GetString("clone")
		deployKey, _ := cmd.Flags().GetString("key")
		cronicle.InitStarter(croniclePath, clone, deployKey)
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
	initCmd.Flags().String("path", "./", "cronicle path")
	initCmd.Flags().String("clone", "", "clone remote git repository into --path, convenience flag for setting up a new project")
	initCmd.Flags().String("key", "~/.ssh/id_rsa", "path to private ssh-key that has read access to remote")

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// initCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// initCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
