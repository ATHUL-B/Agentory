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
| Virtual keys (item 5), A2A routes (6), harness Jobs (7), declarative runtime (8) | Backlog |

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

- `Agent.status`: `phase` (`Ready` once the workload is available; `Failed` on a resource conflict), `observedGeneration`, `revision` (`<name>-<10 hex>`, from a hash of the spec), `endpoints.internal`, `conditions` (`Accepted`, `EgressPolicyApplied`, `WorkloadReady`, `Ready`).
- `Task.status`: `phase` (`Pending` … `Succeeded/Failed/Cancelled`), `agentRevision`, `conditions` (`AgentResolved`, `Started`), plus `traceId`, `usage`, `result` (filled by later backends).

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
| `AGENTORY_LLM_API_KEY`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY` | From Secret `<agent>-llm`, key `virtual-key` (optional; created by item 5) |
| `AGENTORY_MODEL` | `provider/name`, e.g. `mock/mock-model` |
| `AGENTORY_A2A_URL`, `AGENTORY_MCP_URL` | agent gateway; MCP path `/mcp/<namespace>/<agent>` (routes in item 6) |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | collector; `agentory.agent/namespace/revision` attributes. `observability.sink: none` sets `OTEL_SDK_DISABLED=true` instead |
| `AGENTORY_AGENT`, `AGENTORY_NAMESPACE`, `AGENTORY_REVISION` | identity |

Names starting `AGENTORY_` or `OTEL_`, and the four SDK variables, are reserved:
`container.env` can't set them (CEL).

Gateway locations are operator flags, as `namespace/name:port`:
`--llm-gateway`, `--agent-gateway`, `--otel-collector`, plus `--cluster-domain`.

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
