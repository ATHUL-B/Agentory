#!/usr/bin/env bash
# Create (or update) the local Agentory dev cluster. Safe to re-run.
#
# Optional env: ANTHROPIC_API_KEY and/or OPENAI_API_KEY to enable real model
# calls through Bifrost. Without them the gateways still come up.
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_cmd docker kind kubectl helm
require_docker_running
mkdir -p "${SECRETS_DIR}"

log "kind cluster '${KIND_CLUSTER_NAME}'"
if cluster_exists; then
  ok "already exists"
else
  kind create cluster --name "${KIND_CLUSTER_NAME}" --config "${LOCAL_DIR}/kind-config.yaml" --wait 0s
  ok "created"
fi

log "Helm repositories"
add_helm_repos
ok "updated"

log "Calico ${CALICO_CHART_VERSION} (NetworkPolicy enforcement)"
# Since Calico v3.32 the CRDs ship in their own chart and must be installed first.
# The rendered CRDs exceed Helm's 1 MiB release-Secret limit, so apply them
# directly (server-side, as the CRDs are also too large for client-side apply).
helm template calico-crds projectcalico/crd.projectcalico.org.v1 --version "${CALICO_CHART_VERSION}" \
  | k apply --server-side --force-conflicts -f - >/dev/null
k wait --for=condition=Established crd/installations.operator.tigera.io --timeout=2m >/dev/null
# The operator takes ownership of fields in the Installation it reconciles, so a
# Helm (server-side) upgrade at the same version conflicts with it. Only install
# or change Calico when the pinned version differs from what is deployed.
if [[ "$(release_chart tigera-operator calico)" == "tigera-operator-${CALICO_CHART_VERSION}" ]]; then
  ok "already at ${CALICO_CHART_VERSION}"
else
  h upgrade --install calico projectcalico/tigera-operator \
    --version "${CALICO_CHART_VERSION}" --namespace tigera-operator --create-namespace \
    -f "${LOCAL_DIR}/values/calico.yaml" --wait --timeout 10m >/dev/null
fi
k wait --for=condition=Ready nodes --all --timeout=10m >/dev/null
ok "nodes Ready"

log "Gateway API ${GATEWAY_API_VERSION} CRDs"
k apply --server-side -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" >/dev/null
ok "applied"

log "agentgateway ${AGENTGATEWAY_CHART_VERSION}"
h upgrade --install agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds \
  --version "${AGENTGATEWAY_CHART_VERSION}" --namespace agentgateway-system --create-namespace \
  --wait --timeout 5m >/dev/null
h upgrade --install agentgateway oci://cr.agentgateway.dev/charts/agentgateway \
  --version "${AGENTGATEWAY_CHART_VERSION}" --namespace agentgateway-system \
  -f "${LOCAL_DIR}/values/agentgateway.yaml" --wait --timeout 10m >/dev/null
k apply -f "${LOCAL_DIR}/manifests/gateway.yaml" >/dev/null
k -n agentgateway-system wait --for=condition=Programmed gateway/agentory-gateway --timeout=5m >/dev/null
ok "controller running, Gateway programmed"

log "Tracing: Jaeger + OTel collector ${OTEL_COLLECTOR_CHART_VERSION}"
k create namespace observability --dry-run=client -o yaml | k apply -f - >/dev/null
render "${LOCAL_DIR}/manifests/jaeger.yaml" | k apply -f - >/dev/null
h upgrade --install otel-collector open-telemetry/opentelemetry-collector \
  --version "${OTEL_COLLECTOR_CHART_VERSION}" --namespace observability \
  -f "${LOCAL_DIR}/values/otel-collector.yaml" --wait --timeout 10m >/dev/null
k -n observability rollout status deploy/jaeger --timeout=5m >/dev/null
ok "collector and Jaeger running"

log "Mock LLM (provider \"mock\", no API key needed)"
k create namespace mock-llm --dry-run=client -o yaml | k apply -f - >/dev/null
k -n mock-llm create configmap mock-llm-code --from-file="${REPO_DIR}/tools/mock-llm/mock_llm.py" \
  --dry-run=client -o yaml | k apply -f - >/dev/null
