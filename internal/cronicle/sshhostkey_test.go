package cronicle

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

// addr is a minimal net.Addr for driving the host-key callback.
type addr struct{ s string }

func (a addr) Network() string { return "tcp" }
func (a addr) String() string  { return a.s }

// genHostKey produces a throwaway ed25519 SSH public key so tests can
// simulate a server presenting a key, without touching the network.
func genHostKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519 keygen: %v", err)
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("NewPublicKey: %v", err)
	}
	return sshPub
}

// embeddedGithubKey returns the public key for github.com as parsed from
// the embedded known_hosts — the genuine key the callback should accept.
func embeddedGithubKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	for _, line := range strings.Split(string(embeddedKnownHosts), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "github.com ssh-ed25519") {
			continue
		}
		_, _, pub, _, _, err := gossh.ParseKnownHosts([]byte(line))
		if err != nil {
			t.Fatalf("parse embedded github line: %v", err)
		}
		return pub
	}
	t.Fatal("no github.com ssh-ed25519 line in embedded known_hosts")
	return nil
}

// TestSSHHostKey_EmbeddedHostAccepted: a deploy-key clone to github.com
// verifies out of the box against the embedded key — no operator
// known_hosts setup required. This is the whole point of pre-seeding.
func TestSSHHostKey_EmbeddedHostAccepted(t *testing.T) {
	t.Setenv(envSSHNoVerify, "")
	t.Setenv(envKnownHosts, "")

	cb, err := sshHostKeyCallback("", false)
	if err != nil {
		t.Fatalf("build callback: %v", err)
	}
	if err := cb("github.com:22", addr{"140.82.121.3:22"}, embeddedGithubKey(t)); err != nil {
		t.Errorf("genuine github.com key should verify against embedded set, got: %v", err)
	}
}

// TestSSHHostKey_UnknownHostRefused: a host in NONE of the sources is
// refused, and the error names the escape-hatch env var so the operator
// knows how to proceed.
func TestSSHHostKey_UnknownHostRefused(t *testing.T) {
	t.Setenv(envSSHNoVerify, "")
	t.Setenv(envKnownHosts, "")

	cb, err := sshHostKeyCallback("", false)
	if err != nil {
		t.Fatalf("build callback: %v", err)
	}
	err = cb("git.internal.example:22", addr{"10.0.0.5:22"}, genHostKey(t))
	if err == nil {
		t.Fatal("unknown host must be refused")
	}
	if !strings.Contains(err.Error(), envSSHNoVerify) {
		t.Errorf("error must name the escape-hatch env var %q; got: %v", envSSHNoVerify, err)
	}
}

// TestSSHHostKey_AcceptNewAllowsUnknown: with accept_new_host_key=true,
// an unknown host is accepted (but the error path for KNOWN-mismatch is
// still closed — see the mismatch test).
func TestSSHHostKey_AcceptNewAllowsUnknown(t *testing.T) {
	t.Setenv(envSSHNoVerify, "")
	t.Setenv(envKnownHosts, "")

	cb, err := sshHostKeyCallback("", true /* acceptNew */)
	if err != nil {
		t.Fatalf("build callback: %v", err)
	}
	if err := cb("git.internal.example:22", addr{"10.0.0.5:22"}, genHostKey(t)); err != nil {
		t.Errorf("accept_new_host_key should accept an unknown host; got: %v", err)
	}
}

// TestSSHHostKey_MismatchRefusedEvenWithAcceptNew: presenting a WRONG
// key for a host that IS known (github.com, via embedded) must always
// fail — even with accept_new_host_key — because that's the MITM signal.
func TestSSHHostKey_MismatchRefusedEvenWithAcceptNew(t *testing.T) {
	t.Setenv(envSSHNoVerify, "")
	t.Setenv(envKnownHosts, "")

	cb, err := sshHostKeyCallback("", true /* acceptNew — must NOT rescue a mismatch */)
	if err != nil {
		t.Fatalf("build callback: %v", err)
	}
	// A freshly-generated key is NOT github's real key → mismatch.
	err = cb("github.com:22", addr{"140.82.121.3:22"}, genHostKey(t))
	if err == nil {
		t.Fatal("a wrong key for a known host must be refused (MITM signal)")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "MISMATCH") {
		t.Errorf("error should flag a key mismatch; got: %v", err)
	}
}

// TestSSHHostKey_EscapeHatchDisablesVerification: CRONICLE_SSH_NO_VERIFY
// truthy accepts any key (the documented break-glass). Covers the values
// the isTruthy helper recognizes.
func TestSSHHostKey_EscapeHatchDisablesVerification(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(envSSHNoVerify, v)
			cb, err := sshHostKeyCallback("", false)
			if err != nil {
				t.Fatalf("build callback: %v", err)
			}
			// Any key, any host, accepted.
			if err := cb("anything.example:22", addr{"10.9.9.9:22"}, genHostKey(t)); err != nil {
				t.Errorf("%s=%q should disable verification; got: %v", envSSHNoVerify, v, err)
			}
		})
	}
}

// TestSSHHostKey_RepoKnownHostsFile: a known_hosts path supplied via the
// repo block verifies a host the embedded set doesn't know.
func TestSSHHostKey_RepoKnownHostsFile(t *testing.T) {
	t.Setenv(envSSHNoVerify, "")
	t.Setenv(envKnownHosts, "")

	host := "git.internal.example"
	key := genHostKey(t)
	line := gossh.MarshalAuthorizedKey(key)
	khLine := host + " " + strings.TrimSpace(string(line)) + "\n"

	dir := t.TempDir()
	khPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(khPath, []byte(khLine), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}

	cb, err := sshHostKeyCallback(khPath, false)
	if err != nil {
		t.Fatalf("build callback: %v", err)
	}
	if err := cb(host+":22", addr{"10.0.0.5:22"}, key); err != nil {
		t.Errorf("host in repo known_hosts should verify; got: %v", err)
	}
}

// TestIsTruthy covers the env-var truthiness helper.
func TestIsTruthy(t *testing.T) {
	on := []string{"1", "true", "TRUE", " yes ", "on", "On"}
	off := []string{"", "0", "false", "no", "off", "nope"}
	for _, s := range on {
		if !isTruthy(s) {
			t.Errorf("isTruthy(%q) = false, want true", s)
		}
	}
	for _, s := range off {
		if isTruthy(s) {
			t.Errorf("isTruthy(%q) = true, want false", s)
		}
	}
}
