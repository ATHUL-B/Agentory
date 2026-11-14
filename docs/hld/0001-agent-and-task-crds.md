# HLD-0001: Agent and Task CRDs with pluggable runtime backends

- **Status:** In progress. §3 API implemented as `agentory.dev/v1alpha1` (backlog item 3); backends pending
- **Date:** 2026-11-14
- **Related:** [ADR-0001](../adr/0001-own-operator-with-pluggable-runtimes.md),
  [ADR-0002](../adr/0002-gateways-and-observability.md)

## 1. Goals

- One API (`Agent`, `Task`) for three kinds of agents: harness-per-task,
  user containers, and declarative (skills + MCP) agents.
- All model traffic through the LLM gateway, all tool/A2A traffic through the
  agent gateway, enforced by network policy.
- Tracing to Langfuse/MLflow with no agent code changes.
- Every agent reachable over A2A (with a generated Agent Card) and REST.
- A runtime abstraction that lets us add Agent Substrate later without API
  changes.

## 2. Non-goals (v1)

- Snapshot/suspend, live-state forks, gVisor/Kata isolation.
- Triggers, team budgets, approval ladder and UI (separate HLDs; this HLD only
  leaves room for them).
- Multi-cluster.

## 3. API

API group `agentory.dev/v1alpha1` (placeholder domain). Both CRDs are
namespaced; a namespace maps to a team.

### 3.1 `Agent`

```yaml
apiVersion: agentory.dev/v1alpha1
kind: Agent
metadata:
  name: payments-oncall
  namespace: team-payments
spec:
  description: "On-call triage for payments services"
  runtime: declarative        # harness | container | declarative
  lifecycle: service          # perTask | service   (suspendable: reserved)

  # exactly one of the following, matching runtime:
  declarative:
    instructions: |
      You are the on-call agent for payments…
    skills:
      - name: k8s-triage
        source: { git: { url: https://…/skills.git, commit: 3f2a…, path: k8s-triage } }
  # harness:
  #   kind: claude-code         # claude-code | codex
  #   image: ghcr.io/…/harness-claude-code:0.1.0   # optional override
  #   workspace: { git: { url: https://…/repo.git, ref: main } }
  # container:
  #   image: ghcr.io/team/multi-agent:1.4.0
  #   port: 8080
  #   protocol: a2a             # a2a | http (http gets an A2A adapter)

  model:
    provider: anthropic         # resolved through Bifrost
    name: claude-sonnet-5-5
  tools:
    mcpServers:
      - name: datadog                        # platform-registered server
        tools: [query_logs, get_monitor]     # allow-list; empty = all
      - name: team-runbooks
        url: https://runbooks.payments.internal/mcp   # user-supplied endpoint
  a2a:
    expose: true                # register route + Agent Card on the gateway
    allowedCallers: [team-sre/*]
  observability:
    sink: default               # default | langfuse | mlflow | none
  resources:
    requests: { cpu: 250m, memory: 256Mi }
  # reserved for later HLDs: policy (approvals, autonomy), budget

status:
  phase: Ready                  # Pending | Ready | Degraded | Failed
  observedGeneration: 3
  revision: payments-oncall-7c9f2   # hash of the resolved spec
  endpoints:
    a2a: http://agentgateway.agentory-system/agents/team-payments/payments-oncall
    agentCard: …/.well-known/agent-card.json
  virtualKeyRef: { name: payments-oncall-llm }   # Secret holding the Bifrost virtual key
  conditions:
    - type: WorkloadReady
    - type: GatewayRouteReady
    - type: VirtualKeyReady
    - type: EgressPolicyApplied
```

**Validation** (CEL rules on the CRD):
- Exactly one of `declarative` / `harness` / `container`, matching `runtime`.
- `runtime: harness` requires `lifecycle: perTask`.
- `lifecycle: suspendable` is rejected until a backend supports it.
- Skill sources pin an immutable ref (git commit, OCI digest).

### 3.2 `Task`

A durable, auditable unit of work. **Not** created for every synchronous A2A
call: those go through the gateway directly to `service` agents.

