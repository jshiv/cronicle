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
	"github.com/spf13/cobra"

	"github.com/jshiv/cronicle/internal/cronicle"
)

// logFormat is bound to the --log-format persistent flag and resolved by
// PersistentPreRun before any subcommand executes.
var logFormat string

// liveFormat is bound to the --live-format persistent flag. Sets the
// wire encoding for the live SSE event stream (GET /v1/runs/{id}/events).
// Default "pretty" emits ANSI-colored multi-line bytes that an xterm.js
// frontend renders as a faithful TTY mirror.
var liveFormat string

// colorMode is bound to the --color persistent flag. auto (the default)
// defers to context: stdout pretty honors TTY+NO_COLOR; live SSE pretty
// defaults to color (consumers are terminal renderers). always/never are
// explicit overrides that beat both TTY detection and NO_COLOR.
var colorMode string

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Color policy must be applied BEFORE SetupLogging because the
		// pretty stdout handler captures color.NoColor at construction
		// time via fatih/color's SprintFunc closures.
		cronicle.SetColorMode(cronicle.ColorMode(colorMode))
		cronicle.SetupLogging(cronicle.LogFormat(logFormat))
		cronicle.SetLiveFormat(cronicle.LiveFormat(liveFormat))
	},
	Use:   "cronicle",
	Short: "Schedule shell commands and AI agents as code, run as a DAG on cron.",
	Long: `Cronicle runs schedules defined in cronicle.hcl. Each schedule is a DAG of
tasks (shell commands) and agents (LLM runs) fired on a cron trigger, with
every command, its output, and its result shown as it runs.

Write each task as one small step with its command inline in cronicle.hcl,
and build larger jobs by chaining tasks with depends:

  schedule "daily" {
    cron = "0 6 * * *"

    task "fetch" {
      command = ["sh", "-c", "curl -fsS https://example.com/data > ${scratch}/data.json"]
    }

    task "count" {
      depends = ["fetch"]
      command = ["python3", "-c", "import json; print(len(json.load(open('${scratch}/data.json'))))"]
    }
  }

To run the same step for every item of a list, use for_each rather than a
loop inside one task; each item becomes its own task in the DAG. Here each
open PR gets its own agent, in its own checkout of the PR's commit:

  task "list_prs" {
    command = ["gh", "pr", "list", "--json", "number,title,headRefOid"]
  }

  agent "review" {
    for_each = { for pr in task.list_prs.output : pr.number => pr }
    repo {
      url    = "https://github.com/org/app.git"
      commit = each.value.headRefOid
    }
    tools  = ["git", "text_editor"]
    prompt = "Review PR #${each.key}, ${each.value.title}, and write it to ${scratch}/review-${each.key}.md."
  }

Run "cronicle guide" for the full guide to writing schedules.`,
	Example: `  cronicle init                      # starter cronicle.hcl + AGENTS.md guide
  cronicle exec                      # run every schedule once, now
  cronicle exec --schedule daily     # run one schedule's DAG
  cronicle run                       # start the scheduler
  cronicle guide                     # how to write schedules`,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	//	Run: func(cmd *cobra.Command, args []string) { },
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		cronicle.Fatal(err)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&logFormat, "log-format", "auto",
		"stdout log format: auto (pretty if TTY, text if piped), pretty, text, or json")
	rootCmd.PersistentFlags().StringVar(&liveFormat, "live-format", "pretty",
		"wire format for the live SSE event stream (GET /v1/runs/{id}/events): pretty (ANSI; the default), json, or text. pretty-color is a deprecated alias for pretty (the live wire defaults to color regardless of TTY because consumers are xterm-class renderers).")
	rootCmd.PersistentFlags().StringVar(&colorMode, "color", "auto",
		"when to emit ANSI color in pretty output: auto (TTY+NO_COLOR for stdout; always-on for SSE wire), always (force on), never (force off). Honors the NO_COLOR convention from https://no-color.org in auto mode.")
}
