"""Tests for the declarative runtime. Run: python3 -m unittest discover -s runtime/declarative"""

import json
import os
import tempfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import threading
import unittest
import urllib.request

from runtime import (
    INTERNAL_ERROR,
    INVALID_PARAMS,
    METHOD_NOT_FOUND,
    ChatClient,
    MCPClient,
    MCPToolProvider,
    ConversationStore,
    DeclarativeAgent,
    PromptBuilder,
    RpcError,
    RuntimeConfig,
    Skill,
    SkillLoader,
    ToolProvider,
    Tracer,
    build_server,
    load_card,
)

PARENT = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"


def send(text, context_id=None, request_id=1):
    msg = {"role": "user", "messageId": "m1", "parts": [{"kind": "text", "text": text}]}
    if context_id:
        msg["contextId"] = context_id
    return {"jsonrpc": "2.0", "id": request_id, "method": "message/send", "params": {"message": msg}}


class RecordingTracer(Tracer):
    def __init__(self):
        self.payloads = []
        super().__init__("http://collector:4318", "oncall", {"agentory.agent": "oncall"},
                         send=lambda url, data: self.payloads.append((url, json.loads(data))))

    def spans(self):
        return [s for _, p in self.payloads for rs in p["resourceSpans"] for ss in rs["scopeSpans"] for s in ss["spans"]]


class FakeChat(ChatClient):
    def __init__(self, replies=None):
        super().__init__("http://gw/v1", "sk-bf-key", "mock/mock-model")
        self.calls = []
        self.replies = list(replies or [])

    def complete(self, messages, traceparent, tools=None):
        self.calls.append({"messages": [dict(m) for m in messages], "traceparent": traceparent, "tools": tools})
        if self.replies:
            return self.replies.pop(0)
        return {"role": "assistant", "content": "answer to " + messages[-1]["content"]}, \
            {"prompt_tokens": 10, "completion_tokens": 3}


def agent_with(chat=None, tracer=None, skills=None, tools=None):
    cfg = RuntimeConfig(instructions="You are the payments on-call agent.", skills=skills or [])
    ids = iter(f"id-{i}" for i in range(100))
    return DeclarativeAgent(cfg, chat or FakeChat(), tracer or RecordingTracer(), tools=tools,
                            agent_name="oncall", id_factory=lambda: next(ids))


