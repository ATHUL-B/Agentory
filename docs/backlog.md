# Backlog

Status: `todo` | `in-progress` | `done`. Update in the same PR as the work.
Priority: P0 (prototype), P1 (on-call MVP), P2 (later).

## Prototype (P0)

| ID | Item | Priority | Status | Design | PR |
|----|------|----------|--------|--------|----|
| 1 | Architecture, ADRs and Agent/Task HLD | P0 | in-progress | HLD-0001 | - |
| 2 | Local dev cluster: kind + Calico, Bifrost, agentgateway, OTel collector → Jaeger; `make up` / `make smoke` / `make check` / `make down`. Verified on macOS + Colima: egress lockdown, virtual-key auth, routing to provider and traces to Jaeger. A 200 from a real provider key is still unverified | P0 | done | ADR-0002 | #3 |
| 2a | CI: run `make check` and `make test` (incl. operator envtest + lint) on every PR (GitHub Actions); kubebuilder's scaffolded lint/test/e2e workflows are a starting point (regenerate with `kubebuilder edit` or copy from a fresh scaffold) | P0 | todo | - | - |
| 2b | Local Langfuse profile (cert-manager + ClickHouse operator), collector exporter switch | P1 | todo | ADR-0002 amendment | - |
| 3 | Operator scaffold (kubebuilder), `Agent` + `Task` CRDs with CEL validation; controllers record revision/acceptance and resolve Task→Agent; `make operator-up` deploys to kind | P0 | done | HLD-0001 §3 | - |
| 4 | `DeploymentBackend` + `runtime: container` + egress NetworkPolicy + gateway env injection | P0 | todo | HLD-0001 §5.1, §6 | - |
| 5 | Bifrost virtual key per agent (`LLMGatewayClient`) | P0 | todo | ADR-0002 | - |
| 6 | A2A route + generated Agent Card on agentgateway | P0 | todo | HLD-0001 §7 | - |
| 7 | `JobBackend` + `runtime: harness` (Claude Code first) + Task lifecycle and results | P0 | todo | HLD-0001 §5.3 | - |
| 8 | Declarative runtime (Python): A2A server, skills, MCP via gateway, OTel | P0 | todo | HLD-0001 §5.2 | - |
| 9 | End-to-end demo script (four scenarios) | P0 | todo | HLD-0001 §9 | - |
| 9a | Mock LLM provider (`mock/mock-model`) behind Bifrost: deterministic replies, tool calls, streaming, error injection; used by `make smoke` | P0 | done | HLD-0001 §9 | - |

## On-call MVP (P1)

| ID | Item | Priority | Status | Design | PR |
|----|------|----------|--------|--------|----|
| 10 | `Trigger` CRD: PagerDuty/Opsgenie, Slack mention, cron, webhook → Task | P1 | todo | HLD needed | - |
| 11 | Human approval: Task `AwaitingInput`, Slack approve/deny, per-tool policy at MCP gateway | P1 | todo | HLD needed | - |
| 12 | `TeamBudget`: per-namespace token/cost limits mapped to Bifrost virtual keys; queue instead of fail | P1 | todo | HLD needed | - |
| 13 | Web UI for non-developers: create/edit declarative agents, connect MCP servers (writes CRDs) | P1 | todo | HLD needed | - |
| 14 | Runbook → skill conversion with review diff | P1 | todo | HLD needed | - |
| 15 | On-call agent templates (triage, investigate, remediate-with-approval) | P1 | todo | - | - |
| 16 | Incident timeline view (readable trace) | P1 | todo | HLD needed | - |
| 17 | SSO, team workspaces (namespace per team), group sync | P1 | todo | HLD needed | - |

## Later (P2)

| ID | Item | Priority | Status | Design | PR |
|----|------|----------|--------|--------|----|
| 18 | **Agent Substrate runtime backend** (`SubstrateBackend`, `lifecycle: suspendable`): golden snapshots, suspend when idle, live-state forks, gVisor. Directly or via kagent 1.x. Needed for untrusted code or many mostly-idle agents | P2 | todo | ADR-0001 | - |
| 19 | Gateway-recorded replay from step N; incident → regression test | P2 | todo | HLD-0001 §8 | - |
| 20 | Shadow mode: run a candidate agent alongside humans and score agreement | P2 | todo | - | - |
| 21 | Approval ladder: tools promote suggest → approve → auto based on track record | P2 | todo | - | - |
| 22 | agent-sandbox warm pool for harness Jobs (faster start) and Kata isolation | P2 | todo | - | - |
| 23 | Actor-chain JWTs for A2A (Uber-style STS) | P2 | todo | - | - |
| 24 | Omni-MCP discovery tools and response projection | P2 | todo | - | - |
| 25 | Service catalog import (Backstage) for onboarding | P2 | todo | - | - |
| 26 | A2A adapter sidecar for plain-HTTP containers | P2 | todo | HLD-0001 §5.1 | - |
| 27 | Option A fallback: `KagentBackend` on kagent v0.10.x, only if time-to-demo outweighs API ownership | P2 | todo | ADR-0001 | - |
