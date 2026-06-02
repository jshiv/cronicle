package cronicle

import (
	"reflect"
	"testing"
)

func TestRedactEnv(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, nil},
		{"empty", []string{}, []string{}},
		{
			"basic key=value",
			[]string{"TOKEN=abc123", "OTHER=plain"},
			[]string{"TOKEN=***", "OTHER=***"},
		},
		{
			"empty value still redacted",
			[]string{"EMPTY="},
			[]string{"EMPTY=***"},
		},
		{
			"value containing equals",
			[]string{"URL=https://x.io/?a=b"},
			[]string{"URL=***"},
		},
		{
			"malformed entry (no equals) preserved as-is",
			[]string{"NOEQUALS"},
			[]string{"NOEQUALS"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactEnv(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("redactEnv(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactEnvDoesNotMutateInput(t *testing.T) {
	in := []string{"TOKEN=secret"}
	_ = redactEnv(in)
	if in[0] != "TOKEN=secret" {
		t.Errorf("redactEnv mutated input: %v", in)
	}
}
