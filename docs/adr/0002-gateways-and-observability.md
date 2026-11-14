# ADR-0002: Gateways and observability

- **Status:** Accepted
- **Date:** 2026-11-14

## Context

Every agent needs model access, tool access (MCP), agent-to-agent calls (A2A)
and tracing. Implementing these per agent would duplicate auth, rate limiting,
cost tracking and logging, and would not work for black-box user containers.
Uber's internal platform and AWS OAP both route all traffic through gateways.

## Decision

1. **LLM gateway: Bifrost** (Apache-2.0 core).
   - Every agent receives a Bifrost **virtual key** (budget, rate limit,
     allowed models). Provider keys live only in Bifrost.
   - Agents use OpenAI/Anthropic-compatible base URLs pointing at Bifrost.
   - Some governance features (RBAC, SSO, audit, guardrails) may be
     enterprise-only; Agentory implements those in its own control plane.
   - Alternative: LiteLLM, behind the same base-URL contract.
2. **Agent gateway: agentgateway**, for A2A routing between agents and MCP
   proxying with per-agent tool filtering and JWT policy.
3. **Enforced, not optional.** The operator creates a deny-all egress
   NetworkPolicy per agent, allowing only DNS, the two gateways and the OTel
   collector.
4. **Central gateways, not per-pod sidecars.** Per-task pods are short-lived;
   sidecar start-up and memory would dominate.
5. **OpenTelemetry is the contract.** Gateways and Agentory runtimes emit OTel
   spans using GenAI semantic conventions; W3C `traceparent` is propagated
   across A2A. An OTel collector exports to the configured sink: **Langfuse**
   by default, **MLflow** or Jaeger as options. Agent code never imports a
   vendor SDK.

### Amendment (2026-11-14): local environment

- **Local trace sink is Jaeger, not Langfuse.** The Langfuse Helm chart v2.x
  requires cert-manager, the ClickHouse operator, ClickHouse + Keeper, Postgres,
  Valkey and S3-compatible storage, which is too heavy for a 16 GB laptop.
  Langfuse remains the default for shared environments; a local Langfuse
  profile is backlog item 2b. Agent code is unaffected either way (point 5).
- **CNI is Calico** on kind, because NetworkPolicy egress enforcement
  (point 3) is what the lockdown relies on.
- The collector (0.163.x) names the gRPC OTLP exporter `otlp_grpc`; `otlp` is a
  deprecated alias.

## Consequences

- Black-box containers get model/tool tracing and cost for free.
- The gateways are critical-path services and need HA, timeouts and
  backpressure.
- Swapping Bifrost, agentgateway or the trace sink changes configuration, not
  agent code.
