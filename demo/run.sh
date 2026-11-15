#!/usr/bin/env bash
# Agentory end-to-end demo (backlog item 9): an incident in the payments team.
#
#   1. A no-code on-call agent (instructions + a runbook skill + MCP tools)
#      finds the bad deploy, and the whole run shows up as one trace.
#   2. A coding agent: a Task runs Claude Code in its own pod and reports its
#      result, token usage, cost and trace.
#   3. A team's own multi-agent container asks the on-call agent over A2A; the
#      cross-agent call is one trace.
#   4. Lockdown: from inside an agent, the internet, other services and tools
#      off its allow-list are all blocked. Only the gateways answer.
#
# Usage:  make demo          (pauses between steps when run in a terminal)
#         DEMO_AUTO=1 make demo     no pauses (CI, recordings)
#         make demo-clean           remove everything the demo created
# With a real model (needs its key at `make up`):
#         DEMO_PROVIDER=anthropic DEMO_MODEL=<model> make demo
#
# Every step is also checked; the script exits non-zero if any check fails.
# shellcheck source=../deploy/local/scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../deploy/local/scripts/lib.sh"
# Keep going after a failed step: each one is reported by check, and the exit
# status says whether all passed. Setup problems still stop the demo (die).
set +e

DEMO_DIR="${REPO_DIR}/demo"
NS=team-payments
REGISTRY_NS=agentory-mcp
MODEL_PROVIDER="${DEMO_PROVIDER:-mock}"
MODEL_NAME="${DEMO_MODEL:-mock-model}"
GW_PORT="${DEMO_GATEWAY_PORT:-18080}"
JAEGER_PORT="${DEMO_JAEGER_PORT:-16686}"
GW_URL="http://localhost:${GW_PORT}"
JAEGER_URL="http://localhost:${JAEGER_PORT}"
INTERACTIVE=0
if [[ -t 0 && -z "${DEMO_AUTO:-}" ]]; then INTERACTIVE=1; fi
failures=0

# ------------------------------------------------------------------ presentation

bold() { printf '\033[1m%s\033[0m\n' "$*"; }
say()  { printf '\033[0;36m%s\033[0m\n' "$*"; }
scene() { printf '\n\033[1;35m━━━ %s ━━━\033[0m\n' "$*"; }
pause() {
  if ((INTERACTIVE)); then
    printf '\033[2m  [enter]\033[0m'
    read -r _ </dev/tty
  fi
}
# show <command...>: print the command the audience would type, then run it.
show() {
  local cmd="$*"
  printf '\033[1;33m$ %s\033[0m\n' "${cmd/#k /kubectl }"
  "$@"
}
indent() { sed -e "s/^/${1:-  }/"; }  # indent [prefix]: indent stdin
# Print 0 (holds) or 1, to feed check.
equals()   { if [[ "$1" == "$2" ]]; then echo 0; else echo 1; fi; }
contains() { if [[ "$1" == *"$2"* ]]; then echo 0; else echo 1; fi; }
check() {  # check <exit status> <description>
  if [[ "$1" == 0 ]]; then ok "$2"; else printf '\033[1;31m  ✗\033[0m %s\n' "$2"; failures=$((failures + 1)); fi
}

# The mock model acts on [[tool:NAME {json}]] directives so the run is
# deterministic; a real model decides for itself, so it gets plain questions.
directive() {
  if [[ "${MODEL_PROVIDER}" == mock ]]; then printf ' [[tool:%s %s]]' "$1" "$2"; fi
}
# expect_mock <text>: an --expect check, only when the answer is predictable.
EXPECT=()
expect_mock() {
  EXPECT=()
  if [[ "${MODEL_PROVIDER}" == mock ]]; then EXPECT=(--expect "$1"); fi
}

