package cronicle

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/fatih/color"
)

// A shell task's command is shown two ways:
//
//   - commandLines, in the frame header, for reading: an inline script
//     (sh -c, python -c, node -e, …) on its own lines, syntax-highlighted.
//   - shellQuote, for pasting: one line that bash or zsh runs as the same
//     argv. It's the JSON/text `command` field and the rerun line under a
//     failed block.

// shellQuote renders argv as a single line that can be pasted into bash or
// zsh and runs the same argv. Plain words stay bare, anything else is
// single-quoted, and args holding newlines or other control characters
// (sh -c heredoc scripts) use $'…' so the line never breaks.
func shellQuote(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = shellQuoteArg(a)
	}
	return strings.Join(out, " ")
}

func shellQuoteArg(a string) string {
	switch {
	case a == "":
		return "''"
	case isBareWord(a):
		return a
	case !hasControl(a):
		return singleQuote(a)
	}
	var b strings.Builder
	b.WriteString("$'")
	for _, r := range a {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteString("'")
	return b.String()
}

// commandLines renders argv for the frame header. A multi-line inline
// script goes under the quoted args before it, one highlighted line per
// row with tabs expanded; any args after it close the block. Everything
// else is a single shellQuote'd line.
func commandLines(argv []string) []string {
	script, lang := inlineScript(argv)
	if script < 0 || !strings.Contains(strings.TrimRight(argv[script], "\n"), "\n") {
		return []string{shellQuote(argv)}
	}
	lines := []string{shellQuote(argv[:script])}
	for _, line := range highlightLines(strings.TrimRight(argv[script], "\n"), lang) {
		lines = append(lines, "  "+strings.ReplaceAll(line, "\t", "    "))
	}
	if rest := argv[script+1:]; len(rest) > 0 {
		lines = append(lines, shellQuote(rest))
	}
	return lines
}

// inlineScript finds the argument holding an inline script — the one after
// -c / -e for a known interpreter, e.g. `sh -c <script>` or
// `uv run --with rich python -c <script>` — and its chroma language.
// Returns -1 when there's none.
func inlineScript(argv []string) (int, string) {
	lang := ""
	for i, a := range argv {
		if l, ok := scriptLanguages[strings.TrimRight(filepath.Base(a), "0123456789.")]; ok {
			lang = l
			continue
		}
		if lang != "" && (a == "-c" || a == "-e") && i+1 < len(argv) {
			return i + 1, lang
		}
	}
	return -1, ""
}

// scriptLanguages maps interpreter basenames (version suffix stripped, so
// python3.12 → python) to chroma lexer names.
var scriptLanguages = map[string]string{
	"sh": "bash", "bash": "bash", "zsh": "bash", "dash": "bash", "ash": "bash",
	"python": "python", "pypy": "python",
	"node": "javascript", "bun": "javascript",
	"ruby": "ruby",
	"perl": "perl",
}

// highlightLines splits src into lines and colors the tokens on each with
// ANSI codes, or leaves them plain when color is off or lang is unknown.
// Every colored span is closed on its own line: the frame gutter printed
// between lines resets color, so a span left open across a line break
// (a multi-line string) would lose its color after the first line.
func highlightLines(src, lang string) []string {
	var lexer chroma.Lexer
	if !color.NoColor && lang != "" {
		lexer = lexers.Get(lang)
	}
	if lexer == nil {
		return strings.Split(src, "\n")
	}
	it, err := chroma.Coalesce(lexer).Tokenise(nil, src)
	if err != nil {
		return strings.Split(src, "\n")
	}
	var b strings.Builder
	for _, tok := range it.Tokens() {
		code := tokenColor(tok.Type)
		for i, piece := range strings.Split(tok.Value, "\n") {
			if i > 0 {
				b.WriteByte('\n')
			}
			if code == "" || piece == "" {
				b.WriteString(piece)
				continue
			}
			b.WriteString("\x1b[" + code + "m" + piece + "\x1b[0m")
		}
	}
	out := b.String()
	// Some lexers (javascript) append a newline the source didn't have.
	if !strings.HasSuffix(src, "\n") {
		out = strings.TrimSuffix(out, "\n")
	}
	if stripANSI(out) != src {
		return strings.Split(src, "\n") // never show text that differs from what runs
	}
	return strings.Split(out, "\n")
}

// tokenColor picks the ANSI color for a token, coloring only what helps
// scanning a script — keywords, strings, numbers, comments, builtins, shell
// variables. Plain code keeps the terminal's default foreground, and the
// 16 standard colors take the terminal theme's shades, so it reads on
// light and dark backgrounds alike.
func tokenColor(t chroma.TokenType) string {
	switch {
	case t.InCategory(chroma.Comment):
		return "90" // bright black
	case t.InCategory(chroma.Keyword), t == chroma.OperatorWord:
		return "35" // magenta
	case t == chroma.LiteralStringInterpol, t.InSubCategory(chroma.NameVariable):
		return "36" // cyan
	case t.InSubCategory(chroma.LiteralString):
		return "32" // green
	case t.InSubCategory(chroma.LiteralNumber):
		return "33" // yellow
	case t.InSubCategory(chroma.NameBuiltin), t.InSubCategory(chroma.NameFunction):
		return "34" // blue
	}
	return ""
}

func isBareWord(a string) bool {
	// "=" is left out: zsh expands a word starting with it (=ls → /bin/ls).
	return !strings.ContainsFunc(a, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("_-./:,+@%", r))
	})
}

// hasControl reports whether a contains a control character.
func hasControl(a string) bool {
	return strings.ContainsFunc(a, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func singleQuote(a string) string {
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}
