package secretstore

import (
	"context"
	"strings"
	"testing"
)

func newStoreWith(t *testing.T, values map[string]string) *Store {
	t.Helper()
	be := memBackend(t)
	for k, v := range values {
		if err := be.PutSecret(k, v, "test"); err != nil {
			t.Fatalf("PutSecret: %v", err)
		}
	}
	s := New().SetBackend(be).StaleTTL(0)
	if err := s.Start(context.Background(), nil, 0); err != nil {
		t.Fatalf("start: %v", err)
	}
	return s
}

func TestExpandValue_basicReplacement(t *testing.T) {
	s := newStoreWith(t, map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-xxx",
		"GITHUB_TOKEN":      "ghp_yyy",
	})

	cases := []struct {
		in, want string
	}{
		{"ANTHROPIC_API_KEY=$secret.ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY=sk-ant-xxx"},
		{"GITHUB_TOKEN=$secret.GITHUB_TOKEN", "GITHUB_TOKEN=ghp_yyy"},
		{"PLAIN=hello", "PLAIN=hello"},
		{"COMBINED=prefix-$secret.GITHUB_TOKEN-suffix", "COMBINED=prefix-ghp_yyy-suffix"},
	}
	for _, c := range cases {
		got, err := s.ExpandValue(c.in)
		if err != nil {
			t.Errorf("ExpandValue(%q): err %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ExpandValue(%q): want %q, got %q", c.in, c.want, got)
		}
	}
}

func TestExpandValue_unresolvedErrors(t *testing.T) {
	s := newStoreWith(t, map[string]string{"FOO": "bar"})
	_, err := s.ExpandValue("X=$secret.MISSING_ONE Y=$secret.MISSING_TWO")
	if err == nil {
		t.Fatal("missing refs should error")
	}
	for _, want := range []string{"MISSING_ONE", "MISSING_TWO"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %q", want, err.Error())
		}
	}
}

func TestExpandValue_dollarEscape(t *testing.T) {
	s := newStoreWith(t, map[string]string{"FOO": "bar"})
	got, err := s.ExpandValue("LITERAL=$$secret.FOO")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "LITERAL=$secret.FOO" {
		t.Errorf("escape: want LITERAL=$secret.FOO, got %q", got)
	}
}

func TestExpandEnv_partialFailureReturnsFirstError(t *testing.T) {
	s := newStoreWith(t, map[string]string{"OK": "value"})
	_, err := s.ExpandEnv([]string{
		"GOOD=$secret.OK",
		"BAD=$secret.NOPE",
	})
	if err == nil {
		t.Fatal("partial failure should error")
	}
	if !strings.Contains(err.Error(), "NOPE") {
		t.Errorf("error should mention missing name, got %q", err.Error())
	}
}

func TestLookupRef(t *testing.T) {
	s := newStoreWith(t, map[string]string{"FOO": "bar"})

	v, ok, err := s.LookupRef("$secret.FOO")
	if err != nil || !ok || v != "bar" {
		t.Errorf("hit: v=%q ok=%v err=%v", v, ok, err)
	}

	// Non-ref string → ok=false, no error.
	_, ok, err = s.LookupRef("plain-value")
	if ok || err != nil {
		t.Errorf("non-ref: ok=%v err=%v", ok, err)
	}

	// Ref that doesn't resolve → error.
	if _, _, err := s.LookupRef("$secret.MISSING"); err == nil {
		t.Errorf("missing ref should error")
	}
}
