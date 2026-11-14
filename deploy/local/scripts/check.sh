#!/usr/bin/env bash
# Static checks that need no cluster or Docker: shell syntax, chart rendering
# with our values, and OTel collector config validation (if otelcol-contrib is
# installed). Run in CI and before every commit touching deploy/local.
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_cmd helm
failures=0
fail() { printf '\033[1;31m  ✗\033[0m %s\n' "$*" >&2; failures=$((failures + 1)); }
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

log "Shell syntax"
for f in "${LOCAL_DIR}"/scripts/*.sh; do
  if bash -n "$f"; then ok "$(basename "$f")"; else fail "$(basename "$f")"; fi
done
if command -v shellcheck >/dev/null 2>&1; then
  if shellcheck -x -P SCRIPTDIR "${LOCAL_DIR}"/scripts/*.sh; then ok "shellcheck"; else fail "shellcheck"; fi
else
  warn "shellcheck not installed; skipped"
fi

log "Unit tests"
for tool in tools/mock-llm tools/echo-agent tools/oncall-mcp harness/claude-code runtime/declarative; do
  if python3 -m unittest discover -s "${REPO_DIR}/${tool}" -q > "${tmp}/unittest.log" 2>&1; then
    ok "${tool} ($(grep -oE 'Ran [0-9]+ tests' "${tmp}/unittest.log"))"
  else
    fail "${tool} unit tests: $(tail -5 "${tmp}/unittest.log")"
  fi
done

log "Chart rendering with our values"
add_helm_repos
render_chart() {  # name, chart, version, values...
  local name="$1" chart="$2" version="$3"; shift 3
  local args=() v
  for v in "$@"; do args+=(-f "$v"); done
  if helm template "$name" "$chart" --version "$version" ${args[@]+"${args[@]}"} > "${tmp}/${name}.yaml" 2> "${tmp}/${name}.err"; then
    ok "$name"
  else
    fail "$name: $(tail -3 "${tmp}/${name}.err")"
  fi
}
ANTHROPIC_API_KEY=dummy OPENAI_API_KEY=dummy bifrost_generated_values "sk-bf-check" > "${tmp}/bifrost-generated.yaml"
render_chart calico-crds projectcalico/crd.projectcalico.org.v1 "${CALICO_CHART_VERSION}"
render_chart calico projectcalico/tigera-operator "${CALICO_CHART_VERSION}" "${LOCAL_DIR}/values/calico.yaml"
render_chart agentgateway oci://cr.agentgateway.dev/charts/agentgateway "${AGENTGATEWAY_CHART_VERSION}" "${LOCAL_DIR}/values/agentgateway.yaml"
render_chart otel-collector open-telemetry/opentelemetry-collector "${OTEL_COLLECTOR_CHART_VERSION}" "${LOCAL_DIR}/values/otel-collector.yaml"
printf 'image:\n  tag: %s\n' "${BIFROST_IMAGE_TAG}" > "${tmp}/bifrost-image.yaml"
render_chart bifrost bifrost/bifrost "${BIFROST_CHART_VERSION}" "${LOCAL_DIR}/values/bifrost.yaml" \
  "${tmp}/bifrost-generated.yaml" "${tmp}/bifrost-image.yaml"

log "Rendered output matches what the manifests expect"
expect() {  # file, pattern, description
  if grep -qE "$2" "${tmp}/$1"; then ok "$3"; else fail "$3 (pattern '$2' not found in $1)"; fi
}
expect calico-crds.yaml 'name: installations.operator.tigera.io' "Calico CRD chart provides the Installation CRD the operator chart needs"
expect otel-collector.yaml 'app.kubernetes.io/name: opentelemetry-collector' "collector pods carry the label the smoke NetworkPolicy selects"
expect otel-collector.yaml 'name: otel-collector$' "collector Service is named otel-collector"
expect otel-collector.yaml 'otlp_grpc/jaeger' "collector exports traces to Jaeger"
expect bifrost.yaml 'name: bifrost$' "Bifrost Service is named bifrost"
expect bifrost.yaml 'otel-collector.observability.svc.cluster.local:4317' "Bifrost OTel plugin points at the collector"
expect bifrost.yaml 'vk-agentory-dev' "dev virtual key rendered into Bifrost config"
expect bifrost.yaml 'env.ANTHROPIC_API_KEY' "provider keys referenced by env var, not inlined"
expect bifrost.yaml 'mock-llm.mock-llm.svc.cluster.local:8080' "mock provider points at the mock LLM Service"
expect bifrost.yaml '"base_provider_type": ?"openai"' "mock provider is an OpenAI-compatible custom provider"

log "OTel collector config validation"
if command -v otelcol-contrib >/dev/null 2>&1; then
  # Extract the collector config from the rendered ConfigMap (key "relay").
  awk '/^  relay: \|/{f=1; next} f && /^  [^ ]/{f=0} f{sub(/^    /,""); print}' \
    "${tmp}/otel-collector.yaml" > "${tmp}/relay.yaml"
  if MY_POD_IP=127.0.0.1 otelcol-contrib validate --config="${tmp}/relay.yaml" > "${tmp}/validate.log" 2>&1; then
    ok "otelcol-contrib validate"
  else
    fail "otelcol-contrib validate: $(tail -5 "${tmp}/validate.log")"
  fi
else
  warn "otelcol-contrib not installed; skipped (see deploy/local/README.md)"
fi

echo
if ((failures)); then die "${failures} check(s) failed"; fi
ok "all static checks passed"
