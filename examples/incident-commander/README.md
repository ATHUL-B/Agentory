# Incident commander (example multi-agent container)

A "bring your own container" agent that coordinates other agents. On an A2A
`message/send` it:

1. asks each agent in `SPECIALISTS` the same question over A2A, through the
   agent gateway;
2. asks its own model, through the LLM gateway with its own virtual key, to
   turn their findings into an incident summary;
3. replies with the summary. `metadata.findings` holds each specialist's
   answer and `metadata.traceId` the trace.

It carries W3C `traceparent` on every hop and exports its own span, so the
caller, this agent, each specialist and every model call share one trace. A
specialist that fails is reported as unavailable in the findings instead of
failing the whole request.

It only uses what the operator injects (`AGENTORY_A2A_URL`,
`AGENTORY_NAMESPACE`, `AGENTORY_LLM_*`, `AGENTORY_MODEL`,
`AGENTORY_AGENT_CARD_PATH`, `OTEL_*`), plus one variable of its own:

| Variable | Meaning |
|---|---|
| `SPECIALISTS` | Comma-separated agents to ask: `name` (same namespace) or `namespace/name` |

```yaml
apiVersion: agentory.dev/v1alpha1
kind: Agent
metadata: { name: incident-commander, namespace: team-payments }
spec:
  runtime: container
  lifecycle: service
  container:
    image: agentory-incident-commander:dev
    port: 8080
    env: [{ name: SPECIALISTS, value: payments-oncall }]
  model: { provider: mock, name: mock-model }
```

It uses the standard library only, in one file, so it can be copied as a
starting point. `make operator-up` builds the image and loads it into kind,
`make test` runs the tests, and `make demo` uses it in scenario 3.
