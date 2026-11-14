# ADR-0001: Own operator with pluggable runtime backends

- **Status:** Accepted
- **Date:** 2026-11-14

## Context

Agentory needs Kubernetes CRDs for agents and tasks and must run three kinds of
agent: coding harnesses per task, user containers as services, and declarative
agents for non-developers.

Agent Substrate (CNCF Sandbox, 1.0 in Nov 2026) gives dense actors with
snapshot/resume and gVisor isolation. It is Linux/Kubernetes-only, very new, and
the biggest operational risk. We decided to **defer Substrate** and ship the
rest of the platform first.

kagent (Apache-2.0) is the closest existing project:

| kagent line | Substrate | Agent model |
|---|---|---|
| v1.0.x (`api/v1alpha3`) | **Required** (`Harness.spec.substrate` is `+required`) | `Agent` = `AgentTemplate` + `Harness` (kagent, Claude, Codex, BYO); sessions in PostgreSQL run as Substrate actors |
| v0.10.6 (`api/v1alpha2`) | Optional (only the separate `SandboxAgent` CRD) | `Agent` of type `Declarative` or `BYO`, run as Deployments |

Deferring Substrate rules out kagent 1.x as our runtime for now.

## Options

### Option A: Build on kagent v0.10.x (alternative, not chosen)

Use kagent v0.10.6's `Agent` CRD (Declarative + BYO as Deployments), its A2A
gateway, MCP integration and UI; add Agentory's `Task`, triggers, budgets and
on-call product on top.

- ✅ Fastest path to working declarative and BYO agents, plus a UI.
- ✅ Mature A2A/MCP behaviour we would not have to write.
- ❌ v0.10.x is the pre-1.0 line; upstream development has moved to 1.x, so
  we inherit a frozen API and a forced migration (every agent definition) later.
- ❌ No pod-per-task Claude/Codex harness; we would build it outside kagent.
- ❌ Our API surface would be kagent's, not ours.

**When to choose A instead:** if time-to-demo matters more than API ownership,
for example for a throwaway demo or a short evaluation. It can be adopted
later by implementing a `KagentBackend` (see below), since the `RuntimeBackend`
interface does not care what runs the agent.

### Option B: Own thin operator with pluggable runtime backends (chosen)

Agentory owns `Agent` and `Task` CRDs (`agentory.dev/v1alpha1`). The operator
delegates execution to a `RuntimeBackend` interface:

- `DeploymentBackend`: `lifecycle: service` (container and declarative agents).
- `JobBackend`: `lifecycle: perTask` (harness agents; later backed by an
  agent-sandbox warm pool).
- `SubstrateBackend` _(backlog)_: `lifecycle: suspendable`, directly on
  Substrate or via kagent 1.x.

- ✅ Stable API we control; Substrate becomes an added backend, not a rewrite.
- ✅ Covers all three agent types from day one.
- ✅ Matches the OOP and interface-first rules in `CLAUDE.md`.
- ❌ More code up front: the operator and a small declarative runtime.

### Option C: kagent 1.x with Substrate now (rejected for now)

- ✅ Density, snapshots, forks and isolation immediately.
- ❌ Contradicts the decision to defer Substrate; highest setup risk (Substrate
  on local kind/Mac unproven); API is `v1alpha3` and still moving.

## Decision

**Option B.** We borrow kagent's ideas (A2A gateway semantics, HITL approval
protocol, telemetry contract, egress credential injection) without depending on
its runtime. Option A stays documented as the fallback.

## Consequences

- We build and maintain the operator and the declarative runtime.
- Service agents are always-on pods until `SubstrateBackend` exists, so idle
  cost is not optimised in v1.
- No sandbox isolation stronger than Kubernetes defaults in v1: suitable for
  internal, trusted agents only.
- Replay is implemented from gateway-recorded model/tool responses rather than
  memory snapshots (see HLD-0001 §8).
- Revisit this ADR when any of these hold: we need untrusted-code isolation,
  idle agents dominate cost, or Substrate on our target clusters is proven.
