# On-call MCP demo server

A deterministic MCP server with on-call tools, so agents can use real tools
locally without any accounts. It stands in for deploy history, Kubernetes
events and metrics, and tells one incident story: **checkout broke right after
deploy v2.31.0**.

| Tool | Returns for `{"service": "checkout"}` |
|---|---|
| `get_recent_deploys` | `v2.31.0 12 minutes ago by alice: switch payment client to pooled connections`, … |
| `get_pod_events` | `BackOff … (x14)`, `Readiness probe failed: connection pool exhausted` |
| `get_error_rate` | `error rate 18.4% (baseline 0.3%), elevated since 11 minutes ago` |

Services: `checkout` (the incident) and `ledger` (healthy). Unknown services
and missing arguments come back as tool errors (`isError: true`), not protocol
errors.

MCP Streamable HTTP, protocol `2025-06-18`: `POST /mcp` with JSON responses
(`initialize` returns `Mcp-Session-Id`), `GET /mcp` → 405 (no server stream),
`GET /healthz`. Standard library only; `make up` runs it in namespace
`mcp-demo` at `http://oncall-mcp.mcp-demo.svc.cluster.local:8080/mcp`.

```bash
make test   # includes these tests
```
