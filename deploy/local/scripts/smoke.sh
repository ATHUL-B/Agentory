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

log "Operator (optional: make operator-up)"
if k get crd agents.agentory.dev >/dev/null 2>&1; then
  ns=agentory-smoke
  if k -n agentory-operator-system rollout status deploy/agentory-operator-controller-manager --timeout=60s >/dev/null 2>&1; then
    pass "controller running"
  else
    fail "controller not running"
  fi

  # Invalid spec: a harness agent must be perTask, enforced by CEL in the CRD.
  if out="$(k -n "$ns" apply -f - 2>&1 <<'EOF'
apiVersion: agentory.dev/v1alpha1
kind: Agent
metadata: { name: smoke-invalid }
spec:
  runtime: harness
  lifecycle: service
  harness: { kind: claude-code }
  model: { provider: mock, name: mock-model }
EOF
)"; then
    fail "invalid Agent was accepted"
    k -n "$ns" delete agent smoke-invalid --ignore-not-found >/dev/null
  elif grep -q "requires lifecycle 'perTask'" <<<"$out"; then
    pass "invalid Agent rejected by CRD validation"
  else
    fail "invalid Agent rejected for the wrong reason: ${out}"
  fi

  k -n "$ns" apply -f - >/dev/null <<'EOF'
apiVersion: agentory.dev/v1alpha1
kind: Agent
metadata: { name: smoke-agent }
spec:
  runtime: declarative
  lifecycle: service
  declarative: { instructions: "Smoke-test agent." }
  model: { provider: mock, name: mock-model }
---
apiVersion: agentory.dev/v1alpha1
kind: Task
metadata: { name: smoke-task }
spec:
  agentRef: { name: smoke-agent }
  input: { message: "Smoke-test task." }
EOF
  if k -n "$ns" wait --for=condition=Accepted agent/smoke-agent --timeout=30s >/dev/null 2>&1; then
    pass "Agent accepted by the controller"
  else
    fail "Agent not accepted within 30s"
  fi
  if k -n "$ns" wait --for=condition=AgentResolved task/smoke-task --timeout=30s >/dev/null 2>&1; then
    pass "Task resolved its Agent"
  else
    fail "Task did not resolve its Agent within 30s"
  fi
  agent_rev="$(k -n "$ns" get agent smoke-agent -o jsonpath='{.status.revision}')"
  task_rev="$(k -n "$ns" get task smoke-task -o jsonpath='{.status.agentRevision}')"
  if [[ -n "$agent_rev" && "$agent_rev" == "$task_rev" ]]; then
    pass "Task recorded the Agent revision (${agent_rev})"
  else
    fail "Task agentRevision '${task_rev}' != Agent revision '${agent_rev}'"
  fi
  k -n "$ns" delete task/smoke-task agent/smoke-agent --ignore-not-found --wait=false >/dev/null
else
  warn "skipped: operator not deployed"
fi

log "Container agent via DeploymentBackend (optional: make operator-up)"
if k get crd agents.agentory.dev >/dev/null 2>&1; then
  # A namespace with no other policies, so blocked traffic proves the Agent's own policy.
  e2e=agentory-e2e
  k create namespace "$e2e" --dry-run=client -o yaml | k apply -f - >/dev/null
  k -n "$e2e" apply -f - >/dev/null <<EOF
apiVersion: agentory.dev/v1alpha1
kind: Agent
metadata: { name: smoke-crew }
spec:
  runtime: container
  lifecycle: service
  container: { image: ${SMOKE_AGENT_IMAGE}, port: 8080 }
  model: { provider: mock, name: mock-model }
EOF
  if k -n "$e2e" wait --for=condition=Ready agent/smoke-crew --timeout=180s >/dev/null 2>&1; then
    pass "container Agent Ready (Deployment, Service, ServiceAccount, NetworkPolicy)"
    k -n "$e2e" rollout status deploy/smoke-crew --timeout=60s >/dev/null 2>&1 || true
    pod="$(k -n "$e2e" get pod -l agentory.dev/agent=smoke-crew -o jsonpath='{.items[0].metadata.name}')"
    agent_curl() {
      k -n "$e2e" exec "$pod" -- curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$1" 2>/dev/null || true
    }
    if [[ "$(k -n "$e2e" exec "$pod" -- printenv AGENTORY_LLM_BASE_URL 2>/dev/null)" == "http://bifrost.bifrost.svc.cluster.local:8080/v1" ]]; then
      pass "gateway environment injected"
    else
      fail "AGENTORY_LLM_BASE_URL missing or wrong in the agent pod"
    fi
    assert_code "$(agent_curl http://bifrost.bifrost.svc.cluster.local:8080/health)" "200" "agent egress allowed: LLM gateway"
    assert_code "$(agent_curl https://api.anthropic.com)" "000" "agent egress blocked: api.anthropic.com"
    assert_code "$(agent_curl http://mock-llm.mock-llm.svc.cluster.local:8080/healthz)" "000" \
      "agent egress blocked: services not on the allow-list"

    # Ingress: only the agent gateway's namespace may reach the agent.
    in_code() {  # namespace -> HTTP status of a request to the agent from a pod there
      local from="$1" probe="smoke-ingress-probe"
      k -n "$from" delete pod "$probe" --ignore-not-found >/dev/null 2>&1
      k -n "$from" run "$probe" --image="${CURL_IMAGE}" --restart=Never --command -- \
        curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://smoke-crew.${e2e}.svc.cluster.local:8080" >/dev/null
      k -n "$from" wait --for=jsonpath='{.status.phase}'=Succeeded "pod/$probe" --timeout=60s >/dev/null 2>&1 || true
      k -n "$from" logs "$probe" 2>/dev/null || echo "000"
      k -n "$from" delete pod "$probe" --ignore-not-found --wait=false >/dev/null 2>&1
    }
    assert_code "$(in_code agentgateway-system)" "200" "agent ingress allowed: from the agent gateway namespace"
    assert_code "$(in_code mock-llm)" "000" "agent ingress blocked: from other namespaces"
  else
    fail "container Agent not Ready within 180s: $(k -n "$e2e" get agent smoke-crew -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')"
  fi
  k -n "$e2e" delete agent smoke-crew --ignore-not-found --wait=false >/dev/null