render_manifest() {
  sed -e "s|\${MODEL_PROVIDER}|${MODEL_PROVIDER}|g" \
      -e "s|\${MODEL_NAME}|${MODEL_NAME}|g" \
      -e "s|\${INCIDENT_COMMANDER_IMAGE}|${INCIDENT_COMMANDER_IMAGE}|g" \
      "$1"
}
# apply_manifest <file>: show the YAML (minus comments), then apply it.
apply_manifest() {
  local file="${DEMO_DIR}/manifests/$1"
  printf '\033[1;33m$ kubectl apply -f demo/manifests/%s\033[0m\n' "$1"
  render_manifest "$file" | grep -v '^#' | sed -e 's/^/    /'
  render_manifest "$file" | k apply -f - >/dev/null
}
wait_ready() {  # wait_ready <agent>
  if k -n "$NS" wait --for=condition=Ready "agent/$1" --timeout=180s >/dev/null 2>&1; then
    k -n "$NS" rollout status "deploy/$1" --timeout=90s >/dev/null 2>&1 || true
    return 0
  fi
  warn "$1 not Ready: $(k -n "$NS" get agent "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}')"
  return 1
}
ask() {  # ask <agent> <trace> <context> <text> [demo.py args...]
  local agent="$1" trace="$2" context="$3" text="$4"; shift 4
  printf '\033[1;33m> %s\033[0m\n' "$text"
  python3 "${DEMO_DIR}/demo.py" ask --gateway "$GW_URL" --agent "${NS}/${agent}" \
    --text "$text" --trace "$trace" --context "$context" "$@" | sed -e 's/^/  /'
}
show_trace() {  # show_trace <trace> <service...>
  local trace="$1" s args=(); shift
  for s in "$@"; do args+=(--expect-service "$s"); done
  bold "Trace ${trace}   (${JAEGER_URL}/trace/${trace})"
  python3 "${DEMO_DIR}/demo.py" trace --jaeger "$JAEGER_URL" --id "$trace" "${args[@]}" | sed -e 's/^/  /'
}

# ------------------------------------------------------------------ setup

PF_PIDS=()
cleanup() {
  local pid
  for pid in ${PF_PIDS[@]+"${PF_PIDS[@]}"}; do kill "$pid" 2>/dev/null || true; done
}
trap cleanup EXIT

port_forward() {  # port_forward <namespace> <service> <local:remote>
  local log port="${3%%:*}" _
  log="$(mktemp -t agentory-demo-pf.XXXXXX)"
  k -n "$1" port-forward "svc/$2" "$3" >"$log" 2>&1 &
  PF_PIDS+=($!)
  for _ in $(seq 1 30); do
    if curl -s -o /dev/null "http://localhost:${port}/"; then return 0; fi
    sleep 0.5
  done
  die "port-forward to $1/$2 on :${port} failed: $(cat "$log")"
}

preflight() {
  require_cmd kubectl python3 curl kind
  cluster_exists || die "cluster '${KIND_CLUSTER_NAME}' not found: run 'make up' first"
  k -n agentory-operator-system get deploy agentory-operator-controller-manager >/dev/null 2>&1 \
    || die "the operator is not deployed: run 'make operator-up'"
  k get crd mcpservers.agentory.dev >/dev/null 2>&1 || die "MCPServer CRD missing: run 'make operator-up'"
  k -n mcp-demo get deploy oncall-mcp >/dev/null 2>&1 || die "demo MCP server missing: run 'make up'"
  if [[ "${MODEL_PROVIDER}" != mock && -z "${DEMO_MODEL:-}" ]]; then
    die "DEMO_PROVIDER=${MODEL_PROVIDER} needs DEMO_MODEL (the provider's model id)"
  fi
  k create namespace "$NS" --dry-run=client -o yaml | k apply -f - >/dev/null
  k create namespace "$REGISTRY_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
  port_forward agentgateway-system agentory-gateway "${GW_PORT}:8080"
  port_forward observability jaeger "${JAEGER_PORT}:16686"
}

clean() {
  log "Removing the demo"
  k delete namespace "$NS" --ignore-not-found --timeout=120s >/dev/null
  k -n "$REGISTRY_NS" delete mcpserver oncall --ignore-not-found >/dev/null
  ok "removed namespace ${NS} and MCPServer ${REGISTRY_NS}/oncall"
}

# ------------------------------------------------------------------ scenarios

