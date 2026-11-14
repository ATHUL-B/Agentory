# Mock LLM

A deterministic, OpenAI-compatible LLM server so the whole data plane
(agent → Bifrost → provider) works locally and in CI **without an API key**.
In the local cluster it is registered in Bifrost as provider `mock`; call it as
model **`mock/mock-model`**.

Standard library only (`mock_llm.py`), so it runs from a stock `python` image
with the code mounted from a ConfigMap. Nothing needs building.

## Behaviour

| Request | Response |
|---|---|
| Plain chat | `mock reply: <last user message>` (truncated to 200 chars), `finish_reason: stop` |
| `tools` offered, no tool called yet | A call to the **first** tool with `{}` arguments, `finish_reason: tool_calls` |
| Last message is a tool result | `mock reply: tool <name> returned: <result>` |
| User message contains `[[error:503]]` | HTTP 503 (any 3-digit status), for testing retries and fallbacks |
| `stream: true` | Server-sent events: one content/tool chunk, a finish chunk with `usage`, `[DONE]` |

Token usage is a word count (prompt = all message words, completion = reply
words), which is enough to test cost and budget plumbing.

Routes: `POST …/chat/completions`, `GET …/models`, `GET /healthz`, under any
path prefix, so it works whether a client's base URL ends in `/v1` or not.

## Run and test

```bash
make test                                          # unit + HTTP tests
python3 tools/mock-llm/mock_llm.py                 # serve on :8080 (PORT to change)
curl -s localhost:8080/v1/chat/completions -d '{"messages":[{"role":"user","content":"hi"}]}'
```

Through Bifrost in the local cluster:

```bash
kubectl --context kind-agentory -n bifrost port-forward svc/bifrost 8080 &
curl -s localhost:8080/v1/chat/completions \
  -H "x-bf-vk: $(cat deploy/local/.secrets/dev-virtual-key)" -H 'Content-Type: application/json' \
  -d '{"model":"mock/mock-model","messages":[{"role":"user","content":"hi"}]}'
```
