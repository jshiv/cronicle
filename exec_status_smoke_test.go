//go:build smoke

package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSmoke_ExecStatus(t *testing.T) {
	for _, tc := range []struct {
		name        string
		command     string
		args        []string
		wantFailure bool
	}{
		{"success", "true", nil, false},
		{"failed_schedule", "false", nil, true},
		{"missing_schedule", "true", []string{"--schedule", "missing"}, true},
		{"missing_task", "true", []string{"--schedule", "review", "--task", "missing"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			config := filepath.Join(dir, "cronicle.hcl")
			body := `schedule "review" {
  task "check" { command = ["` + tc.command + `"] }
  task "cleanup" {
    depends = ["check"]
    continue_on_failure = true
    command = ["touch", "${path}/cleaned"]
  }
}
schedule "other" {
  task "check" { command = ["touch", "${path}/other-ran"] }
}
`
			if err := os.WriteFile(config, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"exec", "--path", config}, tc.args...)
			out, err := exec.Command(cronicleBin, args...).CombinedOutput()
			if (err != nil) != tc.wantFailure {
				t.Fatalf("err=%v, want failure=%v\n%s", err, tc.wantFailure, out)
			}
			if len(tc.args) == 0 {
				for _, name := range []string{"cleaned", "other-ran"} {
					if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
						t.Fatalf("%s did not run: %v\n%s", name, err, out)
					}
				}
			}
		})
	}
}
