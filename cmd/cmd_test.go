// Cobra-level tests for cmd/. These exercise the CLI wiring layer:
// flag registration, subcommand assembly, help output, and the
// argument / flag validation that happens before any subcommand's
// Run / RunE actually does work.
//
// They deliberately do NOT exercise the heavy paths (cronicle.Run,
// cronicle.ExecTasks, cronicle.StartHTTPWorker, openSecretClient) —
// those need a running scheduler / state.db / producer and are
// covered by the internal/cronicle/ test suite and the binary smoke
// tests at the repo root. Keeping the surface narrow here means tests
// stay fast and don't need fixtures.
//
// Why this file matters: cmd/ had zero tests before this. A typo'd
// flag name, a forgotten AddCommand, or a renamed flag without an
// updated default would all have shipped silently.
package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// executeCmd runs the root command with the supplied args and returns
// (combined out+err, exec error). Cobra writes errors and help text
// to the command's Out/Err writers; redirecting them into one buffer
// lets a single string assertion check either.
func executeCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	// Cobra's --help / --version bool flags persist on the shared rootCmd
	// across Execute calls; a prior test that ran --help would otherwise
	// leave help=true and short-circuit a later --version run into help
	// output. Reset them so each invocation starts clean.
	if f := rootCmd.Flags().Lookup("help"); f != nil {
		_ = f.Value.Set("false")
	}
	if f := rootCmd.Flags().Lookup("version"); f != nil {
		_ = f.Value.Set("false")
	}
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	rootCmd.SetArgs(args)
	err := rootCmd.Execute()
	return buf.String(), err
}

// ---- root + persistent flags ------------------------------------------

// TestRoot_PersistentFlagsRegistered guards the three knobs that every
// subcommand inherits — accidentally renaming or removing one would
// break --color / --log-format / --live-format on every subcommand at
// once, which is hard to notice from any single subcommand's test.
func TestRoot_PersistentFlagsRegistered(t *testing.T) {
	cases := []struct {
		name string
		def  string
	}{
		{"log-format", "auto"},
		{"live-format", "pretty"},
		{"color", "auto"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := rootCmd.PersistentFlags().Lookup(tc.name)
			if f == nil {
				t.Fatalf("persistent flag --%s missing", tc.name)
			}
			if f.DefValue != tc.def {
				t.Errorf("--%s default: got %q, want %q", tc.name, f.DefValue, tc.def)
			}
		})
	}
}

// TestRoot_AllSubcommandsRegistered guards against a future PR
// accidentally dropping the AddCommand wiring for a subcommand.
func TestRoot_AllSubcommandsRegistered(t *testing.T) {
	want := map[string]bool{
		"init":    false,
		"run":     false,
		"exec":    false,
		"worker":  false,
		"secret":  false,
		"version": false,
	}
	for _, c := range rootCmd.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("subcommand %q not registered on rootCmd", name)
		}
	}
}

// TestRoot_HelpListsAllSubcommands verifies --help output actually
// surfaces every subcommand to users, not just register them silently.
func TestRoot_HelpListsAllSubcommands(t *testing.T) {
	out, err := executeCmd(t, "--help")
	if err != nil {
		t.Fatalf("unexpected --help error: %v", err)
	}
	for _, sub := range []string{"init", "run", "exec", "worker", "secret", "version"} {
		if !strings.Contains(out, sub) {
			t.Errorf("--help output missing %q; output:\n%s", sub, out)
		}
	}
}

// TestRoot_UnknownSubcommand: cobra should reject an unknown
// subcommand with a non-nil error so the binary exits non-zero.
func TestRoot_UnknownSubcommand(t *testing.T) {
	_, err := executeCmd(t, "this-command-does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown subcommand, got nil")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Errorf("error message should name the problem; got: %v", err)
	}
}

// ---- version subcommand -----------------------------------------------

// TestVersionCmd_PrintsVersion: `cronicle version` prints the build
// version string. With no ldflags injected (test binary) it reports the
// "dev" default — the assertion just guards that the command runs and
// emits the version var rather than empty output.
func TestVersionCmd_PrintsVersion(t *testing.T) {
	out, err := executeCmd(t, "version")
	if err != nil {
		t.Fatalf("unexpected version error: %v", err)
	}
	if !strings.Contains(out, version) {
		t.Errorf("version output missing %q; got: %q", version, out)
	}
}

// TestRoot_VersionFlagWired: cobra only emits a --version flag when
// rootCmd.Version is set. Guard that wiring so the flag doesn't silently
// disappear if the init order changes.
func TestRoot_VersionFlagWired(t *testing.T) {
	if rootCmd.Version == "" {
		t.Fatal("rootCmd.Version is empty; --version flag will not be registered")
	}
	out, err := executeCmd(t, "--version")
	if err != nil {
		t.Fatalf("unexpected --version error: %v", err)
	}
	if !strings.Contains(out, version) {
		t.Errorf("--version output missing %q; got: %q", version, out)
	}
}

// ---- run subcommand flags ---------------------------------------------

// TestRunCmd_FlagWiring asserts every flag the run subcommand
// documents is actually registered, with the expected default. The
// flag names and defaults are part of cronicle's user-facing contract
// (scripts pass --path, --listen, etc.); silently renaming would break
// downstream automation.
func TestRunCmd_FlagWiring(t *testing.T) {
	cases := []struct {
		name string
		def  string
	}{
		{"path", "./cronicle.hcl"},
		{"config-source", ""},
		{"workdir", "."},
		{"worker", "true"},
		{"worker-count", "4"},
		{"cron", ""},
		{"command", ""},
		{"log-to-file", "false"},
		{"listen", ""},
		{"listen-token", ""},
		{"state-source", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := runCmd.Flags().Lookup(tc.name)
			if f == nil {
				t.Fatalf("flag --%s missing on runCmd", tc.name)
			}
			if f.DefValue != tc.def {
				t.Errorf("--%s default: got %q, want %q", tc.name, f.DefValue, tc.def)
			}
		})
	}
}

