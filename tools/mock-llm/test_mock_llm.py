"""Tests for the mock LLM. Run: python3 -m unittest discover -s tools/mock-llm"""

import json
import threading
import unittest
import urllib.error
import urllib.request

from mock_llm import (
    MODEL_ID,
    MockCompletionEngine,
    MockError,
    StreamEncoder,
    build_server,
)

WEATHER_TOOL = {
    "type": "function",
    "function": {"name": "get_weather", "parameters": {"type": "object", "properties": {}}},
}


def user(text):
    return {"role": "user", "content": text}


class EngineTest(unittest.TestCase):
    def setUp(self):
        self.engine = MockCompletionEngine(clock=lambda: 1_700_000_000)

    def test_plain_reply_echoes_last_user_message(self):
        result = self.engine.complete({"model": "mock/mock-model", "messages": [user("hello there")]})
        choice = result["choices"][0]
        self.assertEqual(choice["message"], {"role": "assistant", "content": "mock reply: hello there"})
        self.assertEqual(choice["finish_reason"], "stop")
        self.assertEqual(result["model"], "mock/mock-model")
        self.assertEqual(result["created"], 1_700_000_000)

    def test_usage_counts_words_and_totals(self):
        usage = self.engine.complete({"messages": [user("one two three")]})["usage"]
        self.assertEqual(usage["prompt_tokens"], 3)
        self.assertEqual(usage["completion_tokens"], 5)  # "mock reply: one two three"
        self.assertEqual(usage["total_tokens"], 8)

    def test_defaults_model_when_missing(self):
        self.assertEqual(self.engine.complete({"messages": [user("x")]})["model"], MODEL_ID)

    def test_ids_are_unique_per_request(self):
        first = self.engine.complete({"messages": [user("a")]})["id"]
        second = self.engine.complete({"messages": [user("b")]})["id"]
        self.assertNotEqual(first, second)

    def test_content_parts_are_flattened(self):
        message = {"role": "user", "content": [{"type": "text", "text": "part one"}, {"type": "text", "text": "two"}]}
        reply = self.engine.complete({"messages": [message]})["choices"][0]["message"]["content"]
        self.assertEqual(reply, "mock reply: part one two")

    def test_long_messages_are_truncated(self):
        reply = self.engine.complete({"messages": [user("x" * 1000)]})["choices"][0]["message"]["content"]
        self.assertEqual(len(reply), len("mock reply: ") + 200)

    def test_calls_first_tool_when_tools_offered(self):
        result = self.engine.complete({"messages": [user("weather?")], "tools": [WEATHER_TOOL]})
        choice = result["choices"][0]
        self.assertEqual(choice["finish_reason"], "tool_calls")
        self.assertIsNone(choice["message"]["content"])
        call = choice["message"]["tool_calls"][0]
        self.assertEqual(call["function"], {"name": "get_weather", "arguments": "{}"})
        self.assertEqual(call["type"], "function")

    def test_answers_after_tool_result(self):
        first = self.engine.complete({"messages": [user("weather?")], "tools": [WEATHER_TOOL]})
        assistant = first["choices"][0]["message"]
        call_id = assistant["tool_calls"][0]["id"]
        messages = [user("weather?"), assistant, {"role": "tool", "tool_call_id": call_id, "content": "sunny"}]
        second = self.engine.complete({"messages": messages, "tools": [WEATHER_TOOL]})
        self.assertEqual(second["choices"][0]["finish_reason"], "stop")
        self.assertEqual(second["choices"][0]["message"]["content"], "mock reply: tool get_weather returned: sunny")

    def test_error_trigger_raises_requested_status(self):
        with self.assertRaises(MockError) as ctx:
            self.engine.complete({"messages": [user("please fail [[error:429]]")]})
        self.assertEqual(ctx.exception.status, 429)

    def test_rejects_missing_messages(self):
        for bad in ({}, {"messages": []}, {"messages": "hi"}):
            with self.subTest(bad=bad), self.assertRaises(MockError) as ctx:
                self.engine.complete(bad)
            self.assertEqual(ctx.exception.status, 400)


class StreamEncoderTest(unittest.TestCase):
    def test_stream_has_content_finish_and_done(self):
        completion = MockCompletionEngine(clock=lambda: 0).complete({"messages": [user("hi")]})
        events = list(StreamEncoder().chunks(completion))
        self.assertEqual(events[-1], b"data: [DONE]\n\n")
        first = json.loads(events[0].decode()[len("data: "):])
        last = json.loads(events[1].decode()[len("data: "):])
        self.assertEqual(first["choices"][0]["delta"]["content"], "mock reply: hi")
        self.assertEqual(last["choices"][0]["finish_reason"], "stop")
        self.assertEqual(last["usage"], completion["usage"])

    def test_stream_tool_calls_carry_index(self):
        completion = MockCompletionEngine().complete({"messages": [user("x")], "tools": [WEATHER_TOOL]})
        first = json.loads(next(StreamEncoder().chunks(completion)).decode()[len("data: "):])
        self.assertEqual(first["choices"][0]["delta"]["tool_calls"][0]["index"], 0)


class HttpServerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = build_server(host="127.0.0.1", port=0)
        cls.base = f"http://127.0.0.1:{cls.server.server_address[1]}"
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def request(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method,
                                     headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                return resp.status, resp.headers.get("Content-Type"), resp.read()
        except urllib.error.HTTPError as err:
            return err.code, err.headers.get("Content-Type"), err.read()

    def test_health(self):
        status, _, body = self.request("GET", "/healthz")
        self.assertEqual((status, json.loads(body)), (200, {"status": "ok"}))

    def test_models_list_under_any_prefix(self):
        for path in ("/v1/models", "/v1/v1/models"):
            with self.subTest(path=path):
                status, _, body = self.request("GET", path)
                self.assertEqual(status, 200)
                self.assertEqual(json.loads(body)["data"][0]["id"], MODEL_ID)

    def test_chat_completion_under_any_prefix(self):
        for path in ("/v1/chat/completions", "/v1/v1/chat/completions", "/chat/completions"):
            with self.subTest(path=path):
                status, ctype, body = self.request("POST", path, {"messages": [user("ping")]})
                self.assertEqual(status, 200)
                self.assertEqual(ctype, "application/json")
                self.assertEqual(json.loads(body)["choices"][0]["message"]["content"], "mock reply: ping")

    def test_streaming(self):
        status, ctype, body = self.request("POST", "/v1/chat/completions",
                                           {"messages": [user("ping")], "stream": True})
        self.assertEqual(status, 200)
        self.assertEqual(ctype, "text/event-stream")
        self.assertTrue(body.endswith(b"data: [DONE]\n\n"))

    def test_error_trigger_over_http(self):
        status, _, body = self.request("POST", "/v1/chat/completions", {"messages": [user("[[error:503]]")]})
        self.assertEqual(status, 503)
        self.assertEqual(json.loads(body)["error"]["code"], 503)

    def test_invalid_json_is_400(self):
        req = urllib.request.Request(self.base + "/v1/chat/completions", data=b"{not json",
                                     method="POST", headers={"Content-Type": "application/json"})
        with self.assertRaises(urllib.error.HTTPError) as ctx:
            urllib.request.urlopen(req, timeout=5)
        self.assertEqual(ctx.exception.code, 400)

    def test_unknown_route_is_404(self):
        self.assertEqual(self.request("GET", "/nope")[0], 404)
        self.assertEqual(self.request("POST", "/v1/embeddings", {"input": "x"})[0], 404)


if __name__ == "__main__":
    unittest.main()
