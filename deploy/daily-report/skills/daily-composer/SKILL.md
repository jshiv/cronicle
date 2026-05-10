---
name: daily-composer
description: Compose a unified daily team report from per-source markdown files in a scratch directory. Use when the prompt asks to merge crawler outputs (slack, discord, email, code) into a single morning brief.
allowed-tools:
  - text_editor
  - bash
---

# Daily Composer

You will receive a scratch directory path containing per-source markdown files produced by upstream crawler tasks. Read each one and compose a single unified daily report.

Steps:

1. List the scratch dir to see which sources produced output. Expected files (any subset is fine; the others are crawlers that didn't run or didn't have content):
   - `slack.md` — what happened in Slack
   - `discord.md` — what happened in Discord
   - `email.md` — what came through email
   - `code.md` — code changes / merged PRs

2. For each present file, read it via `text_editor view`.

3. Compose a unified report at `${scratch}/REPORT.md` with this shape:

   ```markdown
   # Daily Report — <YYYY-MM-DD>

   ## TL;DR
   - one-line summary of what mattered today across all channels

   ## Slack
   - bullet
   - bullet

   ## Discord
   - bullet
   - bullet

   ## Email
   - bullet
   - bullet

   ## Code
   - bullet
   - bullet
   ```

   Skip sections whose source file is missing or empty rather than emitting a "no data" placeholder. The TL;DR is required.

4. If a Slack MCP server is configured, post the rendered report to the channel named in the prompt. Otherwise, just leave the file in the scratch dir.

5. Reply with the literal lines:
   ```
   REPORT: ${scratch}/REPORT.md
   COMPOSED.
   ```