```yaml
apiVersion: agentory.dev/v1alpha1
kind: Task
metadata:
  name: fix-flaky-test-8812
  namespace: team-payments
spec:
  agentRef: { name: payments-coder }
  input:
    message: "Fix the flaky test in ledger/settle_test.go"
    data: { issue: PAY-8812 }
  timeout: 30m
  # reserved: trigger (source metadata), approvals
status:
  phase: Running        # Pending | Running | AwaitingInput | Succeeded | Failed | Cancelled
  startedAt: …
  completedAt: …
  agentRevision: payments-coder-1b2e4
  traceId: 4bf92f3577b34da6a3ce929d0e0e4736
  usage: { inputTokens: 41210, outputTokens: 7001, costUsd: 0.31 }
  result:
    summary: "Replaced time.Now() with injected clock; test stable over 200 runs."
    artifacts:
      - { type: git-branch, ref: agentory/fix-flaky-test-8812 }
  conditions: […]
```

Task semantics by agent lifecycle:
- `perTask` agent: the operator creates one Job per Task (via `JobBackend`).
- `service` agent: the operator sends the input as an A2A message to the
  running agent and tracks the A2A task ID until it completes.

Completed Tasks are garbage-collected after a configurable TTL. Traces and
results remain in the trace sink and result store.

### 3.3 Decisions made while implementing (item 3)

- **API group** is `agentory.dev` (kubebuilder domain `dev`, group `agentory`).
- **Task spec is immutable** (`self == oldSelf`): a Task is an audit record of
  what was asked; re-asking means a new Task.
- **Costs are decimal strings** (`usage.costUsd: "0.31"`): Kubernetes API
  conventions avoid floats.
- **Revision** is `<agent>-<first 10 hex of sha256(spec JSON)>`; Tasks copy it
  into `status.agentRevision`.
- **Until backends exist**, Agents report `Ready=False` with reason
  `BackendNotImplemented` and Tasks report `Started=False` with the same reason,
  so nothing claims to run when it doesn't.
- **Task→Agent re-queue**: the Task controller watches Agents and re-queues
  non-terminal Tasks that reference the changed Agent (a namespace list +
  filter; switch to a field index if Task counts per namespace grow large).

## 4. Operator design

Go, controller-runtime / kubebuilder. Classes and interfaces (OOP per
`CLAUDE.md` §2):

```go
// RuntimeBackend runs agents. One implementation per lifecycle.
type RuntimeBackend interface {
    Supports(agent *v1alpha1.Agent) bool
    // EnsureAgent makes the agent's long-lived resources exist (no-op for perTask).
    EnsureAgent(ctx context.Context, agent *v1alpha1.Agent, deps ResolvedDeps) (AgentRuntimeStatus, error)
    // StartTask begins executing a task and returns a handle to poll.
    StartTask(ctx context.Context, agent *v1alpha1.Agent, task *v1alpha1.Task, deps ResolvedDeps) (TaskHandle, error)
    TaskStatus(ctx context.Context, h TaskHandle) (TaskRuntimeStatus, error)
    CancelTask(ctx context.Context, h TaskHandle) error
    Cleanup(ctx context.Context, agent *v1alpha1.Agent) error
}

// Implementations
type DeploymentBackend struct{ … } // lifecycle: service
type JobBackend struct{ … }        // lifecycle: perTask
// type SubstrateBackend struct{…} // backlog: lifecycle: suspendable

// Collaborators, injected so each is unit-testable
type LLMGatewayClient interface {   // Bifrost
    EnsureVirtualKey(ctx context.Context, owner AgentRef, spec ModelSpec) (VirtualKey, error)
    DeleteVirtualKey(ctx context.Context, owner AgentRef) error
}
type AgentGatewayClient interface { // agentgateway
    EnsureA2ARoute(ctx context.Context, agent AgentRef, target Endpoint, card AgentCard) error
    EnsureMCPBinding(ctx context.Context, agent AgentRef, servers []MCPServerSpec) error
}
type PolicyBuilder interface {      // NetworkPolicy for egress lockdown
    EgressPolicy(agent *v1alpha1.Agent) *networkingv1.NetworkPolicy
}
type AgentCardBuilder interface { Build(agent *v1alpha1.Agent) AgentCard }
```

Reconcilers:
- **AgentReconciler**: validate → resolve dependencies (skills, MCP servers) →
  ensure virtual key → ensure egress policy → pick backend → `EnsureAgent` →
  ensure A2A route + Agent Card → update status/conditions. Finalizer runs
  `Cleanup` and deletes the virtual key and routes.