else
  warn "skipped: operator not deployed"
fi

log "Per-agent virtual keys (optional: make operator-up)"
admin_pass_file="${SECRETS_DIR}/bifrost-admin-password"
if k get crd agents.agentory.dev >/dev/null 2>&1 && [[ -s "$admin_pass_file" ]] \
    && k -n agentory-operator-system get secret llm-gateway-admin >/dev/null 2>&1; then
  e2e=agentory-e2e
  admin_pass="$(cat "$admin_pass_file")"
  vk_api() {  # method path -> HTTP status of an admin call to Bifrost's governance API
    k -n agentory-smoke exec probe -- curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
      -u "${BIFROST_ADMIN_USER}:${admin_pass}" -X "$1" "http://bifrost.bifrost.svc.cluster.local:8080$2" 2>/dev/null || true
  }
  assert_code "$(probe_status http://bifrost.bifrost.svc.cluster.local:8080/api/governance/virtual-keys)" "401" \
    "Bifrost management API requires admin credentials"

  k -n "$e2e" apply -f - >/dev/null <<EOF
apiVersion: agentory.dev/v1alpha1
kind: Agent
metadata: { name: smoke-keyed }
spec:
  runtime: container
  lifecycle: service
  container: { image: ${SMOKE_AGENT_IMAGE}, port: 8080 }
  model: { provider: mock, name: mock-model }
EOF
  if k -n "$e2e" wait --for=condition=VirtualKeyReady agent/smoke-keyed --timeout=120s >/dev/null 2>&1 \
      && k -n "$e2e" wait --for=condition=Ready agent/smoke-keyed --timeout=180s >/dev/null 2>&1; then
    key_id() { k -n "$e2e" get secret smoke-keyed-llm -o jsonpath='{.metadata.annotations.agentory\.dev/virtual-key-id}'; }
    # The pod whose template carries the given key ID (skips pods being replaced).
    pod_for_key() {
      k -n "$e2e" rollout status deploy/smoke-keyed --timeout=120s >/dev/null 2>&1 || true
      k -n "$e2e" get pod -l agentory.dev/agent=smoke-keyed \
        -o jsonpath="{.items[?(@.metadata.annotations.agentory\.dev/virtual-key-id==\"$1\")].metadata.name}" | awk '{print $1}'
    }
    chat_from() {  # pod model -> HTTP status, using the key injected into the pod
      k -n "$e2e" exec "$1" -- sh -c "curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -H \"Authorization: Bearer \$OPENAI_API_KEY\" -H 'Content-Type: application/json' \
        \$AGENTORY_LLM_BASE_URL/chat/completions \
        -d '{\"model\":\"$2\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'" 2>/dev/null || true
    }
    id="$(key_id)"
    pass "Agent has its own key (${id}) in Secret smoke-keyed-llm"
    assert_code "$(vk_api GET "/api/governance/virtual-keys/${id}")" "200" "key exists in Bifrost"
    pod="$(pod_for_key "$id")"
    assert_code "$(chat_from "$pod" mock/mock-model)" "200" "agent pod calls its model with the injected key"
    assert_code "$(chat_from "$pod" mock/other-model)" "403" "the key allows only the agent's own model"

    # Bifrost loses the key: the operator re-creates it and rolls the pods.
    vk_api DELETE "/api/governance/virtual-keys/${id}" >/dev/null
    k -n "$e2e" annotate agent smoke-keyed agentory.dev/smoke-poke="$(date +%s)" --overwrite >/dev/null
    new_id="$id"
    for _ in $(seq 1 30); do new_id="$(key_id)"; [[ "$new_id" != "$id" ]] && break; sleep 1; done
    if [[ "$new_id" != "$id" ]]; then
      pass "lost key re-created (${new_id})"
      pod="$(pod_for_key "$new_id")"
      assert_code "$(chat_from "$pod" mock/mock-model)" "200" "restarted pod works with the rotated key"
    else
      fail "lost key was not re-created within 30s"
    fi

    k -n "$e2e" delete agent smoke-keyed --timeout=60s >/dev/null 2>&1
    assert_code "$(vk_api GET "/api/governance/virtual-keys/${new_id}")" "404" "deleting the Agent deleted its key"
  else
    fail "keyed Agent not Ready: $(k -n "$e2e" get agent smoke-keyed -o jsonpath='{.status.conditions[?(@.status=="False")].message}')"
    k -n "$e2e" delete agent smoke-keyed --ignore-not-found --wait=false >/dev/null
  fi
