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
| 4 | `DeploymentBackend` + `runtime: container` + egress NetworkPolicy + gateway env injection | P0 | done | HLD-0001 §5.1, §6 | - |
| 5 | Bifrost virtual key per agent (`LLMGatewayClient`): least-privilege key, Secret, rotation on loss, finalizer cleanup | P0 | done | ADR-0002, HLD-0001 §4.2 | - |
| 6 | A2A route + generated Agent Card on agentgateway | P0 | done | HLD-0001 §7, §4.3 | - |
| 6a | Enforce `a2a.allowedCallers` at the agent gateway (needs per-agent caller identity: ServiceAccount token or actor JWT, see 23) | P1 | todo | HLD needed | - |
| 6b | MCP routing per agent: `/mcp/<ns>/<agent>` on agentgateway with the agent's `tools.mcpServers` allow-list; platform MCP server registry | P1 | done | HLD-0001 §4.6 | - |
| 6c | Credentials for MCP servers: per-server secret injected by the gateway (`static.policies.auth` / backend auth), never in the agent pod | P1 | todo | - | - |
| 6d | agentgateway tracing to the collector, so MCP and A2A hops appear in the same trace. Today the gateway starts a span it doesn't export, so called agents show as separate branches of the trace (seen in `make demo`, scenario 3) | P1 | todo | - | - |
| 7 | `JobBackend` + `runtime: harness` (Claude Code first) + Task lifecycle and results | P0 | done | HLD-0001 §5.3, §4.4 | - |
| 7a | Harness workspaces: clone `harness.workspace.git` per Task, through a controlled egress exception (git host allow-list via an egress proxy), and push a result branch | P1 | todo | HLD needed | - |
| 7b | Codex harness image (`kind: codex`) | P2 | todo | - | - |
| 7c | Dispatch Tasks to service agents over A2A (`message/send`), recording the reply as the result | P1 | todo | - | - |
| 7d | Cancel a running Task (`Cancelled` phase) and stream harness progress (stream-json) into status/events | P2 | todo | - | - |
| 8 | Declarative runtime (Python): A2A server, skills, MCP via gateway, OTel | P0 | done | HLD-0001 §5.2, §4.5 | - |
| 8a | Skills from git (commit) and OCI (digest): fetch outside the locked-down pod (operator-side into ConfigMaps, or an init step through an egress proxy) | P1 | todo | HLD needed | - |
| 8c | Conversation memory outside the pod (Redis/Postgres) so declarative agents scale past one replica | P1 | todo | - | - |
| 9 | End-to-end demo script (four scenarios): `make demo`, self-checking; BYO multi-agent example; trace tree renderer | P0 | done | HLD-0001 §9 | - |
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
| 28 | **Guardrail adapters.** A `Guardrail` resource that the platform or security team publishes once, and that Agents reference in `spec.guardrails`. Enforced at the gateways, never in agent code, so it also holds for BYO containers: (1) model calls: prompt guards (regex/PII masking, moderation, Bedrock Guardrails, webhook for Llama Guard / NeMo / Presidio). To decide: agentgateway's `backend.ai.promptGuard` in front of Bifrost, or Bifrost's own guardrails (check which edition). (2) MCP tool calls: agentgateway's `backend.mcp.guardrails` remote policy server per JSON-RPC method. (3) A2A messages: webhook. Fail-closed or fail-open per guardrail, a `GuardrailsReady` condition, and a span per verdict | P1 | todo | HLD needed | - |
| 29 | **Long-term memory adapters.** Per-caller and per-team memory (facts, preferences, past incidents), served as an MCP memory server through the agent gateway, so container agents get it too. Isolated by caller identity (needs 6a/23). Providers: mem0 with Milvus or pgvector, AWS AgentCore Memory, Redis. Opt in with `spec.memory`. Prior art: OAP's per-caller memory (AgentCore, mem0+Milvus). Builds on 8c | P1 | todo | HLD needed | - |
| 31 | **Checkpoints, level 1: resume.** Persist conversation and Task state as it happens, so a pod restart, rollout or node loss loses nothing, and a Task picks up where it stopped. Builds on 8c | P1 | todo | HLD needed | - |

## Later (P2)

| ID | Item | Priority | Status | Design | PR |
|----|------|----------|--------|--------|----|
| 18 | **Agent Substrate runtime backend** (`SubstrateBackend`, `lifecycle: suspendable`): golden snapshots, suspend when idle, live-state forks, gVisor. Directly or via kagent 1.x. Needed for untrusted code or many mostly-idle agents. This is checkpoints level 3 (snapshot and fork; levels 1 and 2 are 31 and 32) | P2 | todo | ADR-0001 | - |
| 19 | Gateway-recorded replay from step N; incident → regression test | P2 | todo | HLD-0001 §8 | - |
| 20 | Shadow mode: run a candidate agent alongside humans and score agreement | P2 | todo | - | - |
| 21 | Approval ladder: tools promote suggest → approve → auto based on track record | P2 | todo | - | - |
| 22 | agent-sandbox warm pool for harness Jobs (faster start) and Kata isolation | P2 | todo | - | - |
| 23 | Actor-chain JWTs for A2A (Uber-style STS) | P2 | todo | - | - |
| 24 | Omni-MCP discovery tools and response projection | P2 | todo | - | - |
| 25 | Service catalog import (Backstage) for onboarding | P2 | todo | - | - |
| 26 | A2A adapter sidecar for plain-HTTP containers | P2 | todo | HLD-0001 §5.1 | - |
| 30 | **Shared team knowledge.** Runbooks, postmortems and docs indexed per team, searchable by the team's agents through an MCP server (any vector store). Pairs with 14 (runbook → skill) | P2 | todo | HLD needed | - |
| 32 | **Checkpoints, level 2: retry from a step.** Checkpoint each step of a harness Task (workspace and transcript), so a failed or cancelled Task can be retried or resumed from that step instead of from the start. Needs 7a workspaces | P2 | todo | HLD needed | - |
| 27 | Option A fallback: `KagentBackend` on kagent v0.10.x, only if time-to-demo outweighs API ownership | P2 | todo | ADR-0001 | - |
