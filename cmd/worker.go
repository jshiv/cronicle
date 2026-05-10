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
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jshiv/cronicle/internal/cronicle"
	"github.com/spf13/cobra"
)

// workerCmd represents the worker command. As of v0.5 the only supported
// transport is HTTP long-poll against a `cronicle run --queue self`
// producer. The earlier vice (Redis/NSQ) modes were removed alongside
// the broker dependencies.
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Run a worker that long-polls a cronicle producer for jobs.",
	Long: `Workers consume the producer-served queue over HTTP. Start cronicle on
one host with --queue self, then point one or more workers at it:

	# producer (host A)
	cronicle run \
	    --path cronicle.hcl \
	    --queue self \
	    --listen :8765 \
	    --listen-token "$CRONICLE_LISTEN_TOKEN" \
	    --worker=false

	# workers (any host)
	cronicle worker \
	    --path /local/repo \
	    --producer http://producer:8765 \
	    --producer-token "$CRONICLE_LISTEN_TOKEN" \
	    --worker-id node-1

Multiple workers can connect to the same producer; SQLite WAL serializes
job claims so only one worker executes any given run.
`,
	Args: cobra.MinimumNArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		path, _ := cmd.Flags().GetString("path")
		producerURL, _ := cmd.Flags().GetString("producer")
		producerToken, _ := cmd.Flags().GetString("producer-token")
		workerID, _ := cmd.Flags().GetString("worker-id")
		logToFile, _ := cmd.Flags().GetBool("log-to-file")

		if producerURL == "" {
			slog.Error("--producer is required (e.g. http://producer:8765)")
			os.Exit(2)
		}
		if producerToken == "" {
			producerToken = os.Getenv("CRONICLE_LISTEN_TOKEN")
		}
		if producerToken == "" {
			slog.Error("--producer-token (or CRONICLE_LISTEN_TOKEN env) is required")
			os.Exit(2)
		}

		ctx, cancel := signal.NotifyContext(context.Background(),
			syscall.SIGINT, syscall.SIGTERM)
		defer cancel()

		err := cronicle.StartHTTPWorker(ctx, cronicle.HTTPWorkerOptions{
			ProducerURL: producerURL,
			Token:       producerToken,
			WorkerID:    workerID,
			Path:        path,
			LogToFile:   logToFile,
		})
		if err != nil && err != context.Canceled {
			slog.Error("worker exited", "error", err.Error())
			os.Exit(1)
		}
	},
}

func init() {
	rootCmd.AddCommand(workerCmd)
	workerCmd.Flags().String("path", "./", "Local schedule repo root. Used by tasks that have a repo block; same semantics as the producer's --path.")
	workerCmd.Flags().String("producer", "", "URL of a cronicle run --queue self producer (e.g. http://producer:8765). Required.")
	workerCmd.Flags().String("producer-token", "", "Bearer token for the producer's HTTP API. Falls back to $CRONICLE_LISTEN_TOKEN.")
	workerCmd.Flags().String("worker-id", "", "Stable identifier for this worker in the producer's claim/ack records. Default: <hostname>-<pid>.")
	workerCmd.Flags().Bool("log-to-file", false, "Mirror structured JSON logs to path/.cronicle/log/cronicle.jsonl (rotated by lumberjack); per-run agent transcripts go to path/.cronicle/runs/.")
}
