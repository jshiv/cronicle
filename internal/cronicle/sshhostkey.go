package cronicle

import (
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	homedir "github.com/mitchellh/go-homedir"
	gossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// embeddedKnownHosts is the baked-in set of SSH host keys for the major
// public git hosts (github.com, gitlab.com, bitbucket.org,
// ssh.dev.azure.com). Regenerate with gen_known_hosts.sh — each key is
// cross-checked against the host's officially published fingerprint at
// generation time, so a deploy-key clone to one of these hosts verifies
// out of the box without the operator pre-populating known_hosts.
//
//go:embed embedded_known_hosts.txt
var embeddedKnownHosts []byte

// Environment variables that influence SSH host-key verification.
const (
	// envSSHNoVerify, when truthy, disables host-key verification
	// entirely (gossh.InsecureIgnoreHostKey). This is the break-glass
	// escape hatch — e.g. when a host legitimately rotates its key and
	// the embedded set is stale, or for a throwaway test against an
	// ephemeral host. It is logged loudly because it reopens the MITM
	// hole the verification closes.
	envSSHNoVerify = "CRONICLE_SSH_NO_VERIFY"

	// envKnownHosts points at an additional known_hosts file, layered on
	// top of ~/.ssh/known_hosts and the embedded host set. Useful in
	// containers that have no shell home but mount a known_hosts file.
	envKnownHosts = "CRONICLE_KNOWN_HOSTS"
)

// embeddedKnownHostsFileOnce writes the embedded host keys to a temp
// file once per process (knownhosts.New only consumes file paths, not
// raw bytes) and caches the path.
var (
	embeddedKnownHostsFileOnce sync.Once
	embeddedKnownHostsFilePath string
	embeddedKnownHostsFileErr  error
)

func embeddedKnownHostsFile() (string, error) {
	embeddedKnownHostsFileOnce.Do(func() {
		f, err := os.CreateTemp("", "cronicle-known-hosts-*")
		if err != nil {
			embeddedKnownHostsFileErr = err
			return
		}
		if _, err := f.Write(embeddedKnownHosts); err != nil {
			_ = f.Close()
			embeddedKnownHostsFileErr = err
			return
		}
		if err := f.Close(); err != nil {
			embeddedKnownHostsFileErr = err
			return
		}
		embeddedKnownHostsFilePath = f.Name()
	})
	return embeddedKnownHostsFilePath, embeddedKnownHostsFileErr
}

// isTruthy reports whether an env-var value should be treated as "on".
func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// sshHostKeyCallback builds the host-key verification callback for a
// deploy-key clone. Resolution order:
//
//  1. CRONICLE_SSH_NO_VERIFY truthy → verification disabled (loud WARN).
//  2. Otherwise verify against the union of: the repo block's
//     known_hosts (when set), CRONICLE_KNOWN_HOSTS, ~/.ssh/known_hosts,
//     and the embedded major-host keys.
//  3. On an UNKNOWN host (not in any source): if acceptNew is true,
//     accept this connection (does not persist); else fail with an
//     error that names the escape-hatch env var.
//  4. On a host-key MISMATCH (the server's key differs from a known
//     one): always fail — this is the MITM / rotation signal — with an
//     error that also names the escape hatch.
//
// repoKnownHosts is the optional `known_hosts` path from the repo HCL
// block (may be ""); acceptNew is the repo's accept_new_host_key flag.
func sshHostKeyCallback(repoKnownHosts string, acceptNew bool) (gossh.HostKeyCallback, error) {
	if isTruthy(os.Getenv(envSSHNoVerify)) {
		slog.Warn("SSH host-key verification DISABLED — connection is vulnerable to man-in-the-middle",
			"reason", envSSHNoVerify+" is set",
			"hint", "unset "+envSSHNoVerify+" to restore verification")
		return gossh.InsecureIgnoreHostKey(), nil
	}

	// Collect readable known_hosts files in priority order. knownhosts.New
	// errors on a missing file, so only include paths that exist.
	var files []string
	addFile := func(p string) {
		if p == "" {
			return
		}
		if expanded, err := homedir.Expand(p); err == nil {
			p = expanded
		}
		if _, err := os.Stat(p); err == nil {
			files = append(files, p)
		}
	}
	addFile(repoKnownHosts)
	addFile(os.Getenv(envKnownHosts))
	if home, err := homedir.Dir(); err == nil {
		addFile(filepath.Join(home, ".ssh", "known_hosts"))
	}

	// Embedded major-host baseline (always present).
	embeddedPath, err := embeddedKnownHostsFile()
	if err != nil {
		return nil, fmt.Errorf("prepare embedded known_hosts: %w", err)
	}
	files = append(files, embeddedPath)

	base, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %v: %w", files, err)
	}

	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		err := base(hostname, remote, key)
		if err == nil {
			return nil
		}
		var ke *knownhosts.KeyError
		if errors.As(err, &ke) {
			if len(ke.Want) == 0 {
				// Host is entirely unknown to every source.
				if acceptNew {
					slog.Warn("accepting new SSH host key (accept_new_host_key)",
						"host", hostname,
						"fingerprint", gossh.FingerprintSHA256(key))
					return nil
				}
				return fmt.Errorf("unverified SSH host key for %s: host is not in known_hosts and is not a recognized public git host. "+
					"Add it to ~/.ssh/known_hosts, set repo { known_hosts = \"...\" } or accept_new_host_key = true, "+
					"or set %s=1 to disable verification (insecure — vulnerable to MITM)",
					hostname, envSSHNoVerify)
			}
			// Host is known but the presented key differs — MITM or rotation.
			return fmt.Errorf("SSH host key MISMATCH for %s (presented %s): "+
				"this is either a man-in-the-middle attack or the host rotated its key. "+
				"If you trust the change, update known_hosts; to bypass verification (insecure), set %s=1",
				hostname, gossh.FingerprintSHA256(key), envSSHNoVerify)
		}
		return err
	}, nil
}
