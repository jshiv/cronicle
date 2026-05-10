# Operator checklist — first 10 minutes with cronicle

Before you ship a production schedule:

- [ ] `cronicle init --path ./cron` and version-control the result.
- [ ] Pin a known-good binary via `CRONICLE_VERSION=v0.5.0` (or whatever
  release you've validated) when installing on production hosts.
- [ ] Set `CRONICLE_LISTEN_TOKEN` and pass `--listen-token "$TOKEN"`.
  An open trigger endpoint on an unattended cron service is a
  foot-cannon; the listener refuses to bind without a token anyway.
- [ ] For distributed mode: `--worker=false` on the producer if all
  execution should go to remote workers.
- [ ] `--log-to-file` if you want the on-disk audit trail
  (`.cronicle/log/cronicle.jsonl`, rotated by lumberjack).
- [ ] Plan how cancel propagates: shell tasks honor `ctx` via
  `exec.CommandContext`; agent tasks honor `ctx.Done()` between
  turns. Tool calls in flight finish before the next check.
- [ ] Decide whether you need `${scratch}` cross-task context or
  whether each task should be standalone.
