# Local dev cluster

A single-node kind cluster with the Agentory data plane: the LLM gateway
(Bifrost), the agent gateway (agentgateway), and tracing (OTel collector →
Jaeger), plus Calico so agent egress lockdown is actually enforced
(ADR-0002).

```
make up      # create or update the cluster (idempotent)
make smoke   # end-to-end checks against the running cluster
make check   # static checks, no Docker needed
make down    # delete the cluster (PURGE=1 also deletes generated keys)
```

## What gets installed

| Namespace | Component | Version | Purpose |
|---|---|---|---|
| `tigera-operator`, `calico-system` | Calico | chart `v3.33.0` | CNI that enforces NetworkPolicy (incl. egress) |
| cluster-wide | Gateway API CRDs | `v1.5.0` (standard) | Gateway/HTTPRoute APIs |
| `agentgateway-system` | agentgateway + `agentory-gateway` Gateway | `v1.4.1` | A2A and MCP gateway (routes added by later backlog items) |
| `observability` | OTel collector (contrib) | chart `0.178.0` | Receives OTLP from gateways and agents |
| `observability` | Jaeger all-in-one | `2.22.0` | Local trace viewer (in-memory) |
| `bifrost` | Bifrost | chart `2.1.45`, image `v2.3.1` | LLM gateway, virtual keys, cost, OTel spans |

All pins live in [`versions.env`](versions.env).

## Prerequisites

- A Docker engine with **at least 6 GB** of memory (8 GB+ once agents are
  added). Tested with **Colima** (no admin rights needed):
  `colima start --vm-type vz --cpu 6 --memory 8 --disk 60`. Docker Desktop or
  OrbStack also work.
- `kind` v0.33.0, `kubectl`, `helm` v3.17+ or v4, on `PATH`.
- Optional for `make check`: `shellcheck`, `otelcol-contrib` 0.163.x.

With Homebrew: `brew install kind kubectl helm shellcheck`.

## Model provider keys

Bifrost holds the provider keys; agents only ever get a Bifrost **virtual key**.
Export a key before `make up` to enable real model calls:

```bash
export ANTHROPIC_API_KEY=...   # and/or OPENAI_API_KEY=...
make up
```

The key is stored in the `bifrost/bifrost-provider-keys` Secret and referenced
from Bifrost's config as `env.ANTHROPIC_API_KEY`. It is never written to the
repo. Without a key, everything still comes up and `make smoke` skips the
model-call check.

`make up` also generates, once, into `deploy/local/.secrets/` (git-ignored):

- `bifrost-encryption-key`: Bifrost's encryption key
- `dev-virtual-key`: a virtual key (`vk-agentory-dev`) for manual testing:

```bash
kubectl --context kind-agentory -n bifrost port-forward svc/bifrost 8080 &
curl -s localhost:8080/v1/chat/completions \
  -H "x-bf-vk: $(cat deploy/local/.secrets/dev-virtual-key)" \
  -H 'Content-Type: application/json' \
  -d '{"model":"anthropic/claude-haiku-5-5","messages":[{"role":"user","content":"hi"}]}'
```

## What `make smoke` checks

1. Gateways, collector and Jaeger are ready, and the Gateway is programmed.
2. A probe pod under an agent-style egress policy:
   - **can** reach Bifrost,
   - **cannot** reach `api.anthropic.com` directly,
   - **cannot** reach services that aren't on the allow-list (Jaeger).
3. A span sent to the collector shows up in Jaeger.
4. With a provider key configured:
   - Bifrost rejects requests with **no** virtual key and with an **unknown**
     one (401);
   - the dev virtual key is accepted and routed to the provider key;
   - the provider answers (200). With a dummy key this check fails with
     "provider rejected the API key", which still proves everything up to the
     provider works;
   - Bifrost's own spans appear in Jaeger.

   Override the model with `SMOKE_MODEL=provider/model`.

Without a provider key, step 4 is skipped: Bifrost resolves the provider
before it checks credentials, so with no provider every request is a 400 and
auth can't be observed.

## UIs

```bash
kubectl --context kind-agentory -n observability port-forward svc/jaeger 16686   # http://localhost:16686
kubectl --context kind-agentory -n bifrost port-forward svc/bifrost 8080         # http://localhost:8080
```

## Why Jaeger and not Langfuse locally

Langfuse's Helm chart (v2.x) needs cert-manager, the ClickHouse operator, a
ClickHouse and Keeper cluster, Postgres, Valkey and S3-compatible storage. That
is several GB of memory before any agent runs. Locally we use Jaeger. The
collector is the only place that changes to export to Langfuse or MLflow, so
agent code is unaffected. A Langfuse profile is backlog item 2b.

## Gotchas found on the first real run

| Symptom | Cause | Fix in this repo |
|---|---|---|
| `no matches for kind "Installation"` | Calico ≥ 3.32 ships its CRDs in a separate chart | `up.sh` applies `crd.projectcalico.org.v1` first |
| `Secret "sh.helm.release.v1.calico-crds" … Too long` | Calico CRDs exceed Helm's 1 MiB release limit | CRDs rendered with `helm template` and server-side applied |
| Calico upgrade conflicts on `.spec.calicoNetwork.ipPools` | Helm 4 server-side apply vs fields the operator owns | Calico is only (re)installed when the pinned version changes |
| `deployments.apps "bifrost" not found` | With SQLite persistence the chart runs a StatefulSet | Scripts use `statefulset/bifrost` |
| Edited virtual key has no effect after `make up` | Bifrost's default `split` mode keeps DB rows | `sourceOfTruth: config.json` |
| `Model '…' is not allowed for virtual key` | `allow_all_providers` permits no models, and a key without `models` relies on a catalog lookup that fails for invalid keys | Explicit `provider_configs` with `allowed_models: ["*"]` and provider keys with `models: ["*"]` |
| Jaeger `/api/services` returns 404 | Jaeger v2.22 serves the v3 query API | Smoke test uses `/api/v3/services` |

## Troubleshooting

- **`make up` hangs on Calico:** the node stays `NotReady` until Calico is
  running. Check `kubectl --context kind-agentory -n calico-system get pods`.
- **Bifrost `CrashLoopBackOff`:** run `kubectl -n bifrost logs statefulset/bifrost`;
  a bad provider key format shows up there.
- **Start over:** `make down && make up`. Generated keys survive unless
  `PURGE=1`.
