# Agentory demo

One incident, four scenarios, about ten minutes. Checkout is failing for the
payments team, right after deploy v2.31.0.

| # | Scenario | What it shows |
|---|---|---|
| 1 | **A no-code on-call agent** | Instructions + a runbook skill + two MCP tools, in YAML. The operator creates the pod, a virtual LLM key, the network lockdown, the A2A route and an MCP route with an allow-list. The agent finds the deploy and the error rate, and the run is **one trace** with every tool call and model call, each on the agent's own key. |
| 2 | **A coding agent, one pod per Task** | A `Task` runs Claude Code in a fresh pod, which writes `ROLLBACK.md`. The Task records phase, changed files, tokens, cost and its trace. |
| 3 | **Your own multi-agent container** | [`incident-commander`](../examples/incident-commander) asks `payments-oncall` over A2A, then summarises with its own model. Both agents and all model calls are in **the same trace**. |
| 4 | **Lockdown** | From inside an agent: model providers, MCP servers and other agents can't be reached directly, a tool off the allow-list is rejected, and the gateways work. |

## Run it

```bash
make up            # cluster, gateways, tracing, mock LLM, demo MCP server
make operator-up   # operator + agent images (incl. the incident commander)
make demo          # pauses at each step when run in a terminal
make demo-clean    # remove the demo namespace and MCPServer
```

- `DEMO_AUTO=1 make demo` runs without pauses, for CI or a recording.
- Every step is **checked**. The script ends with `all demo checks passed`,
  or lists what failed and exits non-zero. It's safe to re-run.
- It port-forwards the agent gateway to `localhost:18080` and Jaeger to
  `localhost:16686` (`DEMO_GATEWAY_PORT`, `DEMO_JAEGER_PORT`) while it runs. In
  a terminal it keeps them open at the end, so you can open the trace links in
  Jaeger.
- Everything stays deployed afterwards: `kubectl --context kind-agentory -n team-payments get agents,tasks`.

### With a real model

By default every agent uses the in-cluster **mock model**: no API key, fully
deterministic. It follows `[[tool:NAME {json}]]` directives, which the script
appends to each question, and it echoes, so its answers are terse. To use a
real model, give its key to `make up` (which registers the provider in
Bifrost), then:

```bash
DEMO_PROVIDER=anthropic DEMO_MODEL=<model id> make demo
```

The questions are then sent without directives, and the model picks its own
tools. Answer content is no longer checked, only that each step works. This
mode hasn't been run against a real provider yet.

## Files

| File | What it is |
|---|---|
| [`manifests/00-tool-registry.yaml`](manifests/00-tool-registry.yaml) | The `MCPServer` the platform team publishes once |
| [`manifests/01-oncall-agent.yaml`](manifests/01-oncall-agent.yaml) | Skill ConfigMap + declarative `Agent`: all the payments team writes |
| [`manifests/02-coding-agent.yaml`](manifests/02-coding-agent.yaml) | Claude Code harness `Agent` (the `Task` is created by the script) |
| [`manifests/03-incident-commander.yaml`](manifests/03-incident-commander.yaml) | The BYO multi-agent container as an `Agent` |
| [`run.sh`](run.sh) | The narrated script |
| [`demo.py`](demo.py) | Laptop-side helper: A2A client, and a trace renderer that turns a Jaeger trace into a tree of agents, tool calls and model calls |

The manifests use `${MODEL_PROVIDER}`, `${MODEL_NAME}` and
`${INCIDENT_COMMANDER_IMAGE}`, which the script substitutes.

## Talk track

1. **"Nobody on this team writes code."** Show `01-oncall-agent.yaml`: plain
   instructions, a runbook, and two tools by name. After `apply`, show the
   objects the operator created. Point out the virtual key (budget and cost
   attribution per agent) and the MCP policy (the allow-list).
2. **Ask it.** It calls `get_recent_deploys`, then `get_error_rate`, in the
   same conversation. In the trace, each `chat` span names the agent's key and
   its tokens.
3. **"Now fix it."** The Task's pod exists only while it runs.
   `kubectl get tasks -o wide` shows the trace ID, and the Task status has the
   files, tokens and cost.
4. **"Bring your own agents."** The commander is a plain Python container that
   only uses what the operator injects. The specialist's finding comes back
   over A2A, and the trace covers both agents.
5. **"And it can't go around us."** The lockdown table: direct calls are
   `BLOCKED` (dropped by the network policy), the off-list tool is `REJECTED`
   by the gateway, and only the gateways are `REACHED`.

## Known gaps

- The agent gateway doesn't export spans yet (backlog **6d**). Calls through
  it keep the same trace ID, but each called agent starts its own branch,
  marked `⇢` in the tree, instead of nesting under its caller.
- The harness Task's trace has only its model calls (also marked `⇢`):
  neither the harness runner nor Claude Code exports spans of its own here.
