# Agentory operator

Kubernetes operator for the `agentory.dev/v1alpha1` API: **`Agent`** and
**`Task`** (design: [HLD-0001](../docs/hld/0001-agent-and-task-crds.md)).
Built with kubebuilder v4.16 / controller-runtime.

## Status

| Piece | State |
|---|---|
| `Agent` / `Task` CRDs with schema + CEL validation | ✅ (backlog item 3) |
| Agent controller: spec revision, `Accepted`, `Ready=False/BackendNotImplemented` | ✅ |
| Task controller: resolves its Agent, records `agentRevision`, re-queued when the Agent changes | ✅ |
| **DeploymentBackend**: `container` + `service` agents → Deployment, Service, ServiceAccount, NetworkPolicy (owned by the Agent; deleting it cleans up) | ✅ (item 4) |
| Gateway environment injection and egress/ingress lockdown | ✅ (item 4) |
| **Per-agent virtual keys** in Bifrost: one key per Agent, only its model, in Secret `<agent>-llm`; re-created if lost; deleted with the Agent | ✅ (item 5) |
| **A2A exposure**: generated Agent Card + agentgateway A2A backend + HTTPRoute at `/agents/<ns>/<agent>` | ✅ (item 6) |
| **JobBackend + Tasks**: `harness` agents run each Task as a Job (Claude Code); result, usage, cost, changed files and traceId in `Task.status` | ✅ (item 7) |
| **Declarative agents**: instructions + skills (ConfigMap `SKILL.md`), run by the declarative runtime; one trace from caller to model | ✅ (item 8) |
| **MCP tools**: `MCPServer` registry, per-Agent tool route on the agent gateway, allow-list enforced by the gateway; used by declarative agents and the Claude Code harness | ✅ (item 6b) |
| Caller allow-lists (6a), MCP credentials (6c), workspace clone (7a), git/OCI skills (8a) | Backlog |

## API at a glance

```yaml
apiVersion: agentory.dev/v1alpha1
kind: Agent
spec:
  runtime: declarative          # harness | container | declarative
  lifecycle: service            # perTask | service   (suspendable: reserved)
  declarative: { instructions: "...", skills: [...] }
  model: { provider: mock, name: mock-model }
---
kind: Task
spec:
  agentRef: { name: payments-oncall }
  input: { message: "Investigate PD-123" }
  timeout: 30m                  # default
```

Full examples: [`config/samples/`](config/samples/).

### Validation (enforced by the API server)

| Rule | Message |
|---|---|
| `runtime` must match exactly one of `declarative` / `harness` / `container` | `runtime 'X' requires spec.X and no …` |
| `harness` agents run per task | `runtime 'harness' requires lifecycle 'perTask'` |
| `suspendable` is reserved for the Agent Substrate backend | `lifecycle 'suspendable' is not supported yet …` |
| Skills are immutable: git **commit SHA** or OCI **digest**, exactly one | `exactly one of git or oci must be set` |
| MCP `url` must be http(s); container `port` 1–65535 | schema errors |
| `Task.spec` is immutable after creation | `Task spec is immutable; create a new Task instead` |

### Status

- `Agent.status`: `phase` (`Ready` once the workload is available, the key is ready and the route accepted; `Failed` on a resource conflict), `observedGeneration`, `revision` (`<name>-<10 hex>`, from a hash of the spec), `endpoints` (`a2a`, `agentCard`, `internal`), `virtualKeyRef`, `conditions` (`Accepted`, `VirtualKeyReady`, `EgressPolicyApplied`, `WorkloadReady`, `GatewayRouteReady`, `Ready`).
- `Task.status`: `phase` (`Pending` … `Succeeded/Failed/Cancelled`), `agentRevision`, `startedAt`/`completedAt`, `traceId`, `usage`, `result` (`summary`, `artifacts`), `conditions` (`AgentResolved`, `Started`, `Complete`).

## What a container Agent gets

For `runtime: container`, `lifecycle: service` the operator creates, all named
after the Agent and controlled by it:

- **Deployment**: one replica, the image, `container.env` first and then the
  platform environment below; TCP readiness on the port; no service-account
  token, no service links, `allowPrivilegeEscalation: false`, all capabilities
  dropped (`NET_BIND_SERVICE` added back for ports < 1024), `RuntimeDefault` seccomp.
  The pod template carries `agentory.dev/revision`, so a spec change rolls it.
- **Service** on the agent's port, a **ServiceAccount**, and a **NetworkPolicy**:
  egress only to DNS, the LLM gateway, the agent gateway and the OTel collector;
  ingress only from the agent gateway's namespace.

If an object with that name already exists and isn't owned by the Agent, the
operator leaves it untouched and reports `Ready=False`, reason `ResourceConflict`.

