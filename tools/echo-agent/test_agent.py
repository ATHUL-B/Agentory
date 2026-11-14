"""Tests for the echo agent. Run: python3 -m unittest discover -s tools/echo-agent"""

import json
import os
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from agent import (
    INVALID_PARAMS,
    INVALID_REQUEST,
    METHOD_NOT_FOUND,
    AgentCard,
    EchoAgent,
    LLMClient,
    RpcError,
    build_server,
)


def send(text, request_id=1, context_id=None):
    message = {"role": "user", "messageId": "m1", "parts": [{"kind": "text", "text": text}]}
    if context_id:
        message["contextId"] = context_id
    return {"jsonrpc": "2.0", "id": request_id, "method": "message/send", "params": {"message": message}}


class EchoAgentTest(unittest.TestCase):
    def setUp(self):
        self.prompts = []

        def llm(text):
            self.prompts.append(text)
            return "reply to " + text

        ids = iter(["id-1", "id-2", "id-3"])
        self.agent = EchoAgent(llm, id_factory=lambda: next(ids))

    def test_message_send_returns_agent_message(self):
        resp = self.agent.handle(send("hello", context_id="ctx-9"))
        self.assertEqual(resp["id"], 1)
        result = resp["result"]
        self.assertEqual(result["kind"], "message")
        self.assertEqual(result["role"], "agent")
        self.assertEqual(result["parts"], [{"kind": "text", "text": "reply to hello"}])
        self.assertEqual(result["contextId"], "ctx-9")
        self.assertEqual(self.prompts, ["hello"])

    def test_new_context_when_none_given(self):
        result = self.agent.handle(send("hi"))["result"]
        self.assertEqual(result["messageId"], "id-1")
        self.assertEqual(result["contextId"], "id-2")

    def test_errors(self):
        cases = [
            ({"jsonrpc": "1.0", "id": 1, "method": "message/send"}, INVALID_REQUEST),
            ({"jsonrpc": "2.0", "id": 1, "method": "tasks/get", "params": {}}, METHOD_NOT_FOUND),
            ({"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": {}}, INVALID_PARAMS),
            (send("   "), INVALID_PARAMS),
            ("not an object", INVALID_REQUEST),
        ]
        for request, code in cases:
            with self.subTest(request=request):
                self.assertEqual(self.agent.handle(request)["error"]["code"], code)
        self.assertEqual(self.prompts, [], "invalid requests must not reach the model")


class AgentCardTest(unittest.TestCase):
    def test_loads_generated_card(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump({"name": "payments-oncall", "url": "http://gw/agents/a/b/"}, f)
        self.addCleanup(os.remove, f.name)
        self.assertEqual(AgentCard.load(f.name, "x")["name"], "payments-oncall")

    def test_fallback_card(self):
        card = AgentCard.load("/does/not/exist", "fallback")
        self.assertEqual(card["name"], "fallback")
        self.assertEqual(card["protocolVersion"], "0.3.0")


class FakeGateway(BaseHTTPRequestHandler):
    """Stands in for Bifrost's OpenAI-compatible API."""
    seen = {}

    def do_POST(self):  # noqa: N802
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        FakeGateway.seen = {"auth": self.headers.get("Authorization"), "model": body["model"], "path": self.path}
        if body["model"] == "blocked":
            self.send_response(403)
            self.end_headers()
            return
        data = json.dumps({"choices": [{"message": {"content": "gateway says hi"}}]}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


def serve(server):
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return f"http://127.0.0.1:{server.server_address[1]}"


class LLMClientTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.gateway = ThreadingHTTPServer(("127.0.0.1", 0), FakeGateway)
        cls.url = serve(cls.gateway) + "/v1"

    @classmethod
    def tearDownClass(cls):
        cls.gateway.shutdown()
        cls.gateway.server_close()

    def test_sends_virtual_key_and_model(self):
        reply = LLMClient(self.url, "sk-bf-test", "mock/mock-model").complete("hi")
        self.assertEqual(reply, "gateway says hi")
        self.assertEqual(FakeGateway.seen, {"auth": "Bearer sk-bf-test", "model": "mock/mock-model",
                                            "path": "/v1/chat/completions"})

    def test_gateway_errors_become_rpc_errors(self):
        with self.assertRaises(RpcError):
            LLMClient(self.url, "k", "blocked").complete("hi")
        with self.assertRaises(RpcError):
            LLMClient("http://127.0.0.1:9/v1", "k", "m", timeout=1).complete("hi")


class HttpServerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        agent = EchoAgent(lambda text: "echo: " + text)
        cls.server = build_server(agent, {"name": "card-under-test"}, host="127.0.0.1", port=0)
        cls.base = serve(cls.server)

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def fetch(self, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method="POST" if data else "GET",
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                return resp.status, json.load(resp)
        except urllib.error.HTTPError as err:
            return err.code, None

    def test_serves_card_at_both_well_known_paths(self):
        for path in ("/.well-known/agent-card.json", "/.well-known/agent.json"):
            with self.subTest(path=path):
                self.assertEqual(self.fetch(path), (200, {"name": "card-under-test"}))

    def test_message_send_over_http(self):
        status, body = self.fetch("/", send("ping"))
        self.assertEqual(status, 200)
        self.assertEqual(body["result"]["parts"][0]["text"], "echo: ping")

    def test_unknown_paths(self):
        self.assertEqual(self.fetch("/nope")[0], 404)
        self.assertEqual(self.fetch("/nope", send("x"))[0], 404)


if __name__ == "__main__":
    unittest.main()
