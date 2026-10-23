package cronicle

import (
	osexec "os/exec"
	"strings"
	"testing"

	"github.com/fatih/color"
)

// roundTripArgvs covers the shapes a task command takes: plain words,
// sh -c / python -c heredoc scripts (with and without quotes of their
// own), and characters bash or zsh would otherwise expand.
var roundTripArgvs = [][]string{
	{"/bin/echo", "hello", "--date=2026-10-22"},
	{"/bin/sh", "-c", "echo \"building ${date}\"\nfor r in a b; do\n\techo \"$r\"\ndone\n"},
	{"uv", "run", "--with", "rich", "python", "-c", "import sys\nprint(\"it's\", sys.argv)\n"},
	{"python3", "-c", "x = 'single'\nprint(x)"},
	{"node", "-e", "const s = 'a' + \"b\" + `c${1}`;\nconsole.log(s)"},
	{"/bin/bash", "-c", "# today's run\ncase \"$1\" in\n  a) echo 'a' ;;\nesac\ncount=$(ls | wc -l | tr -d ' ')\n"},
	{"/bin/bash", "-c", "echo 'no trailing newline' $HOME"},
	{"sh", "-c", "echo '$x'\nEOF\necho done\n"},
	{"echo", "tab\there", `back\slash`, "=equals", "~tilde", "a b", "$HOME", "`x`", "!bang", ""},
}

// TestShellQuote_RoundTrips: the rerun line under a failed block (and the
// logged `command`) must paste into bash and zsh and run the same argv,
// byte-for-byte, on one line.
func TestShellQuote_RoundTrips(t *testing.T) {
	for _, shell := range []string{"bash", "zsh"} {
		path, err := osexec.LookPath(shell)
		if err != nil {
			t.Logf("%s not installed; skipping", shell)
			continue
		}
		for _, argv := range roundTripArgvs {
			// Quote a printf of the argv so the shell prints each arg
			// NUL-terminated and the check is exact.
			line := shellQuote(append([]string{"printf", `%s\0`}, argv...))
			if strings.Contains(line, "\n") {
				t.Errorf("shellQuote spans multiple lines: %q", line)
			}
			out, err := osexec.Command(path, "-c", line).Output()
			if err != nil {
				t.Fatalf("%s: %v running %q", shell, err, line)
			}
			got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
			if strings.Join(got, "\x01") != strings.Join(argv, "\x01") || len(got) != len(argv) {
				t.Errorf("%s round-trip of %q\n got  %q\n want %q", shell, line, got, argv)
			}
		}
	}
}

// TestCommandLines: a multi-line inline script reads as code under the
// command that runs it; anything else is one quoted line.
func TestCommandLines(t *testing.T) {
	prev := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = prev }()

	cases := []struct {
		argv []string
		want []string
	}{
		{[]string{"echo", "hello world"}, []string{"echo 'hello world'"}},
		{[]string{"sh", "-c", "echo one-liner"}, []string{"sh -c 'echo one-liner'"}},
		{
			[]string{"uv", "run", "python", "-c", "import os\nif True:\n\tprint(os.getcwd())\n"},
			[]string{"uv run python -c", "  import os", "  if True:", "      print(os.getcwd())"},
		},
		{
			[]string{"bash", "-c", "echo \"$1\"\necho done", "name", "arg one"},
			[]string{"bash -c", `  echo "$1"`, "  echo done", "name 'arg one'"},
		},
	}
	for _, c := range cases {
		if got := commandLines(c.argv); strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("commandLines(%q)\n got  %q\n want %q", c.argv, got, c.want)
		}
	}
}

// TestHighlightLines: highlighting colors the code without changing its
// text, and closes every colored span on the line it starts — a span left
// open would be cut off by the gutter printed before the next line.
func TestHighlightLines(t *testing.T) {
	prev := color.NoColor
	color.NoColor = false
	defer func() { color.NoColor = prev }()

	src := "import os\ndoc = \"\"\"spans\nlines\"\"\"\nprint(doc)"
	lines := highlightLines(src, "python")
	if got := stripANSI(strings.Join(lines, "\n")); got != src {
		t.Fatalf("highlighting changed the text:\n got  %q\n want %q", got, src)
	}
	if !strings.Contains(lines[0], "\x1b[35mimport\x1b[0m") {
		t.Errorf("keyword not colored: %q", lines[0])
	}
	for i, line := range lines {
		if strings.Count(line, "\x1b[0m") != strings.Count(line, "\x1b[")-strings.Count(line, "\x1b[0m") {
			t.Errorf("line %d leaves a color span open: %q", i, line)
		}
	}
	if !strings.HasPrefix(lines[2], "\x1b[32m") {
		t.Errorf("multi-line string lost its color on its second line: %q", lines[2])
	}
}

func TestInlineScript(t *testing.T) {
	cases := []struct {
		argv []string
		idx  int
		lang string
	}{
		{[]string{"/bin/sh", "-c", "x"}, 2, "bash"},
		{[]string{"uv", "run", "--with", "rich", "python3.12", "-c", "x"}, 6, "python"},
		{[]string{"node", "-e", "x"}, 2, "javascript"},
		{[]string{"/bin/echo", "-c", "x"}, -1, ""},
		{[]string{"python", "script.py"}, -1, ""},
	}
	for _, c := range cases {
		if idx, lang := inlineScript(c.argv); idx != c.idx || lang != c.lang {
			t.Errorf("inlineScript(%q) = %d, %q; want %d, %q", c.argv, idx, lang, c.idx, c.lang)
		}
	}
}
