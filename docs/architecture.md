# Architecture

Current-state architecture of Agentory. Update this file in the same PR as any
structural change (see `CLAUDE.md` §5).

> Status: **working prototype** on a local kind cluster. Everything marked
> ✅ is built and checked end to end by `make smoke` and `make demo`;
> _(planned)_ items are on the [backlog](backlog.md) with their item number.

## 1. Vision

Agentory is a Kubernetes-native agent platform where any team, including
non-engineers, can deploy agents from skills and MCP servers, run coding
harnesses (Claude Code, Codex, …) per task, or ship their own multi-agent
containers. Every agent is reachable over **A2A**, and tracing, per-agent keys,
budgets and approvals come from the platform rather than from agent code.

The first use case is **internal on-call agents**: an alert fires, a team's
agent investigates with the team's own tools and runbooks, and it proposes or
takes action under an approval policy that teams can trust.

## 2. Design principles

1. **Agents are Kubernetes resources.** `Agent`, `Task` and `MCPServer` are
   CRDs. A UI and CLI will write CRDs, so GitOps works for engineers and
   non-engineers alike.
2. **The platform owns the network path, like a service mesh.** Agents reach
   models only through the **LLM gateway**, and tools and other agents only
   through the **agent gateway**. Egress is deny-by-default, so this holds even
   for black-box containers.
3. **Secrets never enter agent workloads.** Bifrost holds the provider keys;
   each agent gets only its own scoped virtual key.
4. **Observability is a platform property.** Gateways and runtimes emit
   OpenTelemetry (GenAI semantic conventions) with W3C `traceparent` on every
   hop. The sink (Jaeger, Langfuse, MLflow) is swapped without changing agent code.
5. **The runtime is pluggable.** The operator delegates execution to a
   `RuntimeBackend` interface: Deployments and Jobs today, Agent Substrate
   later, with the same API (ADR-0001).
6. **CRDs for durable intent, not per-request traffic.** Synchronous calls go
   through the gateway; `Task` objects are for durable, auditable work.
7. **Open standards at every boundary**: A2A for agents, MCP for tools,
   OpenAI/Anthropic-compatible APIs for models, OpenTelemetry for traces,
   Kubernetes Gateway API for routes, `SKILL.md` for skills.

## 3. System overview

```
 Triggers (planned, #10)       Clients
 PagerDuty · Slack · cron      A2A callers · other agents · kubectl
        │                                │
        │                ┌───────────────▼──────────────────┐
        └──────────────► │ Agent gateway (agentgateway)      │ ◄─ agent ↔ agent (A2A)
                         │ /agents/<ns>/<agent>   A2A + card │
                         │ /mcp/<ns>/<agent>      MCP, tool  │
                         │                        allow-list │
                         └──────┬─────────────────────┬──────┘
                                │                     │ MCP
 ┌──────────────────────────────▼───────┐             ▼
 │ Agentory operator                    │      MCP servers (registered as
 │  CRDs: Agent · Task · MCPServer      │      MCPServer, platform or team)
 │  RuntimeBackends: Deployment · Job   │
 │  creates per Agent: workload, SA,    │
 │  NetworkPolicy, virtual key, A2A     │
 │  route + card, MCP route + policy    │
 └──────────────┬───────────────────────┘
                │ reconciles
                ▼
 ┌────────────────────────────────────────────────────────┐
 │ Agent workloads (namespace = team)                      │
 │  declarative → Deployment + Service   (service)         │
 │  container   → Deployment + Service   (service)         │
 │  harness     → one Job per Task        (perTask)        │
 │  egress: DNS + LLM gateway + agent gateway + collector  │
 └──────────────┬──────────────────────────────┬──────────┘
                │ model calls, own virtual key │ spans (OTLP)
                ▼                              ▼
 ┌─────────────────────────────┐   ┌─────────────────────────────────┐
 │ LLM gateway (Bifrost)        │   │ OTel collector → Jaeger (local) │
 │ provider keys, virtual key   │   │ Langfuse / MLflow (planned, 2b) │
 │ per agent, model allow-list, │   └─────────────────────────────────┘
 │ token + cost per call        │──spans──▲
 └─────────────────────────────┘
```

## 4. Components

