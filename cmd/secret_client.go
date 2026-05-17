/*
Copyright © 2026 The Cronicle Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/
package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jshiv/cronicle/internal/cronicle/state"
	"github.com/spf13/cobra"
)

// secretClient abstracts the storage seam for the `cronicle secret`
// subcommand. Two impls:
//
//   localClient  — opens .cronicle/state.db on disk via state.Open()
//                  and operates against the SQLite SecretStore directly.
//                  Backend-aware: works for whatever the local sqlite
//                  binary speaks. Doesn't reach a Postgres-backed
//                  cronicle-infra deployment.
//
//   remoteClient — speaks the listener's bearer-auth'd /v1/secrets API
//                  (PUT, DELETE, GET). Backend-agnostic: the listener
//                  forwards to whatever state.Backend the running
//                  cronicle has bound — sqlite, Postgres, or any future
//                  backend that satisfies the SecretStore interface.
//
// Both implement the same five methods so each subcommand body is a
// straight function call; the routing happens in openSecretClient.
type secretClient interface {
	Put(name, value, actor string) error
	Get(name string) (value string, ok bool, err error)
	List() (names []string, etag string, err error)
	Delete(name, actor string) (ok bool, err error)
	Close() error
}

// openSecretClient picks local vs remote based on flags / env. The
// rule: `--remote URL` (or env CRONICLE_REMOTE) routes through HTTP;
// anything else opens the local .cronicle/state.db.
//
// Remote auth: `--token` flag → env CRONICLE_LISTEN_TOKEN → empty. An
// empty token will cause the listener to 401; we don't pre-validate
// here so an operator who runs against an unauthenticated /healthz-
// style endpoint still gets a clear server-side error.
func openSecretClient(cmd *cobra.Command) (secretClient, error) {
	remote, _ := cmd.Flags().GetString("remote")
	if remote == "" {
		remote = os.Getenv("CRONICLE_REMOTE")
	}
	if remote != "" {
		token, _ := cmd.Flags().GetString("token")
		if token == "" {
			token = os.Getenv("CRONICLE_LISTEN_TOKEN")
		}
		return newRemoteClient(remote, token)
	}
	return openLocalClient(cmd)
}

// addRemoteFlags registers --remote and --token on every secret
// subcommand. Centralized so the help text stays consistent.
func addRemoteFlags(cmds ...*cobra.Command) {
	for _, c := range cmds {
		c.Flags().String("remote", "",
			"cronicle listener URL (e.g. http://producer:8765). When set, "+
				"talks to the listener's /v1/secrets API instead of opening "+
				"the local state.db. Falls back to env CRONICLE_REMOTE.")
		c.Flags().String("token", "",
			"bearer token for --remote. Falls back to env CRONICLE_LISTEN_TOKEN. "+
				"Prefer the env var so the token doesn't appear in 'ps' / shell history.")
	}
}

// --- local impl: backed by *state.Store ------------------------------------

type localClient struct{ s *state.Store }

func openLocalClient(cmd *cobra.Command) (secretClient, error) {
	workdir, _ := cmd.Flags().GetString("workdir")
	if workdir == "" {
		workdir = "."
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(abs, ".cronicle", "state.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no state.db at %s — run `cronicle run` once in this workdir, "+
				"or pass --remote http://… --token … to manage a remote cronicle", dbPath)
		}
		return nil, err
	}
	s, err := state.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &localClient{s: s}, nil
}

func (c *localClient) Put(name, value, actor string) error {
	return c.s.PutSecret(name, value, actor)
}
func (c *localClient) Get(name string) (string, bool, error) {
	return c.s.GetSecret(name)
}
func (c *localClient) List() ([]string, string, error) {
	names, err := c.s.ListSecretNames()
	if err != nil {
		return nil, "", err
	}
	etag, _ := c.s.SecretsEtag()
	return names, etag, nil
}
func (c *localClient) Delete(name, actor string) (bool, error) {
	return c.s.DeleteSecret(name, actor)
}
func (c *localClient) Close() error { return c.s.Close() }

// --- remote impl: backed by the listener's /v1/secrets API ----------------

type remoteClient struct {
	base   string // base URL without trailing slash
	token  string
	client *http.Client
}

func newRemoteClient(base, token string) (*remoteClient, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("invalid --remote URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("--remote must be http(s): got %s", base)
	}
	return &remoteClient{
		base:  strings.TrimRight(base, "/"),
		token: token,
		// Short overall timeout — the listener responds in low-ms;
		// anything longer indicates trouble worth surfacing.
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func (c *remoteClient) Close() error { return nil }

// auth applies Authorization + X-Cronicle-Actor headers.
func (c *remoteClient) auth(req *http.Request, actor string) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if actor != "" {
		req.Header.Set("X-Cronicle-Actor", actor)
	}
}

// statusError builds a clear error message for non-2xx responses.
// 401/403 get a hint about token configuration since that's the most
// common operator slip.
func statusError(resp *http.Response, op string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	hint := ""
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		hint = " — check --token / CRONICLE_LISTEN_TOKEN"
	}
	return fmt.Errorf("%s: %s%s: %s", op, resp.Status, hint, strings.TrimSpace(string(body)))
}

func (c *remoteClient) Put(name, value, actor string) error {
	body, err := json.Marshal(struct {
		Value string `json:"value"`
	}{value})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, c.base+"/v1/secrets/"+name, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req, actor)
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return statusError(resp, "PUT secret")
}

// Get over HTTP currently fetches the full map and picks one — the
// listener returns the whole envelope on /v1/secrets. The wire cost
// is small (single-digit KB for typical secret sets) and avoids
// growing the listener's API surface for the rare CLI-side `secret
// get` use case.
func (c *remoteClient) Get(name string) (string, bool, error) {
	all, _, err := c.fetchAll()
	if err != nil {
		return "", false, err
	}
	v, ok := all[name]
	return v, ok, nil
}

func (c *remoteClient) List() ([]string, string, error) {
	values, etag, err := c.fetchAll()
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	// Stable order matches the local path.
	sortStrings(names)
	return names, etag, nil
}

func (c *remoteClient) fetchAll() (map[string]string, string, error) {
	req, err := http.NewRequest(http.MethodGet, c.base+"/v1/secrets", nil)
	if err != nil {
		return nil, "", err
	}
	c.auth(req, "")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", statusError(resp, "GET secrets")
	}
	var env struct {
		Values map[string]string `json:"values"`
		ETag   string            `json:"etag"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, "", fmt.Errorf("decode secrets response: %w", err)
	}
	if env.Values == nil {
		env.Values = map[string]string{}
	}
	return env.Values, env.ETag, nil
}

func (c *remoteClient) Delete(name, actor string) (bool, error) {
	req, err := http.NewRequest(http.MethodDelete, c.base+"/v1/secrets/"+name, nil)
	if err != nil {
		return false, err
	}
	c.auth(req, actor)
	resp, err := c.client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, statusError(resp, "DELETE secret")
	}
}

// sortStrings is here because we don't want to drag in sort just for
// the slice form when the package is already stdlib-heavy.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
