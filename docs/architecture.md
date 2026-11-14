# Architecture

Current-state architecture of Agentory. Update this file in the same PR as any
structural change (see `CLAUDE.md` §5).

> Status: **design / pre-implementation**. Components marked _(planned)_ do not
> exist in code yet.

## 1. Vision

Agentory is a Kubernetes-native agent platform where any team, including
non-engineers, can deploy agents from skills and MCP servers, run coding
harnesses (Claude Code, Codex, …) per task, or ship their own multi-agent
containers, and where every agent is reachable over **A2A** and **REST** with
tracing, budgets and approvals built in.

The first use case is **internal on-call agents**: an alert fires, a team's
agent investigates with the team's own tools and runbooks, and it proposes or
takes action under an approval policy that teams can trust.

## 2. Design principles

1. **Agents are Kubernetes resources.** `Agent` and `Task` are CRDs; the UI and
   CLI write CRDs, so GitOps works for engineers and non-engineers alike.
2. **The platform owns the network path, like a service mesh.** Agents reach
   models only through the **LLM gateway** and tools/other agents only through
   the **agent gateway**. Egress is deny-by-default, so this holds even for
   black-box containers.
3. **Secrets never enter agent workloads.** Gateways hold provider keys and
   inject credentials; agents get scoped virtual keys or short-lived tokens.
4. **Observability is a platform property.** Gateways and runtimes emit
   OpenTelemetry (GenAI semantic conventions); users choose the sink
   (Langfuse, MLflow, Jaeger, …) without changing agent code.
5. **The runtime is pluggable.** The operator delegates execution to a
   `RuntimeBackend` interface so we can start with Deployments and Jobs and add
   Agent Substrate later without changing the API (see ADR-0001).
6. **CRDs for durable intent, not per-request traffic.** High-QPS synchronous
   calls go through the gateway as lightweight sessions; `Task` objects are for
   durable, auditable work.

## 3. System overview

```
 Triggers                      Clients
 (PagerDuty · Slack · cron ·   (A2A · REST · UI · kubectl)
  webhook)  (planned)                  │
      │                                ▼
      │                 ┌──────────────────────────────┐
      └───────────────► │  Agent Gateway (agentgateway)│ ◄── A2A between agents
                        │  A2A + MCP · authN/Z · policy│
                        └──────┬────────────────┬──────┘
                               │                │ MCP tool calls
 ┌─────────────────────────────▼──────┐         ▼
 │ Agentory control plane             │   MCP servers (platform + user-supplied)
 │  • Operator: Agent, Task CRDs      │
 │  • RuntimeBackend implementations  │
 │  • (planned) Trigger, TeamBudget,  │
 │    approvals, UI, registry         │
 └──────────────┬─────────────────────┘
                │ creates / reconciles
                ▼
 ┌─────────────────────────────────────────────────────────┐
 │ Agent workloads (per namespace = per team)              │
 │  container agents  → Deployment + Service   (service)   │
 │  declarative agents→ Deployment + Service   (service)   │
 │  harness agents    → Job per Task           (perTask)   │
 │  egress: deny-all except gateways + OTel collector      │
 └──────────────┬──────────────────────────────────────────┘
                │ model calls (OpenAI/Anthropic-compatible)
                ▼
 ┌──────────────────────────────┐      ┌─────────────────────────────┐
 │ LLM Gateway (Bifrost)        │      │ OTel Collector              │
 │ virtual key per agent/team,  │      │ → Langfuse | MLflow | Jaeger│
 │ routing, fallbacks, budgets  │      └─────────────────────────────┘
 └──────────────────────────────┘          ▲ spans from gateways + runtimes
```

## 4. Components

| Component | Responsibility | Built or reused |
|---|---|---|
| **Operator** | Reconciles `Agent` and `Task`; picks a `RuntimeBackend`; creates workloads, Services, NetworkPolicies, gateway routes, virtual keys, agent cards | Built (Go, kubebuilder) |
| **RuntimeBackend** | Interface for running agents. v1: `DeploymentBackend`, `JobBackend`. Later: `SubstrateBackend` | Built |
| **Declarative runtime** | Agentory's own harness for non-developer agents: instructions + skills + MCP tools, served over A2A, emits OTel spans | Built (Python) |
| **Harness images** | Thin wrappers that run Claude Code / Codex for one `Task` and report results | Built (thin) |
| **LLM gateway** | All model traffic; virtual keys, routing, fallbacks, cost data | Reused: **Bifrost** |
| **Agent gateway** | A2A routing between agents, MCP proxy/tool filtering, JWT policy | Reused: **agentgateway** |
| **OTel collector** | Receives spans, fans out to the configured sink | Reused: OpenTelemetry Collector |
| **Trace sink** | LLM traces, cost, evaluation | Reused: **Langfuse** (default) or **MLflow** |

## 5. Agent types

Two orthogonal fields replace a fixed list of agent types (HLD-0001):

| `runtime` → / `lifecycle` ↓ | `harness` (Claude Code, Codex) | `container` (your image) | `declarative` (skills + MCP) |
|---|---|---|---|
| `perTask` (start, work, exit) | **Type 1**: coding / task agents | Batch jobs | One-shot jobs |
| `service` (long-lived) | n/a | **Type 2**: multi-agent systems | **Type 3**: non-developer agents |
| `suspendable` (planned) | Later | Later | Later (needs Substrate) |

## 6. Key decisions

Recorded as ADRs in [`adr/`](adr/):

- [ADR-0001](adr/0001-own-operator-with-pluggable-runtimes.md): own thin
  operator with pluggable runtime backends (Option B); kagent v0.10.x
  (Option A) kept as a documented alternative; Agent Substrate deferred.
- [ADR-0002](adr/0002-gateways-and-observability.md): Bifrost for model
  traffic, agentgateway for A2A/MCP, OpenTelemetry as the observability
  contract.

## 7. Prior art and what we borrow

| Project | What we take |
|---|---|
| kagent | Agent/harness split, A2A gateway behaviour, HITL approval protocol, telemetry contract, egress credential injection |
| Agent Substrate | Future `SubstrateBackend`: actors, snapshot/resume, gVisor |
| AWS Open Agentic Platform | Reference wiring for Bifrost, agentgateway, Langfuse/OTel, online evals |
| Paperclip | Harness adapters, budgets and governance ideas |
| Uber agent platform | Omni-MCP discovery tools, response projection, actor-chain identity, tools disabled until reviewed |

## 8. Out of scope (for now)

- Snapshot/suspend density, live-state forks, gVisor/Kata isolation (backlog,
  via `SubstrateBackend`).
- Multi-cluster / hub-and-spoke topology.
- Running untrusted third-party code: not safe until sandbox isolation lands.
