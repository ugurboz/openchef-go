# Spikes

Throwaway-but-reproducible programs that verify how the agent CLIs behave. They are a
separate Go module so they never affect the main build. Findings:
[`docs/research/m1-findings.md`](../docs/research/m1-findings.md).

> Spikes run **real** sessions on your logged-in subscriptions. They use the cheapest
> models and a handful of short turns; a full run costs roughly 1 % of a 5-hour window.

```sh
cd spikes
go build -o /tmp/codex-spike  ./codex-appserver && /tmp/codex-spike  -out /tmp/m1-codex
go build -o /tmp/claude-spike ./claude-cli      && /tmp/claude-spike -out /tmp/m1-claude
go build -o /tmp/agy-spike    ./agy-acp         && /tmp/agy-spike    -out /tmp/m1-agy
```

Build to a fixed path first: the spikes re-execute themselves as a minimal MCP server.

Each run writes `*-findings.json` and JSONL transcripts. Transcripts are scrubbed
(home/work paths, host and user names, e-mail addresses, account and installation IDs,
tokens), but **review them before committing** to `testdata/`.
