---
name: file-summarizer
description: |
  Read one or more text files and produce a single-paragraph digest in
  a consistent voice. Use when the operator wants a quick survey of a
  directory's contents rather than per-file summaries.
---

# file-summarizer

## When to use

Use this skill when you've been given a directory (typically via the
`fs__list_directory` and `fs__read_text_file` tools from the MCP
filesystem server) and the operator wants one summary covering all
of it, not file-by-file digests.

## How to summarize

- **One paragraph, 80–120 words.** Resist the urge to bullet — this
  is a digest, not a manifest.
- **Lead with the through-line.** What do these files have in common?
  Where do they diverge? Open the paragraph with that frame.
- **Name files inline** when a specific file carries a key point.
  Use the basename only (e.g. `release-notes.md`), not the full path.
- **No meta commentary.** Don't write "this directory contains…".
  Write the digest as if it were going into a status report.

## Output format

Write to `${scratch}/SUMMARY.md` with exactly this shape:

```markdown
# File summary — <YYYY-MM-DD>

<one paragraph, 80–120 words>

---
_files_: <comma-separated basenames>
```

Replace `<YYYY-MM-DD>` with today's date and `<comma-separated basenames>`
with the file names you actually read.

## When NOT to use

If the operator asks for "summaries" plural (one per file) or
"what's in each file," do that instead — this skill is the
single-paragraph variant.