| Component | Responsibility | Status |
|---|---|---|
| **Operator** ([`operator/`](../operator/README.md)) | Reconciles `Agent`, `Task` and `MCPServer`. Per Agent: workload, ServiceAccount, NetworkPolicy, Bifrost virtual key (rotated, cleaned up by finalizer), A2A route and Agent Card, MCP route and tool policy. Per Task: a Job, then result, usage, cost and trace id in status. Status conditions explain every wait and failure. | ✅ Go, kubebuilder |
| **RuntimeBackend** | Interface for running agents: `DeploymentBackend` (service agents), `JobBackend` (harness Tasks). Later `SubstrateBackend` (#18) | ✅ |
| **Declarative runtime** ([`runtime/declarative`](../runtime/declarative/README.md)) | Runs no-code agents: instructions + `SKILL.md` skills + MCP tools, A2A `message/send`, per-conversation memory, spans for the agent and each tool call | ✅ Python |
| **Claude Code harness** ([`harness/claude-code`](../harness/claude-code/README.md)) | Runs Claude Code headless for one Task with the agent's key and MCP tools; reports summary, changed files, tokens and cost. Codex harness planned | ✅ |
| **LLM gateway** | All model traffic: provider keys, a virtual key per agent, model allow-list, retries/fallbacks, token and cost per call, spans | Reused: **Bifrost** |
| **Agent gateway** | A2A routing to agents, MCP proxy with per-agent tool authorization (CEL) | Reused: **agentgateway** |
| **Tool registry** | `MCPServer` CRD: register a server once; Agents use it by name (own namespace, then the platform namespace) | ✅ |
| **OTel collector + trace sink** | Receives spans from runtimes and Bifrost; Jaeger locally | ✅ collector + Jaeger; Langfuse/MLflow planned (#2b) |
| **Examples and demo** | [`examples/incident-commander`](../examples/incident-commander/README.md) (BYO multi-agent), [`tools/echo-agent`](../tools/echo-agent/README.md), [`tools/oncall-mcp`](../tools/oncall-mcp/README.md), [`demo/`](../demo/README.md) (`make demo`) | ✅ |
| **Mock LLM** ([`tools/mock-llm`](../tools/mock-llm/README.md)) | Deterministic provider behind Bifrost, so everything runs without an API key | ✅ |
| Triggers, approvals, budgets, UI | `Trigger` (#10), approval ladder, `TeamBudget` (#12), onboarding UI | _(planned)_ |

## 5. Agent types

Two orthogonal fields replace a fixed list of agent types (HLD-0001):

| `runtime` → / `lifecycle` ↓ | `harness` (Claude Code) | `container` (your image) | `declarative` (skills + MCP) |
|---|---|---|---|
| `perTask` (start, work, exit) | ✅ **Type 1**: coding / task agents | _(planned)_ batch jobs | _(planned)_ one-shot jobs |
| `service` (long-lived) | n/a | ✅ **Type 2**: multi-agent systems | ✅ **Type 3**: no-code agents |
| `suspendable` | Later (needs Substrate, #18) | Later | Later |

What a workload gets from the operator, whatever its type:

| Injected | Purpose |
|---|---|
| `AGENTORY_LLM_BASE_URL`, `AGENTORY_LLM_API_KEY`, `AGENTORY_MODEL` (plus the OpenAI/Anthropic SDK variables) | Model calls through Bifrost with the agent's own key |
| `AGENTORY_A2A_URL` | Calling other agents through the agent gateway |
| `AGENTORY_MCP_URL`, `AGENTORY_MCP_ENABLED` | The agent's own, allow-listed MCP endpoint |
| `AGENTORY_AGENT_CARD_PATH` | The generated A2A Agent Card |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Tracing with no SDK configuration |

## 6. Flows

### 6.1 Onboard an agent

1. A team applies an `Agent` (YAML, later the UI).
2. The operator validates it (`Accepted`), resolves its skills and MCP servers,
   creates its virtual key in Bifrost (`VirtualKeyReady`), its workload and
   NetworkPolicy (`WorkloadReady`, `EgressPolicyApplied`), its A2A route and
   Agent Card (`GatewayRouteReady`) and its MCP route and policy (`ToolsReady`).
3. `Ready` is true only when all of these are. Otherwise the condition says
   why, e.g. `SkillNotFound` or `MCPServerNotFound`.

### 6.2 Ask an agent (A2A)

```
caller ──A2A message/send──▶ agent gateway /agents/<ns>/<agent>/ ──▶ agent pod
agent  ──chat + tools──────▶ Bifrost (agent's key) ──▶ provider     "call tool X"
agent  ──tools/call────────▶ agent gateway /mcp/<ns>/<agent> ──▶ MCP server
                             (allow-list checked here)
agent  ──tool result───────▶ Bifrost ──▶ provider                   answer
agent  ──answer────────────▶ caller
```

`traceparent` travels on every hop, so it is one trace. Model calls go
through Bifrost; tool and agent calls go through agentgateway. Bifrost has its
own MCP features, but we don't use them (ADR-0002).

### 6.3 Run a Task (harness)

1. A `Task` names a harness Agent and gives a message, plus optional data.
2. The operator creates a Job: a fresh, locked-down pod with the agent's key,
   its MCP tools and a timeout.
3. Claude Code works in the pod, through the same gateways.
4. The harness writes the result as the pod's termination message. The
   operator copies the summary, changed files, tokens, cost and trace id into
   `Task.status` (`Succeeded`, `Failed` or `Timeout`).
5. Deleting the Task deletes the Job.

### 6.4 Agent calls agent

A container agent calls `AGENTORY_A2A_URL/agents/<ns>/<agent>/` and forwards
`traceparent` (see `examples/incident-commander`). Declarative agents can't
call other agents yet ("agents as tools", planned).

## 7. Security model

| Layer | Control | Status |
|---|---|---|
| Network | Default-deny egress per agent: only DNS, Bifrost, agentgateway and the OTel collector. Ingress only from the gateway. No direct internet, MCP server or agent-to-agent access. | ✅ (Calico locally) |
| Credentials | Provider keys live only in Bifrost. Each agent gets its own virtual key (rotated, revoked on delete). No Kubernetes API token is mounted. | ✅ |
| Tools | Per-agent MCP allow-list enforced at the gateway: `tools/list` is filtered and other calls are rejected, even for containers that ignore it | ✅ |
| Models | The virtual key only allows the Agent's model | ✅ |
| Pods | Non-root, all capabilities dropped, `RuntimeDefault` seccomp, no privilege escalation; one fresh pod per Task with a timeout | ✅ |
| Ownership | The operator never adopts objects it didn't create (`ResourceConflict` instead) | ✅ |
| Caller authorization | `a2a.allowedCallers` enforced at the gateway with per-agent identity | _(planned, #6a)_ |
| MCP credentials | Per-server secrets injected by the gateway, never in the pod | _(planned, #6c)_ |
| Budgets | Spend and token limits per team on the virtual keys | _(planned, #12)_ |
| Approvals | Human approval for risky tools, relaxed as trust grows | _(planned)_ |
| Strong isolation | Kata / gVisor sandboxes and warm pools | _(planned, #22, #18)_ |

The current isolation is hardened containers on a shared kernel: fine for
internal, trusted agents, not for untrusted third-party code until #22/#18.

## 8. Observability

| Signal | Source | Status |
|---|---|---|
| Agent span (`invoke_agent`, tokens, conversation id) | Declarative runtime | ✅ |
| Tool span (`execute_tool <name>`) | Declarative runtime | ✅ |
| Model span (provider, model, virtual key = which agent, tokens, cost, finish reason) | Bifrost | ✅ |
| Task usage and cost | `Task.status.usage` | ✅ |
| BYO agent spans | The agent itself, via the injected `OTEL_*` variables | ✅ (example provided) |
| Gateway spans, so A2A/MCP hops nest under their caller | agentgateway | _(planned, #6d)_ |
| Langfuse / MLflow sink, evaluations | Collector exporter | _(planned, #2b)_ |
| Metrics and dashboards | - | _(planned)_ |

## 9. Environments

| Environment | Where | Trace sink | Notes |
|---|---|---|---|
| Local | kind on a laptop, [`deploy/local/`](../deploy/local/README.md) | Jaeger | `make up`, `make operator-up`, `make smoke`, `make demo` |
| Shared / prod | Any Kubernetes with a NetworkPolicy-enforcing CNI | Langfuse (or MLflow) | Not built yet |

## 10. Key decisions

Recorded as ADRs in [`adr/`](adr/):

- [ADR-0001](adr/0001-own-operator-with-pluggable-runtimes.md): own thin
  operator with pluggable runtime backends (Option B); kagent v0.10.x
  (Option A) kept as a documented alternative; Agent Substrate deferred.
- [ADR-0002](adr/0002-gateways-and-observability.md): Bifrost for model
  traffic, agentgateway for A2A/MCP, OpenTelemetry as the observability
  contract.

## 11. Prior art and what we borrow

| Project | What we take |
|---|---|
| kagent | Agent/harness split, A2A gateway behaviour, HITL approval protocol, telemetry contract, egress credential injection |
| Agent Substrate | Future `SubstrateBackend`: actors, snapshot/resume, gVisor |
| AWS Open Agentic Platform | Reference wiring for Bifrost, agentgateway, Langfuse/OTel, online evals |
| Paperclip | Harness adapters, budgets and governance ideas |
| Uber agent platform | Omni-MCP discovery tools, response projection, actor-chain identity, tools disabled until reviewed |

## 12. Out of scope (for now)

- Snapshot/suspend density, live-state forks, gVisor/Kata isolation (backlog,
  via `SubstrateBackend`).
- Multi-cluster / hub-and-spoke topology.
- Running untrusted third-party code: not safe until sandbox isolation lands.
- REST (non-A2A) invocation of agents.
