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

// workerCmd represents the worker command
var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Start a schedule consumer thread on a given distributed message queue.",
	Long: `Cronicle runs a centralized cron job that submits schedules to a message queue 
	for consumption by the worker nodes which will execute all tasks in a given schedule. 
	To start a local distributed cronicle flow with redis as the message broker:

	# Start a redis broker
	sudo docker run --name redis-cronicle -p 6379:6379 -d redis

	# Setup a cronicle repo
	cronicle init --path=./demo

	# In a separate shell, start a worker to consume the schedules queue.
	cronicle worker --path ./demo/cronicle.hcl --queue redis

	# Start cron, in distributed mode "cronicle run" will start a consumer thread by default
	# Note --worker=false will prevent the scheduler from starting a worker thread.
	cronicle run --path ./demo/cronicle.hcl --worker=false --queue redis 


Multipule workers can be started, they will take turns consuming from the queue.
`,
	Args: cobra.MinimumNArgs(0),
	Run: func(cmd *cobra.Command, args []string) {
		path, _ := cmd.Flags().GetString("path")
		queueType, _ := cmd.Flags().GetString("queue")
		queueName, _ := cmd.Flags().GetString("queue-name")
		addr, _ := cmd.Flags().GetString("addr")
		logToFile, _ := cmd.Flags().GetBool("log-to-file")
		producerURL, _ := cmd.Flags().GetString("producer")
		producerToken, _ := cmd.Flags().GetString("producer-token")
		workerID, _ := cmd.Flags().GetString("worker-id")

		// HTTP-mode worker: long-polls a cronicle run --queue self
		// process. Replaces vice transport entirely once Phase 2c
		// removes Redis/NSQ.
		if producerURL != "" {
			if producerToken == "" {
				producerToken = os.Getenv("CRONICLE_LISTEN_TOKEN")
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
			return
		}

		slog.Info("Starting Worker from: " + path)
		runOptions := cronicle.RunOptions{
			RunWorker: true,
			QueueType: queueType,
			QueueName: queueName,
			Addr:      addr,
			LogToFile: logToFile,
		}
		cronicle.StartWorker(path, runOptions)
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
	Configurable via the queue.type field in cronicle.hcl
	`
	workerCmd.Flags().String("queue", "", queueDesc)
	// Note: --queue is required for vice modes (redis|nsq) but not when
	// --producer is set (HTTP long-poll path). We validate at runtime
	// rather than via MarkFlagRequired so both modes coexist on the
	// same command.
	workerCmd.Flags().String("queue-name", "cronicle", "Name of the queue to message schedules over.")

	addrDesc := `
	host:port of the queue service leader, 
	Options: 
		redis server[default: 127.0.0.1:6379]
		nsq   NSQLookupd service [default: localhost:4150 nsqd dameon]
	Configurable via the queue.addr field in cronicle.hcl
	`
	workerCmd.Flags().String("addr", "", addrDesc)
	workerCmd.Flags().Bool("log-to-file", false, "mirror structured JSON logs to path/.cronicle/log/cronicle.jsonl (rotated by lumberjack); per-run agent transcripts go to path/.cronicle/runs/")
	workerCmd.Flags().String("producer", "", "URL of a cronicle run --queue self producer (e.g. http://producer:8765). When set, the worker long-polls /v1/jobs over HTTP instead of using a vice broker.")
	workerCmd.Flags().String("producer-token", "", "bearer token for the producer's HTTP API. Falls back to $CRONICLE_LISTEN_TOKEN.")
	workerCmd.Flags().String("worker-id", "", "stable identifier for this worker in the producer's claim/ack records. Default: <hostname>-<pid>.")

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// workerCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// workerCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}
