package secretsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

// EnvSource exposes the cronicle process's own env as a secret source.
// Useful for the standalone-CLI shape: the operator exports values via
// the shell (or systemd, docker -e, etc.) and cronicle's task dispatch
// resolves $secret.NAME against them — no external service, no file
// to manage, just process env.
//
// Spec forms:
//
//	env://                 // expose every env var matching UPPER_SNAKE_CASE
//	env://?prefix=APP_     // expose only vars whose name starts with APP_
//	                       // (prefix is stripped from the exposed name)
//	env://?names=A,B,C     // expose only the named vars (no prefix strip)
//
// Why filter at all: a default-everything export would happily hand
// out PATH, HOME, USER, etc. when an HCL author wrote $secret.PATH —
// surprising and arguably leaky. Limiting to UPPER_SNAKE_CASE matches
// the secret-name convention already used elsewhere in cronicle and
// excludes most non-secret OS variables.
//
// Etag: sha256 over the sorted "name=value" lines. Cheap and stable;
// the refresh loop short-circuits when nothing changed.
type EnvSource struct {
	prefix string
	names  map[string]struct{} // nil → "everything matching shape"
	spec   string              // for String()
}

// NewEnvSource constructs an EnvSource from a parsed URL's query string.
// Used by Open; direct callers can construct an empty EnvSource for
// "expose everything UPPER_SNAKE_CASE."
func NewEnvSource(prefix string, names []string) *EnvSource {
	s := &EnvSource{prefix: prefix}
	if len(names) > 0 {
		s.names = make(map[string]struct{}, len(names))
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n != "" {
				s.names[n] = struct{}{}
			}
		}
	}
	parts := []string{"env://"}
	if prefix != "" {
		parts = append(parts, "?prefix="+prefix)
	} else if len(names) > 0 {
		parts = append(parts, "?names="+strings.Join(names, ","))
	}
	s.spec = strings.Join(parts, "")
	return s
}

// looksLikeSecretName matches UPPER_SNAKE_CASE — same shape used by
// cronicle-web's isValidSecretName. Keeps EnvSource from accidentally
// exposing lower-case OS vars like "HOME"/"USER" via $secret.X.
func looksLikeSecretName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
			continue
		case r >= 'A' && r <= 'Z':
			continue
		case r >= '0' && r <= '9' && i > 0:
			continue
		default:
			return false
		}
	}
	// First char must be a letter, not digit/underscore.
	r := rune(s[0])
	return r >= 'A' && r <= 'Z'
}

func (s *EnvSource) Fetch(_ context.Context, prevEtag string) (map[string]string, string, bool, error) {
	out := map[string]string{}
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		name, value := kv[:eq], kv[eq+1:]
		// names= filter takes precedence over prefix=, mirroring how
		// most CLIs treat exact lists as more specific than patterns.
		if s.names != nil {
			if _, ok := s.names[name]; !ok {
				continue
			}
		} else if s.prefix != "" {
			if !strings.HasPrefix(name, s.prefix) {
				continue
			}
			name = strings.TrimPrefix(name, s.prefix)
		} else {
			if !looksLikeSecretName(name) {
				continue
			}
		}
		out[name] = value
	}
	etag := envEtag(out)
	if etag == prevEtag {
		return nil, etag, false, nil
	}
	return out, etag, true, nil
}

// envEtag is sha256 over "k=v\n" lines sorted by key. Stable across
// runs so the refresh loop's first post-startup tick reports
// changed=false instead of re-applying an identical map.
func envEtag(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, values[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *EnvSource) String() string { return s.spec }
