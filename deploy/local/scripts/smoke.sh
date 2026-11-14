#!/usr/bin/env bash
# End-to-end smoke test against a running local cluster (make up first).
# Checks: components ready, egress lockdown, Bifrost reachable and auth-enforced,
# traces flow collector -> Jaeger, and (if a provider key is configured) a real
# model call through a virtual key.
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_cmd kubectl
cluster_exists || die "cluster '${KIND_CLUSTER_NAME}' not found; run 'make up' first"

failures=0
pass() { ok "$*"; }
fail() { printf '\033[1;31m  ✗\033[0m %s\n' "$*" >&2; failures=$((failures + 1)); }

# assert_code <actual> <expected-regex> <description>
assert_code() {
  if [[ "$1" =~ ^($2)$ ]]; then pass "$3 ($1)"; else fail "$3: got $1, expected $2"; fi
}

# Run curl inside the locked-down probe pod. Prints the HTTP status, or 000 on
# connection failure.
probe_status() {
  k -n agentory-smoke exec probe -- curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$@" 2>/dev/null || true
}

# Like probe_status, but prints the response body followed by a final line with
# the HTTP status.
probe_body() {
  k -n agentory-smoke exec probe -- curl -s -w '\n%{http_code}' --max-time 30 "$@" 2>/dev/null || echo "000"
}

BIFROST_CHAT=http://bifrost.bifrost.svc.cluster.local:8080/v1/chat/completions

log "Components ready"
for target in "agentgateway-system deploy/agentgateway" "observability deploy/otel-collector" \
              "observability deploy/jaeger" "bifrost statefulset/bifrost"; do
  read -r ns obj <<<"$target"
  if k -n "$ns" rollout status "$obj" --timeout=60s >/dev/null 2>&1; then
    pass "$ns/$obj"
  else
    fail "$ns/$obj not ready"
  fi
done
if k -n agentgateway-system wait --for=condition=Programmed gateway/agentory-gateway --timeout=60s >/dev/null 2>&1; then
  pass "Gateway agentory-gateway programmed"
else
  fail "Gateway agentory-gateway not programmed"
fi

log "Probe pod with agent egress lockdown"
render "${LOCAL_DIR}/smoke/egress-test.yaml" | k apply -f - >/dev/null
k -n agentory-smoke wait --for=condition=Ready pod/probe --timeout=120s >/dev/null
pass "probe pod ready"

log "Egress lockdown (ADR-0002)"
assert_code "$(probe_status http://bifrost.bifrost.svc.cluster.local:8080/health)" "200" \
  "allowed: Bifrost /health"
assert_code "$(probe_status https://api.anthropic.com)" "000" \
  "blocked: direct call to api.anthropic.com"
assert_code "$(probe_status http://jaeger.observability.svc.cluster.local:16686)" "000" \
  "blocked: Jaeger (not on the allow-list)"

log "Bifrost enforces virtual keys"
# Bifrost resolves the provider before checking credentials, so with no provider
# configured every request fails with 400 before auth runs. Only meaningful
# with a provider (a dummy key is enough: the request never leaves the cluster).
provider_configured=0
k -n bifrost get secret bifrost-provider-keys >/dev/null 2>&1 && provider_configured=1
if ((provider_configured)); then
  if k -n bifrost get secret bifrost-provider-keys -o jsonpath='{.data.ANTHROPIC_API_KEY}' | grep -q .; then
    default_model="anthropic/claude-haiku-5-5"
  else
    default_model="openai/gpt-4o-mini"
  fi
  model="${SMOKE_MODEL:-$default_model}"
  body="{\"model\":\"${model}\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with: ok\"}]}"
  assert_code "$(probe_status -X POST -H 'Content-Type: application/json' -d "$body" "$BIFROST_CHAT")" "401" \
    "rejected: no virtual key"
  assert_code "$(probe_status -X POST -H 'Content-Type: application/json' -H 'x-bf-vk: sk-bf-not-a-key' \
    -d "$body" "$BIFROST_CHAT")" "401" "rejected: unknown virtual key"
else
  warn "skipped: needs a provider (re-run 'make up' with ANTHROPIC_API_KEY or OPENAI_API_KEY)"
fi

log "Tracing: collector → Jaeger"
trace_id="$(random_hex 16)"; span_id="$(random_hex 8)"; now_ns="$(date +%s)000000000"
span=$(cat <<EOF
{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"agentory-smoke"}}]},
 "scopeSpans":[{"spans":[{"traceId":"${trace_id}","spanId":"${span_id}","name":"smoke-test","kind":1,
 "startTimeUnixNano":"${now_ns}","endTimeUnixNano":"${now_ns}"}]}]}]}
EOF
)
assert_code "$(probe_status -X POST -H 'Content-Type: application/json' -d "$span" \
  http://otel-collector.observability.svc.cluster.local:4318/v1/traces)" "200" \
  "span accepted by collector"

found=0
for _ in $(seq 1 15); do
  if k get --raw "/api/v1/namespaces/observability/services/jaeger:16686/proxy/api/traces/${trace_id}" 2>/dev/null \
      | grep -q "smoke-test"; then found=1; break; fi
  sleep 2
done
if ((found)); then
  pass "trace ${trace_id} visible in Jaeger"
else
  fail "trace ${trace_id} not found in Jaeger after 30s"
fi

log "Model call through Bifrost with the dev virtual key"
if ((provider_configured)); then
  vk="$(cat "${SECRETS_DIR}/dev-virtual-key")"
  response="$(probe_body -X POST -H 'Content-Type: application/json' -H "x-bf-vk: ${vk}" -d "$body" "$BIFROST_CHAT")"
  code="$(tail -n1 <<<"$response")"
  if grep -q '"key":"' <<<"$response" || [[ "$code" == "200" ]]; then
    pass "virtual key accepted and routed to a provider key"
  else
    fail "virtual key not routed: $(head -c 200 <<<"$response")"
  fi
  if [[ "$code" == "200" ]]; then
    pass "${model} answered (200)"
  elif grep -q 'authentication_error' <<<"$response"; then
    fail "provider rejected the API key (expected with a dummy key; check ANTHROPIC_API_KEY/OPENAI_API_KEY)"
  else
    fail "${model} returned ${code}: $(head -c 200 <<<"$response") (set SMOKE_MODEL to a model your key can use)"
  fi
  found=0
  for _ in $(seq 1 15); do
    if k get --raw "/api/v1/namespaces/observability/services/jaeger:16686/proxy/api/v3/services" 2>/dev/null \
        | grep -q '"bifrost"'; then found=1; break; fi
    sleep 2
  done
  if ((found)); then pass "Bifrost spans visible in Jaeger"; else fail "no Bifrost spans in Jaeger after 30s"; fi
else
  warn "skipped: no provider key configured"
fi

echo
if ((failures)); then die "${failures} check(s) failed"; fi
ok "all smoke checks passed"
