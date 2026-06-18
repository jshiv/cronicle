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
	"runtime"

	"github.com/spf13/cobra"
)

// Build metadata, injected at release time via goreleaser ldflags
// (-X github.com/jshiv/cronicle/cmd.version=... etc). Untagged builds
// (go build / go install) report "dev".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// versionString is the one-line form used for `--version`.
func versionString() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s)", version, commit, date, runtime.Version())
}

// versionCmd represents the version command
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the cronicle version and build metadata.",
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Println(versionString())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
	// Wire cobra's built-in --version flag to the same string.
	rootCmd.Version = versionString()
}
