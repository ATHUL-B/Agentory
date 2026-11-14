"""Tests for the incident commander example. Run: python3 -m unittest discover -s examples/incident-commander"""

import json
import threading
import unittest
import urllib.request

from agent import (
    INTERNAL_ERROR,
    INVALID_PARAMS,
    INVALID_REQUEST,
    METHOD_NOT_FOUND,
    A2AClient,
    IncidentCommander,
    LLMClient,
    RpcError,
    SpanContext,
    Tracer,
    build_server,
    from_env,
    load_card,
)

PARENT = "00-" + "a" * 32 + "-" + "b" * 16 + "-01"


class FakePoster:
    """Records POSTs and answers from a per-URL-fragment script of (status, body)."""

    def __init__(self, script):
        self.script = {k: list(v) for k, v in script.items()}
        self.calls = []

    def __call__(self, url, body, headers, timeout):
        self.calls.append({"url": url, "body": body, "headers": headers})
        for fragment, replies in self.script.items():
            if fragment in url:
                return replies.pop(0) if len(replies) > 1 else replies[0]
        raise AssertionError("unexpected POST " + url)


def a2a_reply(text):
    return 200, {"jsonrpc": "2.0", "id": "1", "result": {"kind": "message", "role": "agent",
                                                         "parts": [{"kind": "text", "text": text}]}}


def chat_reply(text):
    return 200, {"choices": [{"message": {"role": "assistant", "content": text}}]}


def send(text, req_id=1):
    return {"jsonrpc": "2.0", "id": req_id, "method": "message/send",
            "params": {"message": {"role": "user", "messageId": "m1", "parts": [{"kind": "text", "text": text}]}}}


class RecordingTracer(Tracer):
    def __init__(self):
        self.sent = []
        super().__init__("http://collector:4318", "incident-commander", send=lambda url, data: self.sent.append(
            (url, json.loads(data))), clock=lambda: 1000)

    def span(self):
        return self.sent[-1][1]["resourceSpans"][0]["scopeSpans"][0]["spans"][0]


def commander(poster, tracer=None, specialists=("payments-oncall",)):
    return IncidentCommander(
        list(specialists),
        A2AClient("http://gw:8080", "team-payments", post=poster, sleep=lambda s: None),
        LLMClient("http://bifrost:8080/v1", "vk-123", "mock/mock-model", post=poster),
        tracer or RecordingTracer(), id_factory=lambda: "id-1")


class IncidentCommanderTest(unittest.TestCase):
    def test_delegates_then_summarises_in_one_trace(self):
        poster = FakePoster({"/agents/": [a2a_reply("deploy v2.31.0 12 minutes ago")],
                             "/chat/completions": [chat_reply("Cause: v2.31.0. Next: roll back.")]})
        tracer = RecordingTracer()
        resp = commander(poster, tracer).handle(send("Why is checkout failing?"), PARENT)

        result = resp["result"]
        self.assertEqual(result["parts"][0]["text"], "Cause: v2.31.0. Next: roll back.")
        self.assertEqual(result["metadata"]["traceId"], "a" * 32, "continues the caller's trace")
        self.assertEqual(result["metadata"]["findings"], {"payments-oncall": "deploy v2.31.0 12 minutes ago"})

        a2a_call, llm_call = poster.calls
        self.assertEqual(a2a_call["url"], "http://gw:8080/agents/team-payments/payments-oncall/")
        self.assertEqual(a2a_call["body"]["params"]["message"]["parts"][0]["text"], "Why is checkout failing?")
        self.assertIn("Findings from payments-oncall: deploy v2.31.0", llm_call["body"]["messages"][0]["content"])
        self.assertEqual(llm_call["headers"]["Authorization"], "Bearer vk-123")

        span = tracer.span()
        self.assertEqual((span["traceId"], span["parentSpanId"]), ("a" * 32, "b" * 16))
        own = span["spanId"]
        for call in poster.calls:
            self.assertEqual(call["headers"]["traceparent"], f"00-{'a' * 32}-{own}-01", "children of our span")

    def test_new_trace_without_caller_context(self):
        poster = FakePoster({"/agents/": [a2a_reply("x")], "/chat/completions": [chat_reply("y")]})
        tracer = RecordingTracer()
        trace = commander(poster, tracer).handle(send("q"))["result"]["metadata"]["traceId"]
        self.assertRegex(trace, r"^[0-9a-f]{32}$")
        self.assertNotIn("parentSpanId", tracer.span())

    def test_unavailable_specialist_is_reported_not_fatal(self):
        poster = FakePoster({"/agents/team-payments/ledger": [(500, None)],
                             "/agents/": [a2a_reply("deploy v2.31.0")],
                             "/chat/completions": [chat_reply("summary")]})
        resp = commander(poster, specialists=("payments-oncall", "ledger")).handle(send("q"))
        self.assertIn("unavailable", resp["result"]["metadata"]["findings"]["ledger"])
        self.assertEqual(resp["result"]["parts"][0]["text"], "summary")

    def test_cross_namespace_specialist(self):
        self.assertEqual(A2AClient("http://gw:8080/", "team-payments").url("platform/db-oncall"),
                         "http://gw:8080/agents/platform/db-oncall/")

    def test_retries_while_route_warms_up(self):
        poster = FakePoster({"/agents/": [(503, None), (503, None), a2a_reply("ok")]})
        a2a = A2AClient("http://gw:8080", "ns", post=poster, sleep=lambda s: None)
        self.assertEqual(a2a.ask("a", "q", PARENT), "ok")
        self.assertEqual(len(poster.calls), 3)

    def test_agent_error_is_raised(self):
        poster = FakePoster({"/agents/": [(200, {"jsonrpc": "2.0", "id": "1", "error": {"code": -1, "message": "boom"}})]})
        with self.assertRaises(RpcError):
            A2AClient("http://gw:8080", "ns", post=poster).ask("a", "q", PARENT)

    def test_llm_failure_fails_the_request_and_marks_the_span(self):
        poster = FakePoster({"/agents/": [a2a_reply("x")], "/chat/completions": [(429, None)]})
        tracer = RecordingTracer()
        resp = commander(poster, tracer).handle(send("q"))
        self.assertEqual(resp["error"]["code"], INTERNAL_ERROR)
        self.assertEqual(tracer.span()["status"]["code"], 2)

    def test_protocol_errors(self):
        c = commander(FakePoster({}))
        self.assertEqual(c.handle({"id": 1})["error"]["code"], INVALID_REQUEST)
        self.assertEqual(c.handle({"jsonrpc": "2.0", "id": 1, "method": "tasks/get"})["error"]["code"], METHOD_NOT_FOUND)
        self.assertEqual(c.handle(send("  "))["error"]["code"], INVALID_PARAMS)
        self.assertEqual(c.handle({"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": {}})["error"]["code"],
                         INVALID_PARAMS)