- **TaskReconciler**: resolve agent (must be `Ready`) → `StartTask` → requeue
  and poll `TaskStatus` → write result, usage and `traceId` → enforce timeout via
  `CancelTask`.

Idempotency: every `Ensure*` is create-or-update keyed by owner; reconcilers
are level-triggered and safe to retry.

### 4.1 Implemented (item 4)

- `internal/runtime.Backend` has `Name`, `Supports` and `EnsureAgent(ctx, agent, revision)`;
  task methods are added with the first per-task backend (item 7), not before.
- `Registry` picks the first backend whose `Supports` is true; none → `Ready=False`,
  `BackendNotImplemented`.
- `DeploymentBackend` (`container` + `service`) owns a Deployment, Service,
  ServiceAccount and NetworkPolicy named after the Agent. Ownership via
  controller references means deletion is plain garbage collection, so no finalizer.
- It **never adopts** an existing object of the same name it doesn't control:
  the Agent goes `Failed` with `ResourceConflict` and the object is left untouched.
- `EnvBuilder` and `PolicyBuilder` are pure, separately unit-tested; gateway
  locations come from operator flags (`--llm-gateway`, `--agent-gateway`, `--otel-collector`).
- Status writes that hit a conflict requeue after 200ms instead of logging an error.

### 4.2 Implemented (item 5): per-agent virtual keys

- `internal/llmgateway.Client` (Get, FindByName, Create, Update, Delete) with a
  Bifrost implementation (`/api/governance/virtual-keys`, HTTP Basic auth) and an
  in-memory fake for tests. Bifrost generates both ID and value; neither can be
  chosen by the caller.
- `internal/virtualkey.Manager` runs before the backend. Key = only the Agent's
  model; value + ID in the Agent-owned Secret `<agent>-llm`; a spec hash
  annotation avoids redundant updates.
- **Rotation on loss**: a 404 for the stored ID creates a new key. The backend
  stamps `agentory.dev/virtual-key-id` on the pod template (`runtime.Deps`), so pods
  restart, because env from Secrets is read only at container start. The resync is
  10 minutes.
- **Finalizer** `agentory.dev/virtual-key` deletes the key (and any orphan with the
  same name) on Agent deletion.
- Gateway errors are written to status (`VirtualKeyReady=False/GatewayError`)
  before the reconcile returns the error for backoff.
- Bifrost must run with `sourceOfTruth: split`. `config.json` mode prunes
  API-created keys at every start-up (verified).

### 4.3 Implemented (item 6): A2A exposure

- `internal/a2a.RouteManager`: `EnsureCard` (before the backend, so the
  workload can mount it), `EnsureRoute` (after, once the Service exists) and
  `Remove` (when `a2a.expose` is false). The objects are unstructured
  (`HTTPRoute` gateway.networking.k8s.io/v1, `AgentgatewayBackend`
  agentgateway.dev/v1alpha1 with `spec.a2a{host,port}`), so there are no new Go
  module dependencies.
- Verified with agentgateway v1.4.1: the route's `URLRewrite` strips
  `/agents/<ns>/<agent>`; the A2A backend rewrites the card's `url` to the
  gateway address; the proxy serves 503 for a few seconds after a route is
  accepted.
- `internal/ownership.Ensure/Delete` is the shared create-or-update that never
  adopts foreign objects, now used by the backend, the key manager and the
  route manager.
- Exposure needs the CRDs. The operator checks the REST mapper at start-up and
  disables exposure otherwise. ConfigMaps, like Secrets, are read uncached.

## 5. Runtimes

### 5.1 Container (`runtime: container`)
- `DeploymentBackend` creates Deployment + Service from the user's image.
- `protocol: a2a`: route directly. `protocol: http`: inject an A2A adapter
  sidecar that maps A2A messages to `POST /invoke` _(later)_.
- Scaling: replicas, or HPA/KEDA on queue depth (not CPU; agents mostly wait).
  Session state must live outside the pod before scaling past one replica.

### 5.2 Declarative (`runtime: declarative`)
- A shared image, `agentory/declarative-runtime` (Python), configured from a
  ConfigMap generated from the spec.
- Components (classes): `A2AServer`, `AgentLoop`, `SkillLoader` (fetches pinned
  skills), `MCPToolset` (via agent gateway), `ModelClient` (via Bifrost),
  `Tracer` (OTel GenAI spans).
