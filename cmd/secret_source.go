/*
Copyright © 2026 The Cronicle Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/
package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/jshiv/cronicle/internal/cronicle"
	"github.com/jshiv/cronicle/internal/cronicle/secretsource"
	"github.com/jshiv/cronicle/internal/cronicle/secretstore"
	"github.com/spf13/cobra"
)

// addSecretSourceFlags registers the four secret-source flags on a
// command. Centralized so `cronicle run` and `cronicle worker` share
// the exact same surface area and defaults — operators learn it once.
//
// Defaults are deliberately blank: when the flag is unset (and the env
// fallback is empty), startSecretSource derives a source from context
// — see deriveDefaultSpec. This keeps "no flags, just works" true for
// the standalone CLI shape while still letting operators pin a source
// explicitly when they want one.
func addSecretSourceFlags(cmd *cobra.Command) {
	cmd.Flags().String("secret-source", "",
		"pluggable secret ingestion source — env://, file:///abs/path.json, "+
			"http(s)://host/path, or state:// (CLI/HTTP-managed only). "+
			"When unset, cronicle derives a sensible default: workers use "+
			"--producer for HTTP fetch, producers (--listen) use state://, "+
			"standalone uses env://. Falls back to env CRONICLE_SECRET_SOURCE.")
	cmd.Flags().String("secret-source-token", "",
		"bearer token sent as Authorization: Bearer ... on every http(s) secret-source fetch. "+
			"Workers infer it from --producer-token when unset. "+
			"Falls back to env CRONICLE_SECRET_SOURCE_TOKEN.")
	cmd.Flags().Duration("secret-refresh-interval", secretstore.DefaultRefreshInterval,
		"how often to re-poll the secret source (sub-second is wasteful; default 5s).")
	cmd.Flags().Duration("secret-stale-ttl", secretstore.DefaultStaleTTL,
		"refuse to dispense secrets if the most recent successful refresh is older than this. "+
			"Default 5m. Set to 0 to disable staleness gating (not recommended in production).")
}

// startSecretSource binds the process-wide secret store to the local
// state.Backend and (optionally) starts an ingestion pump. Always
// binds the backend so $secret.NAME resolution works even when no
// source is configured — operators can manage entirely via
// `cronicle secret set ...` or the listener's PUT /v1/secrets.
//
// Bearer auth handling intentionally prefers env-var supply (so the
// token never appears in `ps aux`), but accepts a flag for the local-
// dev shape.
func startSecretSource(ctx context.Context, cmd *cobra.Command) error {
	be := cronicle.StateStore()
	if be == nil {
		// No state backend — runtime is in a degenerate state. Don't
		// fail the boot path; secret resolution will return
		// ErrNotConfigured, which is the right signal for tests/
		// minimal runs.
		return nil
	}
	secretstore.Default().SetBackend(be)

	spec, _ := cmd.Flags().GetString("secret-source")
	if spec == "" {
		spec = os.Getenv("CRONICLE_SECRET_SOURCE")
	}
	token, _ := cmd.Flags().GetString("secret-source-token")
	if token == "" {
		token = os.Getenv("CRONICLE_SECRET_SOURCE_TOKEN")
	}

	// Smart-defaults pass: if the operator gave nothing, pick something
	// sensible from the rest of the command's flags. The mapping:
	//
	//   cronicle worker --producer X --producer-token Y
	//      → ingest from X/v1/secrets, bearer = Y
	//   cronicle run --listen :PORT --listen-token T
	//      → state:// (the producer's local state.db IS the truth;
	//        the listener serves it to workers)
	//   anything else (standalone, no listen)
	//      → env:// (the shell IS the secret source)
	if spec == "" {
		derivedSpec, derivedToken := deriveDefaultSpec(cmd)
		spec = derivedSpec
		if token == "" {
			token = derivedToken
		}
	}

	interval, _ := cmd.Flags().GetDuration("secret-refresh-interval")
	staleTTL, _ := cmd.Flags().GetDuration("secret-stale-ttl")

	// state:// is special — it means "the state.db IS the truth, no
	// ingestion pump." Skip Open + refresh, just bind the backend
	// (already done above) and we're done.
	if spec == "" || spec == "state://" {
		slog.Info("secret source: state-backed (no ingestion)", "spec", spec)
		// Disable staleness gating: there's no refresh signal to be stale.
		secretstore.Default().StaleTTL(0)
		return nil
	}

	src, err := secretsource.Open(ctx, spec)
	if err != nil {
		return fmt.Errorf("secret source: %w", err)
	}
	if token != "" {
		bearer, ok := src.(interface {
			WithBearer(string) *secretsource.HTTPSource
		})
		if !ok {
			return fmt.Errorf("secret source: --secret-source-token is only meaningful for http(s) sources (got %s)", src.String())
		}
		bearer.WithBearer(token)
	}

	slog.Info("secret source enabled",
		"source", src.String(),
		"refresh", interval.String(),
		"stale_ttl", staleTTL.String(),
	)
	if err := secretstore.StartDefault(ctx, be, src, interval, staleTTL); err != nil {
		return fmt.Errorf("secret store start: %w", err)
	}
	return nil
}

// deriveDefaultSpec picks a sensible source spec from the command's
// other flags. The mapping mirrors the table in addSecretSourceFlags's
// doc. Returns ("", "") when nothing useful can be inferred — caller
// falls through to env://.
//
// We only inspect flag values that exist on the command; we never
// register them here, so the lookup is harmless on commands that don't
// declare them (cobra returns the empty string).
func deriveDefaultSpec(cmd *cobra.Command) (spec, token string) {
	// Worker shape: --producer X --producer-token Y → HTTP fetch.
	producer := flagOrEmpty(cmd, "producer")
	if producer != "" {
		producerToken := flagOrEmpty(cmd, "producer-token")
		if producerToken == "" {
			producerToken = os.Getenv("CRONICLE_LISTEN_TOKEN")
		}
		// Strip any trailing slash for clean joining.
		base := strings.TrimRight(producer, "/")
		return base + "/v1/secrets", producerToken
	}

	// Producer shape: --listen + --listen-token → state.db is the truth.
	listen := flagOrEmpty(cmd, "listen")
	if listen != "" {
		return "state://", ""
	}

	// Standalone shape: the operator's shell IS the secret source.
	return "env://", ""
}

func flagOrEmpty(cmd *cobra.Command, name string) string {
	f := cmd.Flag(name)
	if f == nil {
		return ""
	}
	return f.Value.String()
}
