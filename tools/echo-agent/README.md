# Echo agent

The reference **bring-your-own-container** A2A agent, and the agent the smoke
tests talk to. Standard library only.

It uses nothing but what the operator injects into every container Agent:

| Variable | Used for |
|---|---|
| `AGENTORY_AGENT_CARD_PATH` | The generated Agent Card, served at `/.well-known/agent-card.json` (and `agent.json`) |
| `AGENTORY_LLM_BASE_URL`, `AGENTORY_LLM_API_KEY`, `AGENTORY_MODEL` | Answering through the LLM gateway with the agent's own virtual key |

It answers A2A JSON-RPC `message/send` by sending the user's text to the model
and returning the reply as an agent message. Other methods return
`-32601 method not found`.

```yaml
apiVersion: agentory.dev/v1alpha1
kind: Agent
metadata: { name: echo }
spec:
  runtime: container
  lifecycle: service
  container: { image: agentory-echo-agent:dev, port: 8080 }
  model: { provider: mock, name: mock-model }
```

Call it from another agent through the gateway:
`POST $AGENTORY_A2A_URL/agents/<namespace>/echo/` with a `message/send` request.

```bash
make test                       # unit + HTTP tests (with the repo's other tests)
make operator-up                # builds agentory-echo-agent:dev and loads it into kind
```