- Skills follow the agentskills.io / `SKILL.md` format.

### 5.3 Harness (`runtime: harness`, `lifecycle: perTask`)
- `JobBackend` creates a Job per Task from a harness image
  (`harness-claude-code`, `harness-codex`).
- A small entrypoint (`harness-runner`): clone the workspace → run the harness
  non-interactively with the task message → collect summary, diff/branch and
  usage → write the result to the Task status (via the operator API or a
  result ConfigMap) → exit.
- Model traffic: `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL` point to Bifrost; the
  pod only receives a virtual key.
- `activeDeadlineSeconds` from `Task.spec.timeout`; `ttlSecondsAfterFinished`
  for cleanup.
- Later: claim pods from an agent-sandbox `SandboxWarmPool` to cut start-up time.

## 6. Gateway wiring and security

Injected into every agent workload:

| Env var | Value |
|---|---|
| `AGENTORY_LLM_BASE_URL` | Bifrost's OpenAI-compatible API (`…/v1`) |
| `OPENAI_BASE_URL`, `ANTHROPIC_BASE_URL` | Bifrost's SDK endpoints (`…/openai`, `…/anthropic`) |
| `AGENTORY_LLM_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` | The agent's Bifrost virtual key, from Secret `<agent>-llm` key `virtual-key` (Bifrost reads it from `x-api-key` or `Authorization`) |
| `AGENTORY_MCP_URL` | agentgateway MCP endpoint scoped to this agent |
| `AGENTORY_A2A_URL` | agentgateway A2A base for calling other agents |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_RESOURCE_ATTRIBUTES` | Collector + agent identity |

NetworkPolicy per agent: egress to DNS, Bifrost, agentgateway and the OTel
collector only; ingress only from the agent gateway's namespace, on the agent's
port (none for per-task agents). Harness agents that need git access get an
explicit allow entry for the git host. CNI must enforce NetworkPolicy (kind:
use Calico or Cilium, not the default kindnet).

Identity (v1): Kubernetes ServiceAccount per agent; agentgateway validates
projected ServiceAccount tokens. Actor-chain JWTs are backlog.

## 7. A2A

- Each `a2a.expose: true` agent gets a gateway route
  `/agents/{namespace}/{name}` and a generated Agent Card (name, description,
  skills, input/output modes, auth).
- Agents call each other only via `AGENTORY_A2A_URL` (direct calls are blocked
  by the network policy). `allowedCallers` enforcement at the gateway is
  backlog item 6a.
- `traceparent` is propagated so multi-agent calls form one trace.

## 8. Observability and replay

- Spans: gateway spans (model call: model, tokens, cost; tool call: server,
  tool, latency) + runtime spans (`invoke_agent`, skill load, task lifecycle).
- Attributes follow OTel GenAI conventions plus `agentory.agent`,
  `agentory.task`, `agentory.namespace`.
- `Task.status.traceId` links Kubernetes state to the trace UI.
- **Replay without snapshots** (later HLD): because every model and tool
  response passes through the gateways, we can record them per task and replay
  a run by feeding recorded responses up to step N, then switching to live
  calls. This is the basis for "incident → test case".

## 9. Testing plan

- **Unit:** each backend, builder and client behind interfaces with fakes;
  CEL validation tests for the CRDs.
- **Integration:** envtest for reconcilers (no real cluster).
- **End-to-end:** kind cluster (Calico) with Bifrost, agentgateway, OTel
  collector, Langfuse and a mock LLM provider so CI needs no real API key.
  Scenarios: one per agent type, egress denial, cross-agent trace.

## 10. Risks and open questions

| Risk / question | Mitigation |
|---|---|
| Bifrost features we need may be enterprise-only | Keep the `LLMGatewayClient` interface; LiteLLM as fallback |
| agentgateway API churn | Isolate behind `AgentGatewayClient` |
| Harness CLIs change flags/output formats | Version-pin harness images; contract tests per harness |
| Writing Task results from the pod securely | Prefer operator-side collection (pod logs / result file) over giving pods API write access |
| No strong isolation in v1 | Internal, trusted agents only; Substrate/Kata backlog item |
| Open: result store for large artifacts (S3/MinIO?) | Decide in implementation PR |
| Open: API group domain | `agentory.dev` placeholder until a domain is chosen |
