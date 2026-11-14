# Claude Code harness

Runs **one Agentory Task** with Claude Code, then exits. The operator's
JobBackend starts it as a Kubernetes Job for every `Task` of a
`runtime: harness`, `kind: claude-code` Agent.

Image: `node:24.22.1-slim` + Claude Code `2.1.311` (npm `stable`) + git +
python3, running as UID 1000. Built and loaded into kind by `make operator-up`
as `agentory-harness-claude-code:dev`.

## What a run does

1. Prepares `/workspace` as a git tree (empty for now; cloning a repository is
   backlog 7a), so changed files can be listed afterwards.
2. Creates a W3C trace and sends `traceparent` to the LLM gateway through
   `ANTHROPIC_CUSTOM_HEADERS`, so the gateway's spans belong to the Task.
3. Runs Claude Code headless:
   `claude --bare -p "<task message>" --output-format json --model <agent model>
   --permission-mode acceptEdits --allowedTools Read,Edit,Write,Bash --permission-prompts none`.
   `ANTHROPIC_BASE_URL` and `ANTHROPIC_API_KEY` are the gateway and the
   **agent's own virtual key**. Every model alias (haiku, sonnet, opus) is pinned
   to the agent's model, the only one that key allows. Non-essential traffic is
   off, since egress is locked to the gateways anyway.
4. Writes a compact JSON result (status, summary, traceId, usage, cost, turns,
   changed files) to the container's **termination message**, trimmed to fit
   Kubernetes' 4 KB limit. The operator reads it from the pod status, so the pod
   needs no API access.

Exit code 0 means succeeded and non-zero means failed. Either way the result
is written.

## Settings (env)

| Variable | Default | |
|---|---|---|
| `AGENTORY_TASK_MESSAGE` | required | The prompt |
| `AGENTORY_MODEL` | required | `provider/model` |
| `AGENTORY_HARNESS_ALLOWED_TOOLS` | `Read,Edit,Write,Bash` | Pre-approved tools |
| `AGENTORY_HARNESS_MAX_TURNS` | unset | Cap on agent turns |

## Test

```bash
make test        # includes these runner tests (fake claude, real git)
```