scenario_oncall() {
  scene "1 · A no-code on-call agent"
  say "Checkout is failing. The payments team wants an on-call agent, but nobody on"
  say "the team writes code. The platform team has published an MCP server once:"
  apply_manifest 00-tool-registry.yaml
  pause
  say "The payments team writes instructions, a runbook skill, and which tools it may use:"
  apply_manifest 01-oncall-agent.yaml
  wait_ready payments-oncall; check $? "payments-oncall is Ready"
  show k -n "$NS" get agent payments-oncall \
    -o jsonpath='{range .status.conditions[*]}{"  "}{.type}{"="}{.status}{" ("}{.reason}{")\n"}{end}'
  say "The operator created the rest: a pod, its own LLM virtual key (its budget),"
  say "a network lockdown, an A2A route + Agent Card, and an MCP route with a tool allow-list:"
  show k -n "$NS" get deploy,svc,networkpolicy,secret,httproute,agentgatewaybackend,agentgatewaypolicy \
    -l agentory.dev/agent=payments-oncall -o name
  pause

  local trace context
  trace="$(random_hex 16)"; context="incident-$(random_hex 4)"
  say "Ask it over A2A, through the agent gateway:"
  expect_mock "v2.31.0"
  ask payments-oncall "$trace" "$context" \
    "Checkout is failing. What changed?$(directive oncall_get_recent_deploys '{"service": "checkout"}')" \
    ${EXPECT[@]+"${EXPECT[@]}"}
  check $? "the agent found deploy v2.31.0 with its tool"
  expect_mock "18.4%"
  ask payments-oncall "$trace" "$context" \
    "How bad is it?$(directive oncall_get_error_rate '{"service": "checkout"}')" \
    ${EXPECT[@]+"${EXPECT[@]}"}
  check $? "same conversation: the agent checked the error rate"
  pause
  say "Both turns as one trace: the agent, each tool call, and each model call with the"
  say "agent's own key and token counts (what per-team budgets are built on):"
  show_trace "$trace" payments-oncall bifrost
  check $? "the trace has the agent, its tool calls and its model calls"
  pause
}

scenario_coder() {
  scene "2 · A coding agent, one pod per Task"
  say "Now prepare the rollback. A harness agent runs Claude Code in a fresh,"
  say "locked-down pod for every Task; the pod is gone when the Task ends:"
  apply_manifest 02-coding-agent.yaml
  wait_ready payments-coder; check $? "payments-coder is Ready"
  local task message yaml phase="" last="" trace _
  task="rollback-checkout-$(random_hex 3)"
  message="Checkout broke after deploy v2.31.0. Write ROLLBACK.md with the plan to roll back to v2.30.4."
  # For the mock: one Bash call that writes the plan.
  message+="$(directive Bash '{"command": "printf '"'"'%s\\n'"'"' '"'"'# Roll back checkout to v2.30.4'"'"' '"'"'Cause: v2.31.0 raised errors to 18.4%.'"'"' '"'"'1. kubectl rollout undo deploy/checkout'"'"' '"'"'2. Watch the error rate return to 0.3%.'"'"' > ROLLBACK.md", "description": "Write the rollback plan"}')"
  yaml="apiVersion: agentory.dev/v1alpha1
kind: Task
metadata: { name: ${task}, namespace: ${NS} }
spec:
  agentRef: { name: payments-coder }
  input:
    message: |
      ${message}
  timeout: 10m"
  printf '\033[1;33m$ kubectl apply -f -\033[0m\n'
  indent "    " <<<"$yaml"
  k apply -f - >/dev/null <<<"$yaml"
  for _ in $(seq 1 150); do
    phase="$(k -n "$NS" get task "$task" -o jsonpath='{.status.phase}' 2>/dev/null)"
    if [[ -n "$phase" && "$phase" != "$last" ]]; then say "  Task ${task}: ${phase}"; last="$phase"; fi
    [[ "$phase" == Succeeded || "$phase" == Failed ]] && break
    sleep 2
  done
  check "$(equals "$phase" Succeeded)" "the Task Succeeded"
  show k -n "$NS" get tasks -o wide
  say "The result is on the Task: what changed, tokens, cost and the trace:"
  k -n "$NS" get task "$task" -o json | python3 -c '
import json, sys
s = json.load(sys.stdin)["status"]
r, u = s.get("result") or {}, s.get("usage") or {}
print("  summary:  " + " ".join((r.get("summary") or "").split())[:160])
print("  files:    " + ", ".join(a.get("ref", "") for a in r.get("artifacts") or []))
print("  tokens:   %s in / %s out" % (u.get("inputTokens", 0), u.get("outputTokens", 0)))
print("  cost:     $%s" % (u.get("costUsd") or "0"))
print("  trace:    " + s.get("traceId", ""))'
  local files
  files="$(k -n "$NS" get task "$task" -o jsonpath='{.status.result.artifacts[*].ref}')"
  check "$(contains "$files" ROLLBACK.md)" "the Task reports ROLLBACK.md"
  trace="$(k -n "$NS" get task "$task" -o jsonpath='{.status.traceId}')"
  show_trace "$trace" bifrost
  check $? "the Task's trace has its model calls, on the coder's own key"
  pause
}

