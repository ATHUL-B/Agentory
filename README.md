# Agentory

A Kubernetes-native agent platform. Teams, including non-engineers, deploy
agents from skills and MCP servers, run coding harnesses (Claude Code, Codex)
per task, or ship their own multi-agent containers. Every agent is reachable
over A2A and REST, with model and tool traffic routed through gateways and
tracing, budgets and approvals built in.

First use case: **internal on-call agents** that teams can onboard quickly
and trust.

> Status: design phase. See [`docs/`](docs/).

## Docs

- [Architecture](docs/architecture.md)
- [Decisions (ADRs)](docs/adr/)
- [High-level designs](docs/hld/)
- [Backlog](docs/backlog.md)

## Getting Started

```bash
git clone git@github.com:ATHUL-B/Agentory.git
cd Agentory
```

Local setup instructions arrive with the dev-cluster work (backlog item 2).

## Contributing

Read [`CLAUDE.md`](CLAUDE.md) for engineering guidelines: one PR per feature,
small commits, tests for every feature, and docs updated with every change.

## License

TBD
