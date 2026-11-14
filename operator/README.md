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
| Runtime backends (Deployment, Job), gateway wiring, virtual keys, A2A routes | Backlog items 4–8 |

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

- `Agent.status`: `phase`, `observedGeneration`, `revision` (`<name>-<10 hex>`, from a hash of the spec), `conditions` (`Accepted`, `Ready`).
- `Task.status`: `phase` (`Pending` … `Succeeded/Failed/Cancelled`), `agentRevision`, `conditions` (`AgentResolved`, `Started`), plus `traceId`, `usage`, `result` (filled by later backends).

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
