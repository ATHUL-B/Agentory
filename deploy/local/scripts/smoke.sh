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
              "observability deploy/jaeger" "mock-llm deploy/mock-llm" "bifrost statefulset/bifrost"; do
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
# The in-cluster mock provider is always configured, so auth is always
# observable. (With no provider at all, Bifrost answers 400 before checking keys.)
vk="$(cat "${SECRETS_DIR}/dev-virtual-key")"
json_post=(-X POST -H 'Content-Type: application/json')
mock_body='{"model":"mock/mock-model","messages":[{"role":"user","content":"Reply with: ok"}]}'
assert_code "$(probe_status "${json_post[@]}" -d "$mock_body" "$BIFROST_CHAT")" "401" \
  "rejected: no virtual key"
assert_code "$(probe_status "${json_post[@]}" -H 'x-bf-vk: sk-bf-not-a-key' -d "$mock_body" "$BIFROST_CHAT")" "401" \
  "rejected: unknown virtual key"

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

log "Model calls through Bifrost (mock provider, no API key needed)"
response="$(probe_body "${json_post[@]}" -H "x-bf-vk: ${vk}" -d "$mock_body" "$BIFROST_CHAT")"
assert_code "$(tail -n1 <<<"$response")" "200" "mock/mock-model via the dev virtual key"
if grep -q '"mock reply: Reply with: ok"' <<<"$response"; then
  pass "response came from the mock provider"
else
  fail "unexpected response: $(head -c 200 <<<"$response")"
fi
if grep -q '"total_tokens":[1-9]' <<<"$response"; then
  pass "token usage reported"
else
  fail "no token usage in response"
fi

tool_body='{"model":"mock/mock-model","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{}}}}]}'
response="$(probe_body "${json_post[@]}" -H "x-bf-vk: ${vk}" -d "$tool_body" "$BIFROST_CHAT")"
if grep -q '"finish_reason":"tool_calls"' <<<"$response" && grep -q '"get_weather"' <<<"$response"; then
  pass "tool call passed through Bifrost"
else
  fail "no tool call in response: $(head -c 200 <<<"$response")"
fi

found=0
for _ in $(seq 1 15); do
  if k get --raw "/api/v1/namespaces/observability/services/jaeger:16686/proxy/api/v3/services" 2>/dev/null \
      | grep -q '"bifrost"'; then found=1; break; fi
  sleep 2
done
if ((found)); then pass "Bifrost spans visible in Jaeger"; else fail "no Bifrost spans in Jaeger after 30s"; fi

log "Real provider (optional)"
if k -n bifrost get secret bifrost-provider-keys >/dev/null 2>&1; then
  if k -n bifrost get secret bifrost-provider-keys -o jsonpath='{.data.ANTHROPIC_API_KEY}' | grep -q .; then
    default_model="anthropic/claude-haiku-5-5"
  else
    default_model="openai/gpt-4o-mini"
  fi
  model="${SMOKE_MODEL:-$default_model}"
  body="{\"model\":\"${model}\",\"max_tokens\":16,\"messages\":[{\"role\":\"user\",\"content\":\"Reply with: ok\"}]}"
  response="$(probe_body "${json_post[@]}" -H "x-bf-vk: ${vk}" -d "$body" "$BIFROST_CHAT")"
  code="$(tail -n1 <<<"$response")"
  if [[ "$code" == "200" ]]; then
    pass "${model} answered (200)"
  elif grep -q 'authentication_error' <<<"$response"; then
    fail "provider rejected the API key (check ANTHROPIC_API_KEY/OPENAI_API_KEY)"
  else
    fail "${model} returned ${code}: $(head -c 200 <<<"$response") (set SMOKE_MODEL to a model your key can use)"
  fi
else
  warn "skipped: no ANTHROPIC_API_KEY or OPENAI_API_KEY configured"
fi

echo
if ((failures)); then die "${failures} check(s) failed"; fi
ok "all smoke checks passed"
