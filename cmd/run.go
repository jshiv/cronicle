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
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

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
		configSource, _ := cmd.Flags().GetString("config-source")
		workdir, _ := cmd.Flags().GetString("workdir")

		runWorker, _ := cmd.Flags().GetBool("worker")
		workerCount, _ := cmd.Flags().GetInt("worker-count")
		cron, _ := cmd.Flags().GetString("cron")
		command, _ := cmd.Flags().GetString("command")
		logToFile, _ := cmd.Flags().GetBool("log-to-file")
		listenAddr, _ := cmd.Flags().GetString("listen")
		listenToken, _ := cmd.Flags().GetString("listen-token")
		if listenToken == "" {
			listenToken = os.Getenv("CRONICLE_LISTEN_TOKEN")
		}
		stateSource, _ := cmd.Flags().GetString("state-source")
		if stateSource == "" {
			stateSource = os.Getenv("CRONICLE_STATE_SOURCE")
		}

		ctx, stop := signal.NotifyContext(context.Background(),
			syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		runOptions := cronicle.RunOptions{
			RunWorker:   runWorker,
			WorkerCount: workerCount,
			LogToFile:   logToFile,
			ListenAddr:  listenAddr,
			ListenToken: listenToken,
			StateSource: stateSource,
			// Defer secret-source startup until AFTER cronicle has opened
			// state.db — the secretstore binds to that backend, so it has
			// to land in the right order. The hook closes over `cmd` so
			// flag lookups continue to work.
			PostStateStoreHook: func() error { return startSecretSource(ctx, cmd) },
		}

		// One-shot mode: --cron + --command bootstraps a synthetic
		// schedule into the on-disk HCL. Only meaningful for file-based
		// runs; with --config-source the user is opting into a remote
		// config and the one-shot wouldn't have a destination.
		if cron != "" && command != "" {
			if configSource != "" {
				slog.Error("--cron/--command bootstrap is incompatible with --config-source",
					"hint", "edit the source's HCL directly or drop --config-source")
				os.Exit(2)
			}
			conf := cronicle.Default()
			conf.Schedules[0].Name = "schedule1"
			conf.Schedules[0].Cron = cron
			conf.Schedules[0].Tasks[0].Name = "task1"
			conf.Schedules[0].Tasks[0].Command = strings.Split(command, " ")
			cronicle.Init(filepath.Dir(path), "", "", conf)
		}

		if configSource != "" {
			// Source-aware dispatch: open the source (file/http/s3/pg),
			// boot the scheduler against it, register heartbeat +
			// config_refresh entries. PostStateStoreHook (set above) wires
			// the secret-source pump after state.db is open.
			if err := cronicle.RunFromSource(ctx, configSource, workdir, runOptions); err != nil {
				slog.Error("cronicle run --config-source failed", "err", err.Error())
				os.Exit(1)
			}
			return
		}

		// File-source path. cronicle.Run opens state.db, then fires the
		// PostStateStoreHook (which binds the secret-source pump), then
		// blocks until ctx is canceled (SIGINT/SIGTERM) running the
		// scheduler. Returns naturally after graceful shutdown so the
		// cobra command exits cleanly.
		cronicle.Run(ctx, path, runOptions)

	},
}

func init() {
	rootCmd.AddCommand(runCmd)
	runCmd.Flags().String("path", "./cronicle.hcl", "Path to a cronicle.hcl file (file-source mode; ignored when --config-source is set)")
	runCmd.Flags().String("config-source", "",
		"pluggable config source URL — file:///abs/path, http(s)://host/cfg, s3://bucket/key, postgres://user@host/db?table=...&key=... "+
			"(no scheme treated as a local file path; takes precedence over --path)")
	runCmd.Flags().String("workdir", ".",
		"working directory for .cronicle/state.db, .cronicle/log/*.jsonl, scratch dirs. "+
			"File-source mode derives this from --path; required for non-file sources")
	runCmd.Flags().Bool("worker", true, "start a worker thread to consume tasks in distributed mode")
	runCmd.Flags().Int("worker-count", 4, "number of in-process self-workers (queue mode); each runs one job at a time, so N = up to N concurrent jobs")
	runCmd.Flags().String("cron", "", "crontab expression for running a command e.g. @every 1h")
	runCmd.Flags().String("command", "", "command to run on the given cron [/bin/echo cronicle]")
	runCmd.Flags().Bool("log-to-file", false, "mirror structured JSON logs to path/.cronicle/log/cronicle.jsonl (rotated by lumberjack); stdout is unaffected and remains controlled by --log-format")
	runCmd.Flags().String("listen", "", "host:port to expose the remote-trigger HTTP API (e.g. :8765). Empty disables. Requires --listen-token.")
	runCmd.Flags().String("listen-token", "", "bearer token for the remote-trigger HTTP API (or env CRONICLE_LISTEN_TOKEN). Required when --listen is set.")
	runCmd.Flags().String("state-source", "", "state plane DSN (or env CRONICLE_STATE_SOURCE). Empty → local .cronicle/state.db (SQLite); postgres://… → durable Postgres so run history / ${last_run} survive restarts.")
	addSecretSourceFlags(runCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// runCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// runCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
