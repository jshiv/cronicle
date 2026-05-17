/*
Copyright © 2026 The Cronicle Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/
package cmd

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// `cronicle secret` is the CLI control plane for the in-tree secret
// store. Two transports, same surface:
//
//   - Default: opens .cronicle/state.db on disk via state.Open() and
//     operates against the SQLite SecretStore directly. Works without
//     a running cronicle, but only for the in-tree sqlite backend.
//
//   - --remote URL [--token T]: PUT/DELETE/GET against the listener's
//     /v1/secrets API. Backend-agnostic — the listener forwards to
//     whatever state.Backend the running cronicle has bound, so this
//     is the right path for Postgres-backed (cronicle-infra hosted)
//     or any future-backend setups. Tokens default to
//     env CRONICLE_LISTEN_TOKEN.
//
// Subcommands:
//   cronicle secret set NAME                  value from stdin
//   cronicle secret set NAME --value V        value on the flag (discouraged)
//   cronicle secret get NAME                  prints value to stdout
//   cronicle secret list                      names only
//   cronicle secret rm NAME                   delete

// nameRE mirrors the listener's validator and the $secret.NAME regex
// in secretstore — UPPER_SNAKE_CASE only.
var nameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage cronicle's secret store (local state.db or remote listener)",
	Long: `cronicle secret manages the in-tree secret store that backs
$secret.NAME resolution at task dispatch.

By default, reads and writes go directly against the local
.cronicle/state.db file (the same store the running cronicle uses).
Pair with --workdir to target a specific cronicle workdir; defaults
to "." (the current directory).

For a remote / Postgres-backed cronicle, pass --remote http://host:port
--token T to route through the listener's /v1/secrets API. The
listener is backend-agnostic, so this works against any state.Backend
(sqlite, Postgres, future). Falls back to env CRONICLE_REMOTE and
CRONICLE_LISTEN_TOKEN.`,
}

var secretSetCmd = &cobra.Command{
	Use:   "set NAME",
	Short: "Upsert a secret value (reads value from stdin by default)",
	Long: `Stores (NAME, value) in the secret store.

Value is read from stdin by default so it doesn't appear in process
listings or shell history. Pipe it in:

    echo -n "sk-ant-..." | cronicle secret set ANTHROPIC_API_KEY

Or use --value for the unsafe-but-quick form (the value will be visible
in 'ps' output and shell history).`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if !nameRE.MatchString(name) {
			return fmt.Errorf("name must match UPPER_SNAKE_CASE (got %q)", name)
		}
		value, _ := cmd.Flags().GetString("value")
		if value == "" {
			b, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			value = strings.TrimRight(string(b), "\n")
		}
		if value == "" {
			return fmt.Errorf("value required (pipe to stdin or pass --value)")
		}
		actor, _ := cmd.Flags().GetString("actor")
		client, err := openSecretClient(cmd)
		if err != nil {
			return err
		}
		defer client.Close()
		if err := client.Put(name, value, actor); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "set: %s\n", name)
		return nil
	},
}

var secretGetCmd = &cobra.Command{
	Use:   "get NAME",
	Short: "Print a secret's value to stdout (use with care)",
	Long: `Prints the stored value as a single line on stdout. Intended
for scripting + debug; emits no trailing newline so the output composes
cleanly into other tools.

Refuses to print when stdout is a TTY unless --force is passed, to
avoid an accidental shoulder-surf when used interactively.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		force, _ := cmd.Flags().GetBool("force")
		if isTerminal(os.Stdout) && !force {
			return fmt.Errorf("stdout is a TTY; pass --force to print plaintext to terminal")
		}
		client, err := openSecretClient(cmd)
		if err != nil {
			return err
		}
		defer client.Close()
		v, ok, err := client.Get(name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no such secret: %s", name)
		}
		fmt.Fprint(cmd.OutOrStdout(), v)
		return nil
	},
}

var secretListCmd = &cobra.Command{
	Use:   "list",
	Short: "List secret NAMES in the store (values never printed)",
	RunE: func(cmd *cobra.Command, args []string) error {
		client, err := openSecretClient(cmd)
		if err != nil {
			return err
		}
		defer client.Close()
		names, etag, err := client.List()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "(no secrets)")
			return nil
		}
		tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "NAME\tSTORE_ETAG\n")
		for _, n := range names {
			fmt.Fprintf(tw, "%s\t%s\n", n, etag)
		}
		return tw.Flush()
	},
}

var secretRmCmd = &cobra.Command{
	Use:     "rm NAME",
	Aliases: []string{"delete", "remove"},
	Short:   "Remove a secret by name",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		actor, _ := cmd.Flags().GetString("actor")
		client, err := openSecretClient(cmd)
		if err != nil {
			return err
		}
		defer client.Close()
		ok, err := client.Delete(args[0], actor)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no such secret: %s", args[0])
		}
		fmt.Fprintf(cmd.OutOrStdout(), "removed: %s\n", args[0])
		return nil
	},
}

func init() {
	rootCmd.AddCommand(secretCmd)
	secretCmd.AddCommand(secretSetCmd)
	secretCmd.AddCommand(secretGetCmd)
	secretCmd.AddCommand(secretListCmd)
	secretCmd.AddCommand(secretRmCmd)

	// --workdir is shared so every subcommand targets the same state.db
	// path the running cronicle would. Default "." matches `cronicle run`.
	// Ignored when --remote is used.
	for _, c := range []*cobra.Command{secretSetCmd, secretGetCmd, secretListCmd, secretRmCmd} {
		c.Flags().String("workdir", ".", "cronicle workdir (state.db is at WORKDIR/.cronicle/state.db). Ignored when --remote is set.")
	}
	// --remote / --token route through the listener instead of a local
	// state.db. Registered on every subcommand so the help is uniform.
	addRemoteFlags(secretSetCmd, secretGetCmd, secretListCmd, secretRmCmd)

	secretSetCmd.Flags().String("value", "",
		"secret value (UNSAFE — appears in ps/shell history; prefer piping to stdin)")
	secretSetCmd.Flags().String("actor", "cli",
		"audit label recorded against this write")
	secretRmCmd.Flags().String("actor", "cli",
		"audit label recorded against this delete")
	secretGetCmd.Flags().Bool("force", false,
		"allow printing plaintext when stdout is a TTY")
}

// isTerminal returns true if f is connected to a terminal. Best-effort —
// portable code path uses os.Stat to check for the char-device bit which
// is reliable on every platform cronicle targets.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
