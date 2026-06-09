package cronicle

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"unicode/utf8"
)

// TestUnescapeHCLDollars_PreservesNonTemplateRuns is the L1 invariant:
// the previous `[$]+` regex collapsed any run of '$' to a single '$',
// damaging legitimate shell `$$` (PID) and awk `$$0` references. The
// targeted replacement only touches the actual gohcl escape pattern
// `$${`.
func TestUnescapeHCLDollars_PreservesNonTemplateRuns(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain ASCII", "echo hello", "echo hello"},
		{"unescapes gohcl-doubled template", "$${date}", "${date}"},
		{"preserves shell PID $$", "echo $$", "echo $$"},
		{"preserves awk $$0", "awk '{print $$0}'", "awk '{print $$0}'"},
		{"mixed template + literal $$", "$${date} log $$.txt", "${date} log $$.txt"},
		{"triple $ not followed by { stays put", "$$$ payday", "$$$ payday"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(unescapeHCLDollars([]byte(tc.in)))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRedactGitURL strips userinfo from URLs so embedded tokens don't
// reach Loki / SSE through git-clone log lines (L5).
func TestRedactGitURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			"https with token in password slot",
			"https://x:ghp_abc123@github.com/org/repo.git",
			"https://github.com/org/repo.git",
		},
		{
			"https with only username",
			"https://alice@gitlab.example/org/repo.git",
			"https://gitlab.example/org/repo.git",
		},
		{
			"https without credentials passes through",
			"https://github.com/org/repo.git",
			"https://github.com/org/repo.git",
		},
		{
			"ssh-style passes through (no inline creds possible)",
			"git@github.com:org/repo.git",
			"git@github.com:org/repo.git",
		},
		{
			"local path passes through",
			"/tmp/repo.git",
			"/tmp/repo.git",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactGitURL(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestIsLikelySecretFilename verifies the L6 sensitive-name detector
// catches the common credential filenames while leaving normal source
// files and public-key counterparts alone.
func TestIsLikelySecretFilename(t *testing.T) {
	hits := []string{
		".env",
		".env.production",
		"path/to/.env.local",
		"keys/server.pem",
		"id_rsa",
		"id_ed25519",
		".netrc",
		"_netrc",
		"subdir/server.key",
	}
	misses := []string{
		"README.md",
		"main.go",
		"id_rsa.pub", // public key — explicitly excluded
		"id_ed25519.pub",
		"build.env.example", // doesn't end in .env
		".envrc",            // direnv config, not credentials
		"my_module.go",
	}
	for _, p := range hits {
		t.Run("hit/"+p, func(t *testing.T) {
			if !isLikelySecretFilename(p) {
				t.Errorf("isLikelySecretFilename(%q) = false, want true", p)
			}
		})
	}
	for _, p := range misses {
		t.Run("miss/"+p, func(t *testing.T) {
			if isLikelySecretFilename(p) {
				t.Errorf("isLikelySecretFilename(%q) = true, want false", p)
			}
		})
	}
}

// TestTruncate_RuneSafe covers the L7 invariant: truncation must not
// land mid-rune for multi-byte UTF-8 input. Slicing by byte (the old
// code) produces invalid UTF-8 when the cut falls inside a multi-byte
// codepoint.
func TestTruncate_RuneSafe(t *testing.T) {
	// "café" — len("café")==5 bytes (4 runes), the é is 2 bytes.
	// Byte-slicing s[:3] yields "ca" + first byte of é = invalid UTF-8.
	const cafe = "café"
	got := truncate(cafe, 3)
	if !isValidUTF8(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
	// Same idea but with a longer multi-byte string.
	emoji := "smoke 🚀 test 🔥"
	got2 := truncate(emoji, 8)
	if !isValidUTF8(got2) {
		t.Errorf("truncate of emoji produced invalid UTF-8: %q", got2)
	}
	// Short ASCII pass-through unchanged.
	if got := truncate("hi", 10); got != "hi" {
		t.Errorf("short input mutated; got %q", got)
	}
}

// TestIsNoRows_RecognizesErrNoRowsViaErrorsIs is the L9 invariant: the
// previous strings.Contains(err.Error(), "no rows") was sloppy and
// would have matched any error whose message merely included "no rows"
// — e.g. a future "no rows in expected join" validation error.
// errors.Is walks the wrap chain properly.
func TestIsNoRows_RecognizesErrNoRowsViaErrorsIs(t *testing.T) {
	if !isNoRows(sql.ErrNoRows) {
		t.Errorf("expected isNoRows(sql.ErrNoRows) = true")
	}
	wrapped := fmt.Errorf("lookup run X: %w", sql.ErrNoRows)
	if !isNoRows(wrapped) {
		t.Errorf("expected isNoRows to walk wrap chain")
	}
	// A non-sql error whose message happens to mention "no rows" must
	// NOT match — the substring trap.
	bogus := errors.New("validation: query produced no rows in expected join")
	if isNoRows(bogus) {
		t.Errorf("isNoRows false-matched on non-sql error: %v", bogus)
	}
}

// isValidUTF8 reports whether s is well-formed UTF-8. We only care
// about catching truncation that lands mid-rune; utf8.ValidString
// detects that property directly.
func isValidUTF8(s string) bool {
	return utf8.ValidString(s)
}
