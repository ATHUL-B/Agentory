"""Tests for the demo helper. Run: python3 -m unittest discover -s demo"""

import io
import json
import unittest

from demo import A2AClient, DemoError, JaegerClient, Trace, main

TRACE = "a" * 32


def span(name, span_id, parent="", start=0, end=1_000_000, attrs=None, error=False):
    s = {"traceId": TRACE, "spanId": span_id, "name": name, "startTimeUnixNano": str(start),
         "endTimeUnixNano": str(end), "attributes": [{"key": k, "value": {"stringValue": v}} for k, v in (attrs or {}).items()]}
    if parent:
        s["parentSpanId"] = parent
    if error:
        s["status"] = {"code": 2}
    return s


def otlp(*services):
    return {"result": {"resourceSpans": [
        {"resource": {"attributes": [{"key": "service.name", "value": {"stringValue": svc}}]},
         "scopeSpans": [{"spans": spans}]} for svc, spans in services]}}


INCIDENT = otlp(
    ("incident-commander", [span("incident-commander message/send", "c1", "laptop", 0, 300_000_000)]),
    ("payments-oncall", [
        span("invoke_agent payments-oncall", "o1", "c1", 10, 200_000_000,
             {"gen_ai.operation.name": "invoke_agent", "gen_ai.usage.input_tokens": "58", "gen_ai.usage.output_tokens": "30"}),
        span("execute_tool oncall_get_recent_deploys", "t1", "o1", 50, 60,
             {"gen_ai.operation.name": "execute_tool"}),
    ]),
    ("bifrost", [
        span("/v1/chat/completions", "b1", "o1", 20, 30),
        span("plugin.governance.prehook", "p1", "b1", 21, 22),
        span("chat mock-model", "m1", "b1", 22, 29, {
            "gen_ai.operation.name": "chat", "bifrost.virtual_key.name": "agentory:team-payments/payments-oncall",
            "gen_ai.usage.input_tokens": "19", "gen_ai.usage.output_tokens": "5",
            "gen_ai.response.finish_reason": "tool_calls"}),
        span("/v1/chat/completions", "b2", "c1", 250_000_000, 260_000_000),
    ]),
)


class FakeFetch:
    def __init__(self, replies):
        self.replies = list(replies)
        self.calls = []

    def __call__(self, url, data, headers, timeout):
        self.calls.append({"url": url, "data": json.loads(data) if data else None, "headers": headers})
        return self.replies.pop(0) if len(self.replies) > 1 else self.replies[0]


def a2a_ok(text, metadata=None):
    result = {"kind": "message", "role": "agent", "contextId": "ctx-1", "parts": [{"kind": "text", "text": text}]}
    if metadata:
        result["metadata"] = metadata
    return 200, json.dumps({"jsonrpc": "2.0", "id": 1, "result": result}).encode()


class TraceTest(unittest.TestCase):
    def test_renders_agents_tools_and_model_calls_as_a_tree(self):
        out = Trace.from_otlp(INCIDENT).render()
        self.assertEqual(out.splitlines(), [
            "⇢ incident-commander   incident-commander message/send  300ms",
            "  ├─ payments-oncall      invoke_agent payments-oncall  199ms  [58→30 tokens total]",
            "  │  ├─ bifrost              /v1/chat/completions  0ms",
            "  │  │  └─ bifrost              chat mock-model  0ms"
            "  [key agentory:team-payments/payments-oncall · 19→5 tokens · tool_calls]",
            "  │  └─ payments-oncall      execute_tool oncall_get_recent_deploys  0ms",
            "  └─ bifrost              /v1/chat/completions  10ms",
            "(+1 gateway plugin spans hidden)",
            "(⇢ parent span not exported: the caller's, or the agent gateway's in between "
            "(backlog 6d); same trace, separate branch)",
        ])

    def test_true_root_has_no_gateway_marker(self):
        out = Trace.from_otlp(otlp(("a", [span("root", "1"), span("child", "2", "1")]))).render()
        self.assertEqual(out.splitlines(), ["a                    root  1ms", "└─ a                    child  1ms"])

    def test_services_and_errors(self):
        trace = Trace.from_otlp(otlp(("a", [span("x", "1", error=True)])))
        self.assertEqual(trace.services, ["a"])
        self.assertIn("ERROR", trace.render())

    def test_wait_for_polls_until_every_service_reported(self):
        partial = otlp(("payments-oncall", [span("invoke_agent payments-oncall", "o1")]))
        fetch = FakeFetch([(404, b""), (200, json.dumps(partial).encode()), (200, json.dumps(INCIDENT).encode())])
        trace = JaegerClient("http://jaeger/", fetch, sleep=lambda s: None).wait_for(TRACE, ["payments-oncall", "bifrost"])
        self.assertIn("bifrost", trace.services)
        self.assertEqual(fetch.calls[0]["url"], f"http://jaeger/api/v3/traces/{TRACE}")
        self.assertEqual(len(fetch.calls), 3)

    def test_wait_for_reports_unknown_trace(self):
        with self.assertRaisesRegex(DemoError, "not found in Jaeger"):
            JaegerClient("http://jaeger", FakeFetch([(404, b"")]), sleep=lambda s: None).wait_for(TRACE, [], 2)

    def test_wait_for_reports_missing_services(self):
        fetch = FakeFetch([(200, json.dumps(otlp(("bifrost", [span("x", "1")]))).encode())])
        with self.assertRaisesRegex(DemoError, "no spans from payments-oncall"):
            JaegerClient("http://jaeger", fetch, sleep=lambda s: None).wait_for(TRACE, ["payments-oncall"], 3)


