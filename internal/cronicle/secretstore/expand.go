package secretstore

import (
	"fmt"
	"regexp"
	"strings"
)

// SECRET_REF_RE matches $secret.NAME references in a value string.
// Mirrors the cronicle-web/cronicle-infra convention. The negative
// lookbehind isn't supported in Go's RE2 engine, so we handle the
// $$secret.X literal-dollar escape with a post-filter below.
//
// Name shape: UPPER_SNAKE — first char A-Z, rest A-Z/0-9/_. Anything
// else fails validation upstream and would never appear here.
var SECRET_REF_RE = regexp.MustCompile(`\$secret\.([A-Z][A-Z0-9_]*)`)

// ExpandValue replaces every $secret.NAME in s with the matching
// value from the Store. Unresolved references produce a clear error
// listing all the missing names — partial expansion is never
// returned, so callers don't have to inspect the result for "did
// this still contain a $secret reference?".
//
// Escape: a literal "$$secret.X" is preserved as "$secret.X" (one
// dollar, no expansion). Matches the convention HCL/shell tooling
// uses to opt out of expansion.
func (s *Store) ExpandValue(in string) (string, error) {
	// Handle escape first by replacing $$ → \x00\x01 placeholder so
	// the regex below doesn't see them, then restore at the end.
	const escSentinel = "\x00\x01"
	work := strings.ReplaceAll(in, "$$", escSentinel)

	var missing []string
	out := SECRET_REF_RE.ReplaceAllStringFunc(work, func(match string) string {
		// Re-extract the name from the match. ReplaceAllStringFunc gives
		// us the whole match, not the capture group, so we slice past
		// the literal "$secret." prefix.
		name := strings.TrimPrefix(match, "$secret.")
		v, ok, err := s.Get(name)
		if err != nil {
			// Surface the store-level error via missing — the caller's
			// "missing references" error message subsumes it.
			missing = append(missing, name+" ("+err.Error()+")")
			return match
		}
		if !ok {
			missing = append(missing, name)
			return match
		}
		return v
	})
	out = strings.ReplaceAll(out, escSentinel, "$")
	if len(missing) > 0 {
		return "", fmt.Errorf("unresolved secret references: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// ExpandEnv applies ExpandValue to every entry of an env slice (each
// "KEY=VALUE" line). Returns a new slice; the input is not mutated.
// On any unresolved reference, returns the (partial, but unused)
// slice and the error — callers MUST check err before using the
// slice. We return the partial work too so test assertions can show
// where the expansion broke.
func (s *Store) ExpandEnv(env []string) ([]string, error) {
	out := make([]string, 0, len(env))
	var firstErr error
	for _, entry := range env {
		// We expand the whole line, including the key. Practically the
		// $secret. references only appear in the VALUE side of
		// KEY=VALUE, but expanding the line means we don't need to
		// parse the KEY=VALUE split and handle quoting subtleties.
		expanded, err := s.ExpandValue(entry)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		out = append(out, expanded)
	}
	return out, firstErr
}

// LookupRef returns the value of a bare "$secret.NAME" string, or the
// resolved value when name was found. Convenience for task dispatch
// pulling a single value (e.g. ANTHROPIC_API_KEY for cfg.APIKey)
// without going through the full env slice.
//
// Returns ("", false, nil) when ref doesn't match the $secret.NAME
// pattern — callers can pass any string and act on the boolean.
func (s *Store) LookupRef(ref string) (string, bool, error) {
	m := SECRET_REF_RE.FindStringSubmatch(ref)
	if len(m) != 2 || m[0] != ref {
		return "", false, nil
	}
	v, ok, err := s.Get(m[1])
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, fmt.Errorf("unresolved secret reference: %s", ref)
	}
	return v, true, nil
}