class ConfigTest(unittest.TestCase):
    def test_from_env(self):
        c = from_env({"AGENTORY_AGENT": "commander", "SPECIALISTS": "payments-oncall, ledger ,",
                      "AGENTORY_A2A_URL": "http://gw:8080", "AGENTORY_NAMESPACE": "team-payments",
                      "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"})
        self.assertEqual((c.name, c.specialists), ("commander", ["payments-oncall", "ledger"]))
        self.assertEqual(c.tracer.endpoint, "http://collector:4318/v1/traces")
        off = from_env({"SPECIALISTS": "a", "OTEL_SDK_DISABLED": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://c"})
        self.assertEqual(off.tracer.endpoint, "")
        with self.assertRaises(SystemExit):
            from_env({})

    def test_traceparent_parsing(self):
        self.assertEqual(SpanContext.parse(PARENT).span_id, "b" * 16)
        for bad in (None, "", "garbage", "00-" + "0" * 32 + "-" + "b" * 16 + "-01"):
            self.assertIsNone(SpanContext.parse(bad))

    def test_fallback_card(self):
        self.assertEqual(load_card("/nonexistent", "commander")["name"], "commander")


class HttpTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        poster = FakePoster({"/agents/": [a2a_reply("deploy v2.31.0")], "/chat/completions": [chat_reply("summary")]})
        cls.tracer = RecordingTracer()
        cls.server = build_server(commander(poster, cls.tracer), load_card(None, "incident-commander"), "127.0.0.1", 0)
        cls.url = f"http://127.0.0.1:{cls.server.server_address[1]}"
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def test_message_send_with_traceparent_header(self):
        req = urllib.request.Request(self.url + "/", data=json.dumps(send("q")).encode(), method="POST",
                                     headers={"Content-Type": "application/json", "traceparent": PARENT})
        with urllib.request.urlopen(req, timeout=5) as r:
            body = json.load(r)
        self.assertEqual(body["result"]["parts"][0]["text"], "summary")
        self.assertEqual(body["result"]["metadata"]["traceId"], "a" * 32)

    def test_card_and_health(self):
        with urllib.request.urlopen(self.url + "/.well-known/agent-card.json", timeout=5) as r:
            self.assertEqual(json.load(r)["name"], "incident-commander")
        with urllib.request.urlopen(self.url + "/healthz", timeout=5) as r:
            self.assertEqual(r.status, 200)


if __name__ == "__main__":
    unittest.main()