class A2AClientTest(unittest.TestCase):
    def test_sends_message_with_context_and_trace(self):
        fetch = FakeFetch([a2a_ok("v2.31.0 12 minutes ago")])
        answer = A2AClient("http://gw:18080/", fetch).ask("team-payments/payments-oncall", "what changed?", "ctx-1", TRACE)
        self.assertEqual((answer.text, answer.context_id), ("v2.31.0 12 minutes ago", "ctx-1"))
        call = fetch.calls[0]
        self.assertEqual(call["url"], "http://gw:18080/agents/team-payments/payments-oncall/")
        self.assertEqual(call["data"]["params"]["message"]["contextId"], "ctx-1")
        self.assertRegex(call["headers"]["traceparent"], rf"^00-{TRACE}-[0-9a-f]{{16}}-01$")

    def test_retries_while_the_route_warms_up(self):
        fetch = FakeFetch([(503, b"no healthy upstream"), (0, b"refused"), a2a_ok("ok")])
        self.assertEqual(A2AClient("http://gw", fetch, sleep=lambda s: None).ask("ns/a", "q").text, "ok")
        self.assertEqual(len(fetch.calls), 3)

    def test_errors(self):
        with self.assertRaises(DemoError):
            A2AClient("http://gw").ask("no-namespace", "q")
        fetch = FakeFetch([(404, b"route not found")])
        with self.assertRaisesRegex(DemoError, "HTTP 404"):
            A2AClient("http://gw", fetch).ask("ns/a", "q")
        fetch = FakeFetch([(200, json.dumps({"jsonrpc": "2.0", "id": 1, "error": {"code": -1, "message": "boom"}}).encode())])
        with self.assertRaisesRegex(DemoError, "boom"):
            A2AClient("http://gw", fetch).ask("ns/a", "q")


class CLITest(unittest.TestCase):
    def run_ask(self, reply, *extra):
        out = io.StringIO()
        code = main(["ask", "--gateway", "http://gw", "--agent", "ns/a", "--text", "q", *extra],
                    out=out, fetch=FakeFetch([reply]), sleep=lambda s: None)
        return code, out.getvalue()

    def test_trace_prints_the_tree(self):
        out = io.StringIO()
        code = main(["trace", "--jaeger", "http://j", "--id", TRACE, "--expect-service", "bifrost"],
                    out=out, fetch=FakeFetch([(200, json.dumps(INCIDENT).encode())]), sleep=lambda s: None)
        self.assertEqual(code, 0)
        self.assertIn("execute_tool oncall_get_recent_deploys", out.getvalue())

    def test_ask_prints_findings_and_answer(self):
        code, out = self.run_ask(a2a_ok("Roll back v2.31.0", {"findings": {"payments-oncall": "deploy 12m ago"}}),
                                 "--show-findings", "--expect", "v2.31.0")
        self.assertEqual(code, 0)
        self.assertEqual(out, "  payments-oncall found: deploy 12m ago\nRoll back v2.31.0\n")

    def test_expectation_can_match_a_finding(self):
        code, _ = self.run_ask(a2a_ok("summary", {"findings": {"payments-oncall": "deploy v2.31.0"}}), "--expect", "v2.31.0")
        self.assertEqual(code, 0)

    def test_ask_fails_when_expectation_missing(self):
        code, _ = self.run_ask(a2a_ok("something else"), "--expect", "v2.31.0")
        self.assertEqual(code, 1)


if __name__ == "__main__":
    unittest.main()