scenario_multi_agent() {
  scene "3 · Your own multi-agent container, one trace across agents"
  say "Another team built an incident commander in their own container"
  say "(examples/incident-commander). It asks payments-oncall over A2A, then summarises:"
  apply_manifest 03-incident-commander.yaml
  wait_ready incident-commander; check $? "incident-commander is Ready"
  local trace
  trace="$(random_hex 16)"
  expect_mock "v2.31.0"
  ask incident-commander "$trace" "incident-$(random_hex 4)" \
    "Checkout is failing. What is going on?$(directive oncall_get_recent_deploys '{"service": "checkout"}')" \
    --show-findings ${EXPECT[@]+"${EXPECT[@]}"}
  check $? "the commander got the on-call agent's finding over A2A"
  pause
  say "One trace: commander → on-call agent → tool → models, each model call on its agent's key:"
  show_trace "$trace" incident-commander payments-oncall bifrost
  check $? "one trace spans both agents and the LLM gateway"
  pause
}

scenario_lockdown() {
  scene "4 · Lockdown: only the gateways answer"
  say "From inside the on-call agent's pod, try to get around the platform:"
  local pod out
  pod="$(k -n "$NS" get pod -l agentory.dev/agent=payments-oncall -o jsonpath='{.items[0].metadata.name}')"
  show k -n "$NS" get networkpolicy
  out="$(k -n "$NS" exec -i "$pod" -- python - 2>&1 <<'PY' || true
import os, sys, urllib.error, urllib.request
sys.path.insert(0, "/app")
from runtime import MCPClient, RpcError

def reach(url):
    """REACHED: something answered. BLOCKED: the network policy dropped it (a
    timeout). REFUSED: the network let it through but nothing listened, which
    proves nothing about the lockdown, so it never counts as BLOCKED."""
    try:
        urllib.request.urlopen(url, timeout=3)
        return "REACHED"
    except urllib.error.HTTPError:
        return "REACHED"  # any HTTP answer means the network let it through
    except urllib.error.URLError as err:
        return "REFUSED" if isinstance(err.reason, ConnectionRefusedError) else "BLOCKED"
    except (TimeoutError, OSError):
        return "BLOCKED"

rows = [
    ("call a model provider directly (api.anthropic.com)", reach("https://api.anthropic.com")),
    ("call the MCP server directly, skipping the allow-list",
     reach("http://oncall-mcp.mcp-demo.svc.cluster.local:8080/healthz")),
    ("call another agent directly, skipping the gateway",
     reach("http://incident-commander.team-payments.svc.cluster.local:8080/healthz")),
]
try:
    MCPClient(os.environ["AGENTORY_MCP_URL"]).call_tool("oncall_get_pod_events", {"service": "checkout"})
    rows.append(("use a tool off its allow-list (get_pod_events)", "ALLOWED"))
except RpcError:
    rows.append(("use a tool off its allow-list (get_pod_events)", "REJECTED"))
rows.append(("reach the LLM gateway", reach(os.environ["AGENTORY_LLM_BASE_URL"].rsplit("/v1", 1)[0] + "/health")))
for what, result in rows:
    print(f"{result:<9} {what}")
PY
)"
  indent <<<"$out"
  grep -q "^BLOCKED   call a model provider" <<<"$out"; check $? "no direct internet: models only through Bifrost"
  grep -q "^BLOCKED   call the MCP server" <<<"$out"; check $? "MCP servers only through the gateway"
  grep -q "^BLOCKED   call another agent" <<<"$out"; check $? "other agents only through the gateway"
  grep -q "^REJECTED  use a tool" <<<"$out"; check $? "the gateway rejects tools off the allow-list"
  grep -q "^REACHED   reach the LLM gateway" <<<"$out"; check $? "the LLM gateway is reachable"
}

# ------------------------------------------------------------------ main

if [[ "${1:-}" == clean ]]; then clean; exit 0; fi

bold "Agentory demo · model ${MODEL_PROVIDER}/${MODEL_NAME} · namespace ${NS}"
if [[ "${MODEL_PROVIDER}" == mock ]]; then
  say "(mock model: deterministic and needs no API key; it echoes, so answers are terse)"
fi
preflight
scenario_oncall
scenario_coder
scenario_multi_agent
scenario_lockdown

echo
if ((failures)); then
  printf '\033[1;31m  ✗ %d demo check(s) failed\033[0m\n' "$failures"
  exit 1
fi
ok "all demo checks passed"
say "Everything stays running: kubectl --context ${KUBE_CONTEXT} -n ${NS} get agents,tasks"
say "Remove it with: make demo-clean"
if ((INTERACTIVE)); then
  say "Jaeger stays open at ${JAEGER_URL} until you press enter."
  pause
fi