| Variable | Value (local defaults) |
|---|---|
| `AGENTORY_LLM_BASE_URL` | `http://bifrost.bifrost.svc.cluster.local:8080/v1` (OpenAI-compatible) |
| `OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` | Bifrost's `/openai` and `/anthropic` SDK endpoints |
| `AGENTORY_LLM_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` | The Agent's virtual key, from Secret `<agent>-llm`, key `virtual-key` (see below) |
| `AGENTORY_MODEL` | `provider/name`, e.g. `mock/mock-model` |
| `AGENTORY_A2A_URL`, `AGENTORY_MCP_URL` | agent gateway; MCP path `/mcp/<namespace>/<agent>` (routes in item 6) |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | collector; `agentory.agent/namespace/revision` attributes. `observability.sink: none` sets `OTEL_SDK_DISABLED=true` instead |
| `AGENTORY_AGENT`, `AGENTORY_NAMESPACE`, `AGENTORY_REVISION` | identity |

Names starting `AGENTORY_` or `OTEL_`, and the four SDK variables, are reserved:
`container.env` can't set them (CEL).

Gateway locations are operator flags, as `namespace/name:port`:
`--llm-gateway`, `--agent-gateway`, `--otel-collector`, plus `--cluster-domain`.

## Per-agent virtual keys

