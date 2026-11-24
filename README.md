# OpenChef

**An orchestrator for AI coding agents.** OpenChef drives the agent CLIs you already use
(Claude Code, Codex CLI, Antigravity) as one kitchen brigade: it plans work, hands each
step to the right agent, verifies the result itself, and moves the work to another
subscription when one runs out of quota.

> Status: **foundation (milestone M0).** There is no release yet and agents cannot be
> orchestrated yet. The design lives in [`docs/architecture.md`](docs/architecture.md).

## What works today

```sh
go run ./cmd/openchef doctor   # detect claude / codex / agy, versions and logins
go run ./cmd/openchef init     # write ~/.openchef/config.toml for the usable CLIs
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the development setup.

OpenChef is not affiliated with Anthropic, OpenAI, Google, or Progress Software (Chef).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
