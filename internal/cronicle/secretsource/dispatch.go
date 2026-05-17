package secretsource

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Open parses a secret-source spec and returns the matching Source.
//
// Accepted forms:
//   - file:///abs/path/secrets.json
//   - /abs/path/secrets.json     (no scheme → treated as file)
//   - relative/path.json         (no scheme → treated as file)
//   - http://host/path
//   - https://host/path
//
// The no-scheme fallback mirrors configsource so operators don't need
// to learn two different conventions. Anything containing "://" is
// dispatched by URL scheme.
//
// ctx is currently unused — file and http construction don't need a
// round-trip — but it's part of the signature so future sources
// (vault, kms, etc.) can use it without breaking callers.
func Open(_ context.Context, spec string) (Source, error) {
	if spec == "" {
		return nil, fmt.Errorf("secret source spec is empty")
	}
	if !strings.Contains(spec, "://") {
		return NewFileSource(spec)
	}
	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid secret source url: %w", err)
	}
	switch u.Scheme {
	case "file":
		if u.Host != "" && u.Host != "localhost" {
			return nil, fmt.Errorf(
				"file:// requires an absolute path (use file:///path or just /path), got host=%q in %q",
				u.Host, spec)
		}
		return NewFileSource(u.Path)
	case "http", "https":
		return NewHTTPSource(spec)
	case "env":
		// env:// exposes the cronicle process's own env vars. Operator
		// scopes via ?prefix=APP_ or ?names=A,B,C; bare env:// exposes
		// every UPPER_SNAKE_CASE name (matches secret-name convention).
		q := u.Query()
		prefix := q.Get("prefix")
		var names []string
		if v := q.Get("names"); v != "" {
			for _, n := range strings.Split(v, ",") {
				if n = strings.TrimSpace(n); n != "" {
					names = append(names, n)
				}
			}
		}
		return NewEnvSource(prefix, names), nil
	default:
		return nil, fmt.Errorf("unsupported secret source scheme %q in %q (want env/file/http/https)",
			u.Scheme, spec)
	}
}