When the operator has LLM gateway admin credentials (env
`LLM_GATEWAY_ADMIN_USERNAME` / `LLM_GATEWAY_ADMIN_PASSWORD`, from the optional
Secret `llm-gateway-admin` in the operator's namespace), every Agent gets its
own Bifrost virtual key:

- **Least privilege**: the key allows only `spec.model` (provider + name). Other
  models get `403 model_blocked` from Bifrost. Changing the model updates the key
  in place; its value doesn't change.
- **Attribution**: the key is named `agentory:<namespace>/<agent>` and carries
  metadata `agentory.agent`, `agentory.namespace`, `agentory.uid`, which Bifrost
  copies onto every log entry.
- **Storage**: the value is written to the Agent-owned Secret `<agent>-llm`
  (key `virtual-key`, annotation `agentory.dev/virtual-key-id`). Workloads read it
  as `AGENTORY_LLM_API_KEY`, `OPENAI_API_KEY` and `ANTHROPIC_API_KEY`.
- **Self-healing**: if Bifrost no longer has the key (for example after its
  database was reset), the next reconcile creates a new one. That happens on any
  change, or within the 10-minute resync. The pod template is stamped with the key
  ID, so the pods restart with the new value. A key left without a Secret (orphan)
  is replaced.
- **Cleanup**: the finalizer `agentory.dev/virtual-key` deletes the key from
  Bifrost before the Agent goes away. If key management is later turned off,
  the finalizer is released without deleting the key (logged).
- **Status**: condition `VirtualKeyReady`, `status.virtualKeyRef`; `Ready` needs
  the key too. A Bifrost outage shows as `VirtualKeyReady=False`, reason
  `GatewayError` with the message, and is retried with backoff.
- Secrets are read straight from the API server: the operator doesn't cache
  (or watch) Secrets cluster-wide.

Without credentials, key management is off and Agents run without
`VirtualKeyReady` (their pods' key variables stay empty).

If Bifrost is gone for good and an Agent is stuck deleting:
`kubectl patch agent <name> --type=json -p '[{"op":"remove","path":"/metadata/finalizers"}]'`.

## A2A exposure

Service Agents with `a2a.expose` (default `true`) are published on the agent
gateway, so other agents and clients reach them at
`http://agentory-gateway.agentgateway-system.svc.cluster.local:8080/agents/<namespace>/<agent>`
(also in `status.endpoints.a2a`). The operator owns three objects per Agent:

- **ConfigMap `<agent>-agent-card`**: the generated A2A Agent Card (protocol
  0.3.0: name, description, gateway URL, version = the Agent's revision, skills
  from `declarative.skills`). It is mounted at
  `/etc/agentory/agent-card/agent-card.json` (`AGENTORY_AGENT_CARD_PATH`) as a
  directory, so updates reach running pods; any A2A server can just serve it.
- **`AgentgatewayBackend`** of type `a2a` pointing at the Agent's Service.
- **`HTTPRoute`** on the `agentory-gateway` Gateway, matching the path prefix
  and stripping it, so the agent serves at `/`.

`GatewayRouteReady` comes from the HTTPRoute's own status (`Accepted` and
`ResolvedRefs` from the gateway); `Ready` needs it. The gateway's proxy picks
up a newly accepted route a few seconds later, so clients should retry a 503
briefly. Turning `a2a.expose` off deletes the route, backend and card.

The workload's network policy only admits traffic from the gateway's namespace,
so going through the gateway is the only way to reach an agent. A direct
pod-to-pod call is blocked.

Not yet: `a2a.allowedCallers` is stored but **not enforced** (backlog 6a: it
needs per-agent caller identity at the gateway), and MCP routes under
`/mcp/<ns>/<agent>` don't exist yet (6b).

Exposure is turned off automatically, and logged, when the Gateway API or
agentgateway CRDs are not installed.

## MCP tools

```yaml
kind: MCPServer               # the tool registry: publish a server once
metadata: { name: oncall, namespace: agentory-mcp }
spec: { url: "http://oncall-mcp.mcp-demo.svc.cluster.local:8080/mcp" }
---
kind: Agent
spec:
  tools:
    mcpServers:
      - name: oncall                              # found in the Agent's namespace, then agentory-mcp
        tools: [get_recent_deploys, get_error_rate]   # allow-list; omit for all tools
      - name: team-runbooks
        url: https://runbooks.payments.internal/mcp   # or point at a URL directly
```

For an Agent with `tools.mcpServers`, the operator owns three objects named
`<agent>-mcp`:
- an **agentgateway MCP backend** with one target per server (HTTPS targets
  get TLS with SNI; `prefixMode: Always`, so tools are named `<server>_<tool>`);
- an **HTTPRoute** at `/mcp/<namespace>/<agent>`, which is the agent's
  `AGENTORY_MCP_URL`;
- an **AgentgatewayPolicy** whose CEL rules (`mcp.tool.target`, `mcp.tool.name`)
  allow exactly the listed tools.

The gateway filters `tools/list` and rejects calls to anything else, so the
allow-list holds even for a container agent that ignores it. `ToolsReady` comes
from the route's status, and `Ready` needs it. An unknown server gives
`MCPServerNotFound` and a bad URL `InvalidMCPURL`. Removing the tools removes
the route.

Workloads get `AGENTORY_MCP_ENABLED=true`. The declarative runtime then loads
the tools from the gateway, and the Claude Code harness passes
`--mcp-config` (tools appear as `mcp__agentory__<server>_<tool>`).

Not yet: credentials for MCP servers (6c), and gateway spans for MCP calls (6d).

## Declarative agents

A `runtime: declarative`, `lifecycle: service` Agent is just instructions and
skills; the operator runs it with the
[declarative runtime](../runtime/declarative/README.md)
(`--declarative-runtime-image`). The operator:

- writes `ConfigMap <agent>-runtime` (`config.json`: instructions + skill paths);
- mounts each skill's ConfigMap read-only at `/etc/agentory/skills/<name>`;
- stamps a hash of that content on the pod template (`agentory.dev/content-hash`),
  so changed instructions or skills restart the pod.

Skill sources:

| Source | Status |
|---|---|
| `configMap: {name}`: a ConfigMap with a `SKILL.md` key (plus any files) in the Agent's namespace | ✅ |
| `git` / `oci` | Accepted by the API; the Agent reports `Ready=False`, `SkillSourceNotSupported` (backlog 8a) |

A missing ConfigMap, or one without `SKILL.md`, gives `Ready=False`,
`SkillNotFound`, and nothing is deployed. The Agent re-checks every 15 s, so
creating the skill afterwards is enough. Prefer **immutable** ConfigMaps with
a new name per version.

## Harness agents and Tasks

A `runtime: harness` agent (`lifecycle: perTask`) has no long-lived workload.
The operator gives it a ServiceAccount, a virtual key and a NetworkPolicy with
**no ingress** (egress only to DNS, the gateways and the collector), and
reports `Ready` with reason `ReadyForTasks`.

Each **Task** for it becomes one **Job owned by the Task**, so deleting the
Task removes it. The Job is configured as follows:
- `backoffLimit: 0` (one attempt; a coding run is not safely repeatable);
- `activeDeadlineSeconds` from `spec.timeout`, with a 1 h TTL after finishing;
- runs as UID 1000 with `fsGroup` and no privilege escalation;
- `/workspace` and `/tmp` are emptyDirs;
- the image is the harness default (`--harness-image-claude-code`), or `spec.harness.image`.

The Task controller:

1. Waits for the Agent to be `Ready` (`Started=False`, `AgentNotReady`) and
   re-checks when the Agent changes.
2. Records `status.agentRevision` when it starts; that never changes afterwards.
3. Maps the Job to the phase: `Pending`, `Running`, then `Succeeded` or `Failed`
   (reason `Timeout` when the deadline passed). Image-pull problems show on a
   Pending Task as `Complete=False`, `ImageProblem`.
4. Copies the harness result from the pod's termination message into status:
   `result.summary`, `result.artifacts` (changed files), `usage` (tokens and
   cost as a decimal string) and `traceId`. The trace ID finds the LLM gateway's
   spans for the run in the trace sink.

Tasks for service agents report `Started=False`, `DispatchNotImplemented`;
they take work over A2A. Setting `harness.workspace` is accepted, but the
Agent's Ready message says it's not supported yet (backlog 7a).

## Develop

```bash
make test         # unit + envtest (real kube-apiserver/etcd), coverage in cover.out
make lint         # golangci-lint (pinned by the scaffold)
make manifests generate   # after editing api/ or RBAC markers
```

From the repo root:

```bash
make operator-test   # test + lint
make operator-up     # build image, load into the kind cluster, deploy CRDs + controller
make smoke           # includes operator checks when it is deployed
```

Notes:
- `go.mod` requires Go 1.26; an older local Go downloads the 1.26 toolchain
  automatically (`GOTOOLCHAIN=auto`).
- `make test` only runs packages that have tests: coverage for test-less
  packages needs the `covdata` tool, which auto-downloaded toolchains don't ship.
- Image builds need Docker BuildKit (`docker buildx`); the legacy builder
  ignores the `.dockerignore` re-include patterns and fails to find `cmd/main.go`.
- Base images are pinned (`golang:1.26.9`, distroless by digest) in the `Dockerfile`.