class SkillTest(unittest.TestCase):
    def write_skill(self, d, text):
        os.makedirs(d, exist_ok=True)
        with open(os.path.join(d, "SKILL.md"), "w") as f:
            f.write(text)

    def test_frontmatter_and_body(self):
        with tempfile.TemporaryDirectory() as d:
            self.write_skill(d, "---\nname: k8s-triage\ndescription: \"Triage pods\"\nlicense: MIT\n---\n# Steps\n1. kubectl get pods\n")
            skill = SkillLoader().load("fallback", d)
        self.assertEqual((skill.name, skill.description), ("k8s-triage", "Triage pods"))
        self.assertEqual(skill.body, "# Steps\n1. kubectl get pods")

    def test_without_frontmatter_uses_given_name(self):
        with tempfile.TemporaryDirectory() as d:
            self.write_skill(d, "Just do the thing.")
            skill = SkillLoader().load("plain", d)
        self.assertEqual((skill.name, skill.description, skill.body), ("plain", "", "Just do the thing."))

    def test_config_load(self):
        with tempfile.TemporaryDirectory() as d:
            skill_dir = os.path.join(d, "skills", "triage")
            self.write_skill(skill_dir, "---\nname: triage\n---\nbody")
            path = os.path.join(d, "config.json")
            with open(path, "w") as f:
                json.dump({"instructions": "Be helpful.", "skills": [{"name": "triage", "path": skill_dir}],
                           "maxHistoryMessages": 6}, f)
            cfg = RuntimeConfig.load(path, SkillLoader())
        self.assertEqual((cfg.instructions, [s.name for s in cfg.skills], cfg.max_history_messages),
                         ("Be helpful.", ["triage"], 6))

    def test_config_requires_instructions(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump({"instructions": "  "}, f)
        self.addCleanup(os.remove, f.name)
        with self.assertRaises(ValueError):
            RuntimeConfig.load(f.name, SkillLoader())


class PromptTest(unittest.TestCase):
    def test_instructions_then_skills(self):
        cfg = RuntimeConfig(instructions="Base.", skills=[Skill("triage", "Triage pods", "1. look"), Skill("x", "", "y")])
        prompt = PromptBuilder.system_prompt(cfg)
        self.assertTrue(prompt.startswith("Base."))
        self.assertIn("## Skill: triage\nTriage pods\n\n1. look", prompt)
        self.assertIn("## Skill: x\n\ny", prompt)

    def test_no_skills(self):
        self.assertEqual(PromptBuilder.system_prompt(RuntimeConfig(instructions="Only this.")), "Only this.")


class AgentTest(unittest.TestCase):
    def test_answers_with_system_prompt_and_continues_trace(self):
        chat, tracer = FakeChat(), RecordingTracer()
        agent = agent_with(chat, tracer, skills=[Skill("triage", "Triage", "Check the pods.")])
        resp = agent.handle(send("why is checkout slow?", context_id="ctx-1"), traceparent=PARENT)

        result = resp["result"]
        self.assertEqual(result["parts"], [{"kind": "text", "text": "answer to why is checkout slow?"}])
        self.assertEqual((result["role"], result["contextId"]), ("agent", "ctx-1"))
        sent = chat.calls[0]["messages"]
        self.assertEqual(sent[0]["role"], "system")
        self.assertIn("payments on-call", sent[0]["content"])
        self.assertIn("Check the pods.", sent[0]["content"])

        span = tracer.spans()[0]
        self.assertEqual(span["traceId"], "0af7651916cd43dd8448eb211c80319c", "continues the caller's trace")
        self.assertEqual(span["parentSpanId"], "b7ad6b7169203331")
        self.assertEqual(chat.calls[0]["traceparent"], f"00-{span['traceId']}-{span['spanId']}-01",
                         "the gateway call is a child of the agent span")
        attrs = {a["key"]: list(a["value"].values())[0] for a in span["attributes"]}
        self.assertEqual(attrs["gen_ai.operation.name"], "invoke_agent")
        self.assertEqual(attrs["gen_ai.usage.input_tokens"], "10")
        self.assertEqual(attrs["gen_ai.conversation.id"], "ctx-1")
        self.assertEqual(span["status"], {"code": 1})

    def test_new_trace_without_parent(self):
        tracer = RecordingTracer()
        agent_with(tracer=tracer).handle(send("hi"), traceparent="garbage")
        span = tracer.spans()[0]
        self.assertEqual(len(span["traceId"]), 32)
        self.assertNotIn("parentSpanId", span)

    def test_remembers_the_conversation(self):
        chat = FakeChat()
        agent = agent_with(chat)
        agent.handle(send("first", context_id="ctx"))
        agent.handle(send("second", context_id="ctx"))
        second = chat.calls[1]["messages"]
        self.assertEqual([m["role"] for m in second], ["system", "user", "assistant", "user"])
        self.assertEqual(second[1]["content"], "first")
        agent.handle(send("other", context_id="ctx-2"))
        self.assertEqual(len(chat.calls[2]["messages"]), 2, "contexts are separate")

    def test_errors_are_json_rpc_and_traced(self):
        tracer = RecordingTracer()
        agent = agent_with(tracer=tracer)
        self.assertEqual(agent.handle({"jsonrpc": "2.0", "id": 3, "method": "tasks/get"})["error"]["code"],
                         METHOD_NOT_FOUND)
        self.assertEqual(agent.handle(send("   "))["error"]["code"], INVALID_PARAMS)
        self.assertEqual(tracer.spans()[0]["status"]["code"], 2)

    def test_gateway_failure(self):
        class Down(FakeChat):
            def complete(self, *a, **k):
                raise RpcError(INTERNAL_ERROR, "LLM gateway returned HTTP 503")
        resp = agent_with(Down()).handle(send("hi"))
        self.assertEqual(resp["error"], {"code": INTERNAL_ERROR, "message": "LLM gateway returned HTTP 503"})

    def test_tool_loop(self):
        class EchoTools(ToolProvider):
            def definitions(self):
                return [{"type": "function", "function": {"name": "lookup", "parameters": {"type": "object"}}}]

            def call(self, name, arguments, traceparent=""):
                return f"{name}({arguments}) = 42"
        chat = FakeChat(replies=[
            ({"role": "assistant", "content": None, "tool_calls": [
                {"id": "c1", "type": "function", "function": {"name": "lookup", "arguments": "{\"q\":1}"}}]},
             {"prompt_tokens": 5, "completion_tokens": 1}),
            ({"role": "assistant", "content": "It is 42."}, {"prompt_tokens": 8, "completion_tokens": 2}),
        ])
        resp = agent_with(chat, tools=EchoTools()).handle(send("what is it?"))
        self.assertEqual(resp["result"]["parts"][0]["text"], "It is 42.")
        tool_msg = chat.calls[1]["messages"][-1]
        self.assertEqual((tool_msg["role"], tool_msg["content"]), ("tool", 'lookup({"q":1}) = 42'))


class StoreTest(unittest.TestCase):
    def test_bounds(self):
        store = ConversationStore(max_messages=3, max_contexts=2)
        store.append("a", {"n": 1}, {"n": 2}, {"n": 3}, {"n": 4})
        self.assertEqual([m["n"] for m in store.history("a")], [2, 3, 4])
        store.append("b", {"n": 1})
        store.append("c", {"n": 1})
        self.assertEqual(store.history("a"), [], "least recently used context evicted")


class ChatClientTest(unittest.TestCase):
    def test_sends_key_model_and_traceparent(self):
        seen = {}

        def post(url, data, headers, timeout):
            seen.update(url=url, body=json.loads(data), headers=headers)
            return {"choices": [{"message": {"role": "assistant", "content": "ok"}}], "usage": {"prompt_tokens": 1}}
        msg, usage = ChatClient("http://gw/v1/", "sk-bf-k", "mock/mock-model", post=post).complete(
            [{"role": "user", "content": "hi"}], PARENT)
        self.assertEqual(msg["content"], "ok")
        self.assertEqual(seen["url"], "http://gw/v1/chat/completions")
        self.assertEqual(seen["headers"]["Authorization"], "Bearer sk-bf-k")
        self.assertEqual(seen["headers"]["traceparent"], PARENT)
        self.assertEqual(seen["body"]["model"], "mock/mock-model")
        self.assertNotIn("tools", seen["body"])

    def test_bad_response(self):
        with self.assertRaises(RpcError):
            ChatClient("http://gw", "k", "m", post=lambda *a: {"nope": 1}).complete([], PARENT)


class TracerTest(unittest.TestCase):
    def test_parse(self):
        self.assertEqual(Tracer.parse_resource_attributes("a=1, b = x=y ,bad"), {"a": "1", "b": "x=y"})
        self.assertIsNone(Tracer.context_from("00-" + "0" * 32 + "-b7ad6b7169203331-01"))
        self.assertEqual(Tracer.context_from(PARENT).span_id, "b7ad6b7169203331")

    def test_export_failures_are_swallowed(self):
        def boom(url, data):
            raise OSError("collector down")
        tracer = Tracer("http://collector:4318", "s", {}, send=boom)
        tracer.start("x", None, {}).end()  # must not raise

    def test_disabled_without_endpoint(self):
        sent = []
        Tracer("", "s", {}, send=lambda u, d: sent.append(u)).start("x", None, {}).end()
        self.assertEqual(sent, [])


class FakeMCP(BaseHTTPRequestHandler):
    """A tiny MCP server; class attributes switch its behaviour per test."""
    sse = False
    seen = []
    tools = [{"name": "oncall_get_recent_deploys", "description": "Recent deploys",
              "inputSchema": {"type": "object", "properties": {"service": {"type": "string"}}}}]

    def do_POST(self):  # noqa: N802
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        FakeMCP.seen.append({"method": body["method"], "session": self.headers.get("Mcp-Session-Id"),
                             "traceparent": self.headers.get("traceparent")})
        if "id" not in body:
            self.send_response(202)
            self.end_headers()
            return
        method, params = body["method"], body.get("params") or {}
        if method == "initialize":
            result = {"protocolVersion": "2025-06-18", "capabilities": {"tools": {}}, "serverInfo": {"name": "fake"}}
        elif method == "tools/list":
            result = {"tools": FakeMCP.tools}
        elif method == "tools/call" and params["name"] == "oncall_get_recent_deploys":
            result = {"content": [{"type": "text", "text": "v2.31.0 12 minutes ago by alice"}], "isError": False}
        elif method == "tools/call":
            result = {"content": [{"type": "text", "text": "unknown service"}], "isError": True}
        else:
            result = None
        msg = {"jsonrpc": "2.0", "id": body["id"], "result": result} if result is not None else \
            {"jsonrpc": "2.0", "id": body["id"], "error": {"code": -32601, "message": "nope"}}
        self.send_response(200)
        if method == "initialize":
            self.send_header("Mcp-Session-Id", "sess-1")
        if FakeMCP.sse:
            data = ("event: message\ndata: " + json.dumps(msg) + "\n\n").encode()
            self.send_header("Content-Type", "text/event-stream")
        else:
            data = json.dumps(msg).encode()
            self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args):
        pass


class MCPTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), FakeMCP)
        cls.url = f"http://127.0.0.1:{cls.server.server_address[1]}/mcp/team/agent"
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def setUp(self):
        FakeMCP.sse = False
        FakeMCP.seen = []

    def test_session_list_and_call(self):
        client = MCPClient(self.url)
        tools = client.list_tools(PARENT)
        self.assertEqual([t["name"] for t in tools], ["oncall_get_recent_deploys"])
        result = client.call_tool("oncall_get_recent_deploys", {"service": "checkout"}, PARENT)
        self.assertEqual(result["content"][0]["text"], "v2.31.0 12 minutes ago by alice")
        self.assertEqual([s["method"] for s in FakeMCP.seen],
                         ["initialize", "notifications/initialized", "tools/list", "tools/call"])
        self.assertIsNone(FakeMCP.seen[0]["session"])
        self.assertEqual({s["session"] for s in FakeMCP.seen[1:]}, {"sess-1"}, "session id reused")
        self.assertEqual(FakeMCP.seen[-1]["traceparent"], PARENT, "trace continues to the gateway")

    def test_sse_responses(self):
        FakeMCP.sse = True
        self.assertEqual(len(MCPClient(self.url).list_tools()), 1)

    def test_provider_definitions_and_calls(self):
        provider = MCPToolProvider(MCPClient(self.url))
        defs = provider.definitions()
        self.assertEqual(defs[0]["function"]["name"], "oncall_get_recent_deploys")
        self.assertEqual(defs[0]["function"]["parameters"]["properties"]["service"]["type"], "string")
        self.assertEqual(provider.call("oncall_get_recent_deploys", '{"service": "checkout"}'),
                         "v2.31.0 12 minutes ago by alice")
        self.assertEqual(provider.call("oncall_other", "{}"), "Error: unknown service")
        self.assertTrue(provider.call("x", "not json").startswith("Error:"))

    def test_unreachable_gateway_means_no_tools_not_failure(self):
        provider = MCPToolProvider(MCPClient("http://127.0.0.1:9/mcp", timeout=1))
        self.assertEqual(provider.definitions(), [])
        self.assertTrue(provider.call("anything", "{}").startswith("Error:"))

    def test_agent_uses_mcp_tools_end_to_end(self):
        chat = FakeChat(replies=[
            ({"role": "assistant", "content": None, "tool_calls": [{"id": "c1", "type": "function", "function": {
                "name": "oncall_get_recent_deploys", "arguments": '{"service": "checkout"}'}}]}, {}),
            ({"role": "assistant", "content": "The 12-minute-old deploy v2.31.0 is the likely cause."}, {}),
        ])
        agent = agent_with(chat, tools=MCPToolProvider(MCPClient(self.url)))
        resp = agent.handle(send("why is checkout failing?"))
        self.assertEqual(resp["result"]["parts"][0]["text"], "The 12-minute-old deploy v2.31.0 is the likely cause.")
        self.assertEqual(chat.calls[0]["tools"][0]["function"]["name"], "oncall_get_recent_deploys")
        tool_msg = chat.calls[1]["messages"][-1]
        self.assertEqual((tool_msg["role"], tool_msg["content"]), ("tool", "v2.31.0 12 minutes ago by alice"))

    def test_config_reads_mcp_flag(self):
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump({"instructions": "x", "mcp": True}, f)
        self.addCleanup(os.remove, f.name)
        self.assertTrue(RuntimeConfig.load(f.name, SkillLoader()).mcp)


class HttpTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.chat = FakeChat()
        cls.server = build_server(agent_with(cls.chat), {"name": "oncall-card"}, host="127.0.0.1", port=0)
        cls.base = f"http://127.0.0.1:{cls.server.server_address[1]}"
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def test_card_health_and_send(self):
        with urllib.request.urlopen(self.base + "/.well-known/agent-card.json", timeout=5) as r:
            self.assertEqual(json.load(r), {"name": "oncall-card"})
        with urllib.request.urlopen(self.base + "/readyz", timeout=5) as r:
            self.assertEqual(r.status, 200)
        req = urllib.request.Request(self.base + "/", data=json.dumps(send("ping")).encode(), method="POST",
                                     headers={"Content-Type": "application/json", "traceparent": PARENT})
        with urllib.request.urlopen(req, timeout=5) as r:
            self.assertEqual(json.load(r)["result"]["parts"][0]["text"], "answer to ping")
        self.assertTrue(self.chat.calls[-1]["traceparent"].startswith("00-0af7651916cd43dd8448eb211c80319c-"))

    def test_load_card_fallback(self):
        self.assertEqual(load_card("/nope", "x")["name"], "x")


if __name__ == "__main__":
    unittest.main()
