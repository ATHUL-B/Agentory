"""Deterministic OpenAI-compatible mock LLM for local development and CI.

Serves the subset of the OpenAI API that Bifrost uses, so the whole data plane
(agent -> Bifrost -> provider) can be exercised without a real API key.

Behaviour (deterministic, see MockCompletionEngine):
  * Plain chat: replies "mock reply: <last user message>".
  * Tools offered and none called yet since the last user turn: returns a call
    to the first tool with empty arguments (finish_reason "tool_calls").
  * After a tool result: replies "mock reply: tool <name> returned: <result>".
  * A user message containing "[[error:<status>]]" makes the server return
    that HTTP status, to test error handling.

Standard library only, so it runs from a stock python image with no build step.
"""

import json
import os
import re
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict, Iterator, List, Optional, Tuple

MODEL_ID = "mock-model"
ERROR_TRIGGER = re.compile(r"\[\[error:(\d{3})\]\]")
MAX_ECHO_CHARS = 200


class MockError(Exception):
    """Raised when a request asks for a simulated provider error."""

    def __init__(self, status: int, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.message = message


class TokenCounter:
    """Approximates token usage by counting whitespace-separated words."""

    @staticmethod
    def count(text: str) -> int:
        return len(text.split())

    def count_messages(self, messages: List[Dict[str, Any]]) -> int:
        return sum(self.count(MessageText.of(m)) for m in messages)


class MessageText:
    """Extracts plain text from OpenAI message content (string or parts list)."""

    @staticmethod
    def of(message: Dict[str, Any]) -> str:
        content = message.get("content")
        if content is None:
            return ""
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            return " ".join(
                part.get("text", "") for part in content if isinstance(part, dict)
            )
        return str(content)


class MockCompletionEngine:
    """Turns a chat-completion request into a deterministic response body."""

    def __init__(self, clock=time.time, tokens: Optional[TokenCounter] = None) -> None:
        self._clock = clock
        self._tokens = tokens or TokenCounter()
        self._counter = 0

    def complete(self, request: Dict[str, Any]) -> Dict[str, Any]:
        messages = request.get("messages")
        if not isinstance(messages, list) or not messages:
            raise MockError(400, "messages must be a non-empty list")

        self._raise_if_error_requested(messages)
        message, finish_reason = self._next_message(messages, request.get("tools") or [])
        completion_tokens = self._tokens.count(message.get("content") or "") + (
            len(message.get("tool_calls", [])) * 5
        )
        prompt_tokens = self._tokens.count_messages(messages)
        self._counter += 1
        return {
            "id": f"chatcmpl-mock-{self._counter}",
            "object": "chat.completion",
            "created": int(self._clock()),
            "model": request.get("model") or MODEL_ID,
            "choices": [{"index": 0, "message": message, "finish_reason": finish_reason}],
            "usage": {
                "prompt_tokens": prompt_tokens,
                "completion_tokens": completion_tokens,
                "total_tokens": prompt_tokens + completion_tokens,
            },
        }

    @staticmethod
    def _raise_if_error_requested(messages: List[Dict[str, Any]]) -> None:
        last_user = next((m for m in reversed(messages) if m.get("role") == "user"), None)
        if last_user is None:
            return
        match = ERROR_TRIGGER.search(MessageText.of(last_user))
        if match:
            status = int(match.group(1))
            raise MockError(status, f"simulated provider error {status}")

    def _next_message(
        self, messages: List[Dict[str, Any]], tools: List[Dict[str, Any]]
    ) -> Tuple[Dict[str, Any], str]:
        last = messages[-1]
        if last.get("role") == "tool":
            name = self._tool_name_for(messages, last.get("tool_call_id"))
            result = MessageText.of(last)[:MAX_ECHO_CHARS]
            return {"role": "assistant", "content": f"mock reply: tool {name} returned: {result}"}, "stop"

        if tools:
            tool = tools[0].get("function", {})
            call_id = f"call_mock_{self._counter + 1}"
            return {
                "role": "assistant",
                "content": None,
                "tool_calls": [{
                    "id": call_id,
                    "type": "function",
                    "function": {"name": tool.get("name", "unknown"), "arguments": "{}"},
                }],
            }, "tool_calls"

        text = MessageText.of(last)[:MAX_ECHO_CHARS]
        return {"role": "assistant", "content": f"mock reply: {text}"}, "stop"

    @staticmethod
    def _tool_name_for(messages: List[Dict[str, Any]], call_id: Optional[str]) -> str:
        for message in reversed(messages):
            for call in message.get("tool_calls") or []:
                if call.get("id") == call_id:
                    return call.get("function", {}).get("name", "unknown")
        return "unknown"


class StreamEncoder:
    """Encodes a completion as OpenAI-style server-sent event chunks."""

    def chunks(self, completion: Dict[str, Any]) -> Iterator[bytes]:
        choice = completion["choices"][0]
        message = choice["message"]
        base = {
            "id": completion["id"],
            "object": "chat.completion.chunk",
            "created": completion["created"],
            "model": completion["model"],
        }
        delta: Dict[str, Any] = {"role": "assistant"}
        if message.get("content") is not None:
            delta["content"] = message["content"]
        if message.get("tool_calls"):
            delta["tool_calls"] = [
                dict(call, index=i) for i, call in enumerate(message["tool_calls"])
            ]
        yield self._event(dict(base, choices=[{"index": 0, "delta": delta, "finish_reason": None}]))
        yield self._event(dict(
            base,
            choices=[{"index": 0, "delta": {}, "finish_reason": choice["finish_reason"]}],
            usage=completion["usage"],
        ))
        yield b"data: [DONE]\n\n"

    @staticmethod
    def _event(payload: Dict[str, Any]) -> bytes:
        return f"data: {json.dumps(payload)}\n\n".encode()


class MockLLMHandler(BaseHTTPRequestHandler):
    """HTTP routing for the mock. Accepts any path prefix (e.g. /v1 or /v1/v1)."""

    engine = MockCompletionEngine()
    encoder = StreamEncoder()
    server_version = "agentory-mock-llm/1"

    def do_GET(self) -> None:  # noqa: N802 (http.server naming)
        if self.path.rstrip("/") in ("/healthz", "/health"):
            self._send_json(200, {"status": "ok"})
        elif self.path.rstrip("/").endswith("/models"):
            self._send_json(200, {
                "object": "list",
                "data": [{"id": MODEL_ID, "object": "model", "created": 0, "owned_by": "agentory"}],
            })
        else:
            self._send_error(404, f"no route for GET {self.path}")

    def do_POST(self) -> None:  # noqa: N802
        if not self.path.split("?")[0].rstrip("/").endswith("/chat/completions"):
            self._send_error(404, f"no route for POST {self.path}")
            return
        try:
            request = self._read_json()
            completion = self.engine.complete(request)
        except MockError as err:
            self._send_error(err.status, err.message)
            return
        if request.get("stream"):
            self._send_stream(completion)
        else:
            self._send_json(200, completion)

    def log_message(self, fmt: str, *args: Any) -> None:
        # One JSON line per request keeps logs greppable in kubectl.
        print(json.dumps({"msg": fmt % args, "path": self.path}), flush=True)

    def _read_json(self) -> Dict[str, Any]:
        length = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError as err:
            raise MockError(400, f"invalid JSON: {err}") from err
        if not isinstance(body, dict):
            raise MockError(400, "request body must be a JSON object")
        return body

    def _send_json(self, status: int, payload: Dict[str, Any]) -> None:
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _send_error(self, status: int, message: str) -> None:
        self._send_json(status, {"error": {"message": message, "type": "mock_error", "code": status}})

    def _send_stream(self, completion: Dict[str, Any]) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        for chunk in self.encoder.chunks(completion):
            self.wfile.write(chunk)
            self.wfile.flush()


def build_server(host: str = "0.0.0.0", port: int = 8080) -> ThreadingHTTPServer:
    return ThreadingHTTPServer((host, port), MockLLMHandler)


def main() -> None:
    port = int(os.environ.get("PORT", "8080"))
    server = build_server(port=port)
    print(json.dumps({"msg": f"mock LLM listening on :{port}"}), flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
