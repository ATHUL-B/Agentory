# Declarative runtime

Runs `runtime: declarative` Agents: an agent made of **instructions and
skills**, with no code from its author. The operator's DeploymentBackend runs
this image (`agentory-declarative-runtime:dev`, built by `make operator-up`)
with everything mounted read-only. Standard library only.

## What it does

- Serves A2A JSON-RPC `message/send` and the generated Agent Card.
- System prompt = the Agent's `instructions`, then each skill (`SKILL.md` body,
  with `name`/`description` from its frontmatter).
- Keeps conversation history per `contextId` (bounded: 40 messages, 1000 contexts,
  least recently used first out). History is in-memory, so it's per pod (8c).
- Calls the model through the LLM gateway (`AGENTORY_LLM_BASE_URL`) with the
  agent's own virtual key, so only the agent's model is allowed.
- **Tracing**: each request is an `invoke_agent` span (OTel GenAI attributes:
  agent name, model, conversation id, token usage) exported as OTLP/HTTP JSON to
  the collector. It **continues the caller's `traceparent`** and passes its own
  span on to the gateway, so one trace covers caller → agent → model call.
- Tools: `ToolProvider` runs a bounded tool loop. No tools are wired yet; MCP
  through the agent gateway is backlog 6b.

## Inputs (from the operator)

| | |
|---|---|
| `AGENTORY_RUNTIME_CONFIG` | `/etc/agentory/runtime/config.json`: `{"instructions": ..., "skills": [{"name", "path"}]}` |
| `/etc/agentory/skills/<name>/SKILL.md` | One read-only mount per skill (from a ConfigMap) |
| `AGENTORY_AGENT_CARD_PATH` | The generated card |
| `AGENTORY_LLM_BASE_URL`, `AGENTORY_LLM_API_KEY`, `AGENTORY_MODEL` | Gateway, key, model |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` | Tracing (`OTEL_SDK_DISABLED=true` turns it off) |

## Test

```bash
make test     # includes these 20 tests
```
