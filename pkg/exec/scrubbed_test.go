package exec

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestScrubbed_blocksParentLeak verifies that ExecuteScrubbedStreamContext
// does NOT leak the parent's env to the child. The whole point of this
// mode is that an agent-driven `env` or `printenv` shouldn't be able
// to read worker-process state.
func TestScrubbed_blocksParentLeak(t *testing.T) {
	const key = "CRONICLE_TEST_LEAK_CANARY"
	if err := os.Setenv(key, "should-not-leak"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	defer os.Unsetenv(key)

	// Spawn /bin/sh -c 'env' under scrubbed mode and confirm the canary
	// isn't in the captured output. PATH (allowlisted) should still be.
	res := ExecuteScrubbedStreamContext(context.Background(),
		[]string{"/bin/sh", "-c", "env"},
		".", nil, nil, nil)
	if res.ExitStatus != 0 {
		t.Fatalf("env exit=%d stderr=%q", res.ExitStatus, res.Stderr)
	}
	if strings.Contains(res.Stdout, key) || strings.Contains(res.Stdout, "should-not-leak") {
		t.Errorf("scrubbed env leaked canary:\n%s", res.Stdout)
	}
	// PATH should be present (it's on the allowlist) — proves we're not
	// just empty-env'ing the child.
	if !strings.Contains(res.Stdout, "PATH=") {
		t.Errorf("scrubbed env should include PATH:\n%s", res.Stdout)
	}
}

// TestScrubbed_taskEnvIsHonored verifies that opts.env entries DO reach
// the child even in scrubbed mode. The scrubbing only affects parent-
// env inheritance; explicitly-passed values flow through.
func TestScrubbed_taskEnvIsHonored(t *testing.T) {
	res := ExecuteScrubbedStreamContext(context.Background(),
		[]string{"/bin/sh", "-c", "echo $FOO"},
		".", []string{"FOO=fromTaskEnv"}, nil, nil)
	if res.ExitStatus != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitStatus, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "fromTaskEnv" {
		t.Errorf("task env value should reach child: %q", res.Stdout)
	}
}

// TestExecuteWithStreamContext_inheritsParentEnv guards the legacy
// behavior: callers using the non-scrubbed entry point still see the
// parent env. This is a long-standing contract that shell-task
// dispatch relies on.
func TestExecuteWithStreamContext_inheritsParentEnv(t *testing.T) {
	const key = "CRONICLE_TEST_INHERIT_PROOF"
	if err := os.Setenv(key, "yes"); err != nil {
		t.Fatalf("setenv: %v", err)
	}
	defer os.Unsetenv(key)

	res := ExecuteWithStreamContext(context.Background(),
		[]string{"/bin/sh", "-c", "echo $" + key},
		".", nil, nil, nil)
	if res.ExitStatus != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitStatus, res.Stderr)
	}
	if strings.TrimSpace(res.Stdout) != "yes" {
		t.Errorf("legacy entry point should inherit parent env: %q", res.Stdout)
	}
}