// ---- exec subcommand flags --------------------------------------------

func TestExecCmd_FlagWiring(t *testing.T) {
	cases := []struct {
		name string
		def  string
	}{
		{"path", "./cronicle.hcl"},
		{"task", ""},
		{"schedule", ""},
		{"time", ""},
		{"end", ""},
		{"cron", ""},
		{"command", ""},
		{"log-to-file", "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := execCmd.Flags().Lookup(tc.name)
			if f == nil {
				t.Fatalf("flag --%s missing on execCmd", tc.name)
			}
			if f.DefValue != tc.def {
				t.Errorf("--%s default: got %q, want %q", tc.name, f.DefValue, tc.def)
			}
		})
	}
}

// ---- worker subcommand flags ------------------------------------------

func TestWorkerCmd_FlagWiring(t *testing.T) {
	cases := []struct {
		name string
		def  string
	}{
		{"path", "./"},
		{"producer", ""},
		{"producer-token", ""},
		{"worker-id", ""},
		{"log-to-file", "false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := workerCmd.Flags().Lookup(tc.name)
			if f == nil {
				t.Fatalf("flag --%s missing on workerCmd", tc.name)
			}
			if f.DefValue != tc.def {
				t.Errorf("--%s default: got %q, want %q", tc.name, f.DefValue, tc.def)
			}
		})
	}
}

// ---- secret subcommand assembly ---------------------------------------

// TestSecretCmd_SubcommandsRegistered: set / get / list / rm must all
// be reachable as `cronicle secret <verb>` — losing one breaks the
// operator's mental model of the CLI.
func TestSecretCmd_SubcommandsRegistered(t *testing.T) {
	want := map[string]bool{"set": false, "get": false, "list": false, "rm": false}
	for _, c := range secretCmd.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("`cronicle secret %s` not registered", name)
		}
	}
}

// TestSecretRmCmd_HasAliases guards the documented aliases (delete,
// remove). Operators in muscle memory from kubectl / docker reach for
// "delete"; silently dropping the alias would surprise.
func TestSecretRmCmd_HasAliases(t *testing.T) {
	want := map[string]bool{"delete": false, "remove": false}
	for _, a := range secretRmCmd.Aliases {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for alias, found := range want {
		if !found {
			t.Errorf("alias %q missing on `cronicle secret rm`", alias)
		}
	}
}

// ---- secret name + arg validation -------------------------------------

// TestSecretSet_RequiresArg: `cronicle secret set` without a NAME
// must fail cobra's ExactArgs check before any RunE runs.
func TestSecretSet_RequiresArg(t *testing.T) {
	out, err := executeCmd(t, "secret", "set")
	if err == nil {
		t.Fatalf("expected error for missing NAME arg, got nil. output:\n%s", out)
	}
	// Cobra's ExactArgs error mentions "accepts 1 arg(s)".
	if !strings.Contains(err.Error(), "arg") {
		t.Errorf("error should mention arg count; got: %v", err)
	}
}

// TestSecretSet_RejectsInvalidName: name validation in secretSetCmd
// catches lowercase / punctuation BEFORE opening the client, so we
// can assert it without a real state.db.
func TestSecretSet_RejectsInvalidName(t *testing.T) {
	cases := []string{
		"lowercase",
		"With-Dash",
		"1LEADING_DIGIT",
		"has space",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := executeCmd(t, "secret", "set", name, "--value", "anything")
			if err == nil {
				t.Errorf("expected name validation error for %q, got nil", name)
				return
			}
			if !strings.Contains(err.Error(), "UPPER_SNAKE_CASE") {
				t.Errorf("error should mention naming rule; got: %v", err)
			}
		})
	}
}

// TestSecretRm_RequiresArg mirrors the set test for the rm subcommand
// — same cobra ExactArgs guard, but documenting it as a test lets a
// future change to the rm signature break loudly.
func TestSecretRm_RequiresArg(t *testing.T) {
	out, err := executeCmd(t, "secret", "rm")
	if err == nil {
		t.Fatalf("expected error for missing NAME arg, got nil. output:\n%s", out)
	}
	if !strings.Contains(err.Error(), "arg") {
		t.Errorf("error should mention arg count; got: %v", err)
	}
}

// TestSecretGet_RequiresArg: same for get.
func TestSecretGet_RequiresArg(t *testing.T) {
	_, err := executeCmd(t, "secret", "get")
	if err == nil {
		t.Fatal("expected error for missing NAME arg, got nil")
	}
	if !strings.Contains(err.Error(), "arg") {
		t.Errorf("error should mention arg count; got: %v", err)
	}
}

// ---- secret shared flag wiring ----------------------------------------

// TestSecretSubcommands_ShareWorkdirFlag verifies every secret
// subcommand exposes --workdir with the same default. Asymmetry here
// (one subcommand missing the flag) would silently route some commands
// to a different state.db than others.
func TestSecretSubcommands_ShareWorkdirFlag(t *testing.T) {
	for _, c := range []*cobra.Command{secretSetCmd, secretGetCmd, secretListCmd, secretRmCmd} {
		t.Run(c.Name(), func(t *testing.T) {
			f := c.Flags().Lookup("workdir")
			if f == nil {
				t.Fatalf("--workdir missing on `secret %s`", c.Name())
			}
			if f.DefValue != "." {
				t.Errorf("--workdir default on `secret %s`: got %q, want \".\"",
					c.Name(), f.DefValue)
			}
		})
	}
}