render "${LOCAL_DIR}/manifests/mock-llm.yaml" | k apply -f - >/dev/null
# Restart so code changes in the ConfigMap are picked up on re-runs.
k -n mock-llm rollout restart deploy/mock-llm >/dev/null
k -n mock-llm rollout status deploy/mock-llm --timeout=3m >/dev/null
ok "running"

log "Bifrost ${BIFROST_CHART_VERSION} (LLM gateway)"
k create namespace bifrost --dry-run=client -o yaml | k apply -f - >/dev/null

# Encryption key and dev virtual key are generated once and reused on re-runs.
[[ -s "${SECRETS_DIR}/bifrost-encryption-key" ]] || random_hex 32 > "${SECRETS_DIR}/bifrost-encryption-key"
[[ -s "${SECRETS_DIR}/dev-virtual-key" ]] || echo "sk-bf-$(random_hex 24)" > "${SECRETS_DIR}/dev-virtual-key"
[[ -s "${SECRETS_DIR}/bifrost-admin-password" ]] || random_hex 24 > "${SECRETS_DIR}/bifrost-admin-password"
chmod 600 "${SECRETS_DIR}"/*

k -n bifrost create secret generic bifrost-admin --from-literal=username="${BIFROST_ADMIN_USER}" \
  --from-file=password="${SECRETS_DIR}/bifrost-admin-password" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

k -n bifrost create secret generic bifrost-encryption \
  --from-file=encryption-key="${SECRETS_DIR}/bifrost-encryption-key" \
  --dry-run=client -o yaml | k apply -f - >/dev/null

provider_args=()
[[ -n "${ANTHROPIC_API_KEY:-}" ]] && provider_args+=(--from-literal=ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY}")
[[ -n "${OPENAI_API_KEY:-}" ]] && provider_args+=(--from-literal=OPENAI_API_KEY="${OPENAI_API_KEY}")
if ((${#provider_args[@]})); then
  k -n bifrost create secret generic bifrost-provider-keys "${provider_args[@]}" \
    --dry-run=client -o yaml | k apply -f - >/dev/null
else
  # Remove keys from a previous run so the cluster matches this invocation.
  k -n bifrost delete secret bifrost-provider-keys --ignore-not-found >/dev/null
  warn "No ANTHROPIC_API_KEY or OPENAI_API_KEY set: only the mock provider (mock/mock-model) is available."
fi

bifrost_generated_values "$(cat "${SECRETS_DIR}/dev-virtual-key")" > "${SECRETS_DIR}/bifrost-generated.yaml"
h upgrade --install bifrost bifrost/bifrost \
  --version "${BIFROST_CHART_VERSION}" --namespace bifrost --set image.tag="${BIFROST_IMAGE_TAG}" \
  -f "${LOCAL_DIR}/values/bifrost.yaml" -f "${SECRETS_DIR}/bifrost-generated.yaml" \
  --wait --timeout 10m >/dev/null
# Pick up changed provider keys on re-runs (the Secret is read only at start-up).
k -n bifrost rollout restart statefulset/bifrost >/dev/null
k -n bifrost rollout status statefulset/bifrost --timeout=5m >/dev/null
ok "running"

cat <<EOF

Local cluster is up (context: ${KUBE_CONTEXT}).

  Smoke test:     make smoke
  Jaeger UI:      kubectl --context ${KUBE_CONTEXT} -n observability port-forward svc/jaeger 16686  → http://localhost:16686
  Bifrost UI:     kubectl --context ${KUBE_CONTEXT} -n bifrost port-forward svc/bifrost 8080       → http://localhost:8080
  Dev virtual key: ${SECRETS_DIR}/dev-virtual-key
  Bifrost admin:  ${BIFROST_ADMIN_USER} / ${SECRETS_DIR}/bifrost-admin-password
  Mock model:     mock/mock-model (deterministic, no API key)
  Tear down:      make down
EOF
