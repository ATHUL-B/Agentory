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
    ResponsesEngine,
    ResponsesStreamEncoder,
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


class ChatToolDirectiveTest(unittest.TestCase):
    def test_directive_calls_named_tool_with_arguments(self):
        tools = [WEATHER_TOOL, {"type": "function", "function": {"name": "Bash", "parameters": {}}}]
        msg = user('do it [[tool:Bash {"command": "ls"}]]')
        call = MockCompletionEngine().complete({"messages": [msg], "tools": tools})["choices"][0]["message"]["tool_calls"][0]
        self.assertEqual(call["function"], {"name": "Bash", "arguments": '{"command": "ls"}'})

    def test_directive_for_a_tool_not_offered_falls_back_to_first(self):
        msg = user('[[tool:Missing {"x": 1}]]')
        call = MockCompletionEngine().complete({"messages": [msg], "tools": [WEATHER_TOOL]})["choices"][0]["message"]["tool_calls"][0]
        self.assertEqual(call["function"]["name"], "get_weather")

    def test_invalid_directive_json_is_400(self):
        with self.assertRaises(MockError) as ctx:
            MockCompletionEngine().complete({"messages": [user("[[tool:get_weather {nope}]]")], "tools": [WEATHER_TOOL]})
        self.assertEqual(ctx.exception.status, 400)


BASH = {"type": "function", "name": "Bash", "parameters": {"type": "object"}}
READ = {"type": "function", "name": "Read", "parameters": {"type": "object"}}


def text_of(response):
    return response["output"][0]["content"][0]["text"]


class ResponsesEngineTest(unittest.TestCase):
    def setUp(self):
        self.engine = ResponsesEngine(clock=lambda: 1_700_000_000)

    def test_string_input_gets_a_text_message(self):
        resp = self.engine.respond({"model": "mock/mock-model", "input": "hello"})
        self.assertEqual(resp["object"], "response")
        self.assertEqual(resp["status"], "completed")
        self.assertEqual(resp["output"][0]["type"], "message")
        self.assertEqual(text_of(resp), "mock reply: hello")
        self.assertEqual(resp["usage"], {"input_tokens": 1, "output_tokens": 3, "total_tokens": 4})

    def test_list_input_with_parts_and_developer_role(self):
        resp = self.engine.respond({"input": [
            {"role": "developer", "content": "be brief"},
            {"type": "message", "role": "user", "content": [{"type": "input_text", "text": "part one"}]},
        ]})
        self.assertEqual(text_of(resp), "mock reply: part one")

    def test_calls_first_tool_then_answers_from_its_output(self):
        first = self.engine.respond({"input": "go", "tools": [BASH, READ]})
        call = first["output"][0]
        self.assertEqual((call["type"], call["name"], call["arguments"]), ("function_call", "Bash", "{}"))

        second = self.engine.respond({"input": [
            {"role": "user", "content": "go"},
            {"type": "function_call", "call_id": call["call_id"], "name": "Bash", "arguments": "{}"},
            {"type": "function_call_output", "call_id": call["call_id"], "output": "file.txt"},
        ], "tools": [BASH, READ]})
        self.assertEqual(text_of(second), "mock reply: tool Bash returned: file.txt")

    def test_directive_picks_tool_and_arguments(self):
        resp = self.engine.respond({"input": 'list [[tool:Read {"file_path": "/a"}]]', "tools": [BASH, READ]})
        call = resp["output"][0]
        self.assertEqual((call["name"], json.loads(call["arguments"])), ("Read", {"file_path": "/a"}))

    def test_errors(self):
        with self.assertRaises(MockError) as ctx:
            self.engine.respond({"input": "please [[error:529]]"})
        self.assertEqual(ctx.exception.status, 529)
        for bad in ({}, {"input": []}, {"input": 7}):
            with self.subTest(bad=bad), self.assertRaises(MockError):
                self.engine.respond(bad)


class ResponsesStreamTest(unittest.TestCase):
    @staticmethod
    def parse(events):
        out = []
        for raw in events:
            lines = raw.decode().strip().split("\n")
            name = lines[0][len("event: "):]
            data = json.loads(lines[1][len("data: "):])
            out.append((name, data))
        return out

    def test_text_stream(self):
        resp = ResponsesEngine().respond({"input": "hi"})
        events = self.parse(ResponsesStreamEncoder().events(resp))
        names = [n for n, _ in events]
        self.assertEqual(names[0], "response.created")
        self.assertEqual(names[-1], "response.completed")
        self.assertIn("response.output_text.delta", names)
        delta = next(d for n, d in events if n == "response.output_text.delta")
        self.assertEqual(delta["delta"], "mock reply: hi")
        self.assertEqual([d["sequence_number"] for _, d in events], list(range(1, len(events) + 1)))
        for name, data in events:
            self.assertEqual(data["type"], name)

    def test_function_call_stream(self):
        resp = ResponsesEngine().respond({"input": "go", "tools": [BASH]})
        events = self.parse(ResponsesStreamEncoder().events(resp))
        done = next(d for n, d in events if n == "response.function_call_arguments.done")
        self.assertEqual(done["arguments"], "{}")
        self.assertEqual(events[-1][1]["response"]["output"][0]["name"], "Bash")


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

    def test_responses_endpoint(self):
        status, ctype, body = self.request("POST", "/v1/responses", {"input": "ping"})
        self.assertEqual((status, ctype), (200, "application/json"))
        self.assertEqual(json.loads(body)["output"][0]["content"][0]["text"], "mock reply: ping")

    def test_responses_streaming(self):
        status, ctype, body = self.request("POST", "/v1/responses", {"input": "ping", "stream": True})
        self.assertEqual((status, ctype), (200, "text/event-stream"))
        self.assertIn(b"event: response.completed", body)

    def test_unknown_route_is_404(self):
        self.assertEqual(self.request("GET", "/nope")[0], 404)
        self.assertEqual(self.request("POST", "/v1/embeddings", {"input": "x"})[0], 404)


if __name__ == "__main__":
    unittest.main()