else
  warn "skipped: operator not deployed with Bifrost credentials"
fi

log "A2A through the agent gateway (optional: make operator-up)"
if k get crd agents.agentory.dev >/dev/null 2>&1 && k get crd httproutes.gateway.networking.k8s.io >/dev/null 2>&1; then
  e2e=agentory-e2e
  for name in smoke-echo smoke-caller; do
    k -n "$e2e" apply -f - >/dev/null <<EOF
apiVersion: agentory.dev/v1alpha1
kind: Agent
metadata: { name: ${name} }
spec:
  description: Smoke-test agent ${name}
  runtime: container
  lifecycle: service
  container: { image: "${ECHO_AGENT_IMAGE}", port: 8080 }
  model: { provider: mock, name: mock-model }
EOF
  done
  if k -n "$e2e" wait --for=condition=Ready agent/smoke-echo agent/smoke-caller --timeout=180s >/dev/null 2>&1; then
    pass "two A2A agents Ready (route accepted by the gateway)"
    k -n "$e2e" rollout status deploy/smoke-echo --timeout=60s >/dev/null 2>&1 || true
    k -n "$e2e" rollout status deploy/smoke-caller --timeout=60s >/dev/null 2>&1 || true
    revision="$(k -n "$e2e" get agent smoke-echo -o jsonpath='{.status.revision}')"
    pod="$(k -n "$e2e" get pod -l agentory.dev/agent=smoke-caller -o jsonpath='{.items[0].metadata.name}')"
    # Run from inside the caller's locked-down pod, using only what the operator injected.
    out="$(k -n "$e2e" exec -i "$pod" -- python - 2>&1 <<'PY' || true
import json, os, time, urllib.error, urllib.request
base = os.environ["AGENTORY_A2A_URL"] + "/agents/agentory-e2e/smoke-echo"
# Ready means the gateway accepted the route; its proxy picks the route up a
# few seconds later (503 until then), so retry briefly like a real client.
for attempt in range(30):
    try:
        with urllib.request.urlopen(base + "/.well-known/agent-card.json", timeout=10) as r:
            card = json.load(r)
        break
    except urllib.error.HTTPError as e:
        if e.code != 503 or attempt == 29:
            raise
        time.sleep(1)
print("CARD_VERSION=" + card.get("version", ""))
print("CARD_URL_OK=" + str(card.get("url") == base))
req = {"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": {"message": {
    "role": "user", "messageId": "m1", "parts": [{"kind": "text", "text": "smoke ping"}]}}}
r = urllib.request.Request(base + "/", data=json.dumps(req).encode(),
                           headers={"Content-Type": "application/json"}, method="POST")
with urllib.request.urlopen(r, timeout=20) as resp:
    print("REPLY=" + json.load(resp)["result"]["parts"][0]["text"])
try:
    urllib.request.urlopen("http://smoke-echo.agentory-e2e.svc.cluster.local:8080/healthz", timeout=5)
    print("DIRECT=allowed")
except Exception:
    print("DIRECT=blocked")
PY
)"
    if grep -q "CARD_VERSION=${revision}" <<<"$out" && grep -q "CARD_URL_OK=True" <<<"$out"; then
      pass "generated Agent Card served through the gateway (version ${revision})"
    else
      fail "Agent Card via gateway wrong: ${out}"
    fi
    if grep -q "REPLY=mock reply: smoke ping" <<<"$out"; then
      pass "agent-to-agent message/send via gateway -> echo -> LLM gateway -> model"
    else
      fail "A2A call failed: ${out}"
    fi
    if grep -q "DIRECT=blocked" <<<"$out"; then
      pass "direct agent-to-agent call bypassing the gateway is blocked"
    else
      fail "direct call was not blocked: ${out}"
    fi
  else
    fail "A2A agents not Ready: $(k -n "$e2e" get agent smoke-echo -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')"
  fi
  k -n "$e2e" delete agent smoke-echo smoke-caller --ignore-not-found --timeout=60s >/dev/null 2>&1
  if k -n "$e2e" get httproute smoke-echo >/dev/null 2>&1; then
    fail "route left behind after deleting the Agent"
  else
    pass "deleting the Agent removed its route"
  fi
else
  warn "skipped: operator or Gateway API not installed"
fi

echo
if ((failures)); then die "${failures} check(s) failed"; fi
ok "all smoke checks passed"
