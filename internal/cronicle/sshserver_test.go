package cronicle_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gliderssh "github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// startTestSSHServer launches an in-process SSH server that serves git
// repositories rooted at serveRoot. Only the public key from
// authorizedKeyPath is accepted. Returns the listen address (host:port)
// and a stop function.
func startTestSSHServer(serveRoot string, authorizedKeyPath string) (string, func(), error) {
	pubBytes, err := os.ReadFile(authorizedKeyPath)
	if err != nil {
		return "", nil, fmt.Errorf("read public key: %w", err)
	}
	allowed, _, _, _, err := gossh.ParseAuthorizedKey(pubBytes)
	if err != nil {
		return "", nil, fmt.Errorf("parse public key: %w", err)
	}

	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, fmt.Errorf("generate host key: %w", err)
	}
	signer, err := gossh.NewSignerFromKey(hostPriv)
	if err != nil {
		return "", nil, fmt.Errorf("host signer: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("listen: %w", err)
	}

	server := &gliderssh.Server{
		PublicKeyHandler: func(_ gliderssh.Context, key gliderssh.PublicKey) bool {
			return gliderssh.KeysEqual(key, allowed)
		},
		Handler: func(s gliderssh.Session) {
			cmd := s.Command()
			if len(cmd) < 2 {
				fmt.Fprintln(s.Stderr(), "missing git command")
				_ = s.Exit(1)
				return
			}
			gitCmd := cmd[0]
			if gitCmd != "git-upload-pack" && gitCmd != "git-receive-pack" {
				fmt.Fprintln(s.Stderr(), "unsupported command")
				_ = s.Exit(1)
				return
			}
			// Strip surrounding quotes (some clients quote the path).
			reqPath := strings.Trim(cmd[1], "'\"")
			repoPath := filepath.Join(serveRoot, filepath.FromSlash(reqPath))
			c := exec.Command(gitCmd, repoPath)
			stdin, _ := c.StdinPipe()
			stdout, _ := c.StdoutPipe()
			stderr, _ := c.StderrPipe()
			if err := c.Start(); err != nil {
				fmt.Fprintln(s.Stderr(), "start:", err)
				_ = s.Exit(1)
				return
			}
			go func() { _, _ = io.Copy(stdin, s); stdin.Close() }()
			go func() { _, _ = io.Copy(s.Stderr(), stderr) }()
			_, _ = io.Copy(s, stdout)
			if err := c.Wait(); err != nil {
				_ = s.Exit(1)
				return
			}
			_ = s.Exit(0)
		},
	}
	server.AddHostKey(signer)

	go func() { _ = server.Serve(listener) }()

	return listener.Addr().String(), func() { _ = server.Close() }, nil
}
