"""Deterministic OpenAI-compatible mock LLM for local development and CI.

Serves the subset of the OpenAI API that Bifrost uses, so the whole data plane
(agent -> Bifrost -> provider) can be exercised without a real API key:

  * Chat Completions  POST .../chat/completions  (OpenAI SDK clients)
  * Responses         POST .../responses         (what Bifrost calls when an
    Anthropic-SDK client such as Claude Code talks to an OpenAI-style provider)

Both share one deterministic policy (MockBrain):
  * Plain chat: replies "mock reply: <last user message>".
  * Tools offered and none called yet since the last user turn: calls a tool.
    By default the first tool with empty arguments; a user message containing
    [[tool:NAME {json args}]] calls NAME with those arguments instead, if offered.
  * After a tool result: replies "mock reply: tool <name> returned: <result>".
  * A user message containing "[[error:<status>]]" makes the server return
    that HTTP status, to test error handling.

Standard library only, so it runs from a stock python image with no build step.
"""

import json
import os
import re
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Dict, Iterator, List, Optional, Tuple

MODEL_ID = "mock-model"
ERROR_TRIGGER = re.compile(r"\[\[error:(\d{3})\]\]")
TOOL_DIRECTIVE = re.compile(r"\[\[tool:([A-Za-z0-9_.\-]+)\s+(\{.*?\})\]\]", re.S)
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


class MessageText:
    """Extracts plain text from OpenAI content (a string or a list of parts)."""

    @staticmethod
    def of(message: Dict[str, Any]) -> str:
        return MessageText.from_content(message.get("content"))

    @staticmethod
    def from_content(content: Any) -> str:
        if content is None:
            return ""
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            return " ".join(
                part.get("text", "") for part in content if isinstance(part, dict)
            )
        return str(content)


@dataclass
class Turn:
    """One conversation item in API-neutral form."""
    role: str  # "user", "assistant", "system" or "tool"
    text: str = ""
    tool_name: str = ""  # for tool results: the tool that produced them


@dataclass
class Decision:
    """What the mock answers with."""
    text: Optional[str] = None
    tool_name: Optional[str] = None
    tool_args: Dict[str, Any] = field(default_factory=dict)

    @property
    def is_tool_call(self) -> bool:
        return self.tool_name is not None


class MockBrain:
    """The deterministic policy both API adapters share."""

    def decide(self, turns: List[Turn], tool_names: List[str]) -> Decision:
        if not turns:
            raise MockError(400, "the conversation is empty")
        last_user = next((t for t in reversed(turns) if t.role == "user"), None)
        user_text = last_user.text if last_user else ""

        match = ERROR_TRIGGER.search(user_text)
        if match:
            status = int(match.group(1))
            raise MockError(status, f"simulated provider error {status}")

        last = turns[-1]
        if last.role == "tool":
            result = last.text[:MAX_ECHO_CHARS]
            return Decision(text=f"mock reply: tool {last.tool_name or 'unknown'} returned: {result}")

        if tool_names:
            directive = TOOL_DIRECTIVE.search(user_text)
            if directive and directive.group(1) in tool_names:
                try:
                    args = json.loads(directive.group(2))
                except json.JSONDecodeError as err:
                    raise MockError(400, f"tool directive arguments are not JSON: {err}") from err
                return Decision(tool_name=directive.group(1), tool_args=args)
            return Decision(tool_name=tool_names[0])

        return Decision(text=f"mock reply: {last.text[:MAX_ECHO_CHARS]}")


class MockCompletionEngine:
    """Chat Completions adapter: request body in, response body out."""

    def __init__(self, clock=time.time, tokens: Optional[TokenCounter] = None,
                 brain: Optional[MockBrain] = None) -> None:
        self._clock = clock
        self._tokens = tokens or TokenCounter()
        self._brain = brain or MockBrain()
        self._counter = 0

    def complete(self, request: Dict[str, Any]) -> Dict[str, Any]:
        messages = request.get("messages")
        if not isinstance(messages, list) or not messages:
            raise MockError(400, "messages must be a non-empty list")

        turns = self._turns(messages)
        tools = [t.get("function", {}).get("name", "unknown") for t in request.get("tools") or []]
        decision = self._brain.decide(turns, tools)
        self._counter += 1

        if decision.is_tool_call:
            message = {"role": "assistant", "content": None, "tool_calls": [{
                "id": f"call_mock_{self._counter}",
                "type": "function",
                "function": {"name": decision.tool_name, "arguments": json.dumps(decision.tool_args)},
            }]}
            finish_reason, completion_tokens = "tool_calls", 5
        else:
            message = {"role": "assistant", "content": decision.text}
            finish_reason, completion_tokens = "stop", self._tokens.count(decision.text or "")

        prompt_tokens = sum(self._tokens.count(t.text) for t in turns)
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
    def _turns(messages: List[Dict[str, Any]]) -> List[Turn]:
        names: Dict[str, str] = {}
        turns = []
        for m in messages:
            for call in m.get("tool_calls") or []:
                names[call.get("id", "")] = call.get("function", {}).get("name", "unknown")
            role = m.get("role", "user")
            turns.append(Turn(role=role, text=MessageText.of(m),
                              tool_name=names.get(m.get("tool_call_id", ""), "") if role == "tool" else ""))
        return turns


class StreamEncoder:
    """Encodes a chat completion as OpenAI-style server-sent event chunks."""

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


class ResponsesEngine:
    """OpenAI Responses API adapter: request body in, response object out."""

    def __init__(self, clock=time.time, tokens: Optional[TokenCounter] = None,
                 brain: Optional[MockBrain] = None) -> None:
        self._clock = clock
        self._tokens = tokens or TokenCounter()
        self._brain = brain or MockBrain()
        self._counter = 0

    def respond(self, request: Dict[str, Any]) -> Dict[str, Any]:
        turns = self._turns(request.get("input"))
        if not turns:
            raise MockError(400, "input must be a non-empty string or list")
        tools = [t.get("name") or t.get("function", {}).get("name", "unknown")
                 for t in request.get("tools") or [] if isinstance(t, dict)]
        decision = self._brain.decide(turns, tools)
        self._counter += 1
        n = self._counter

        if decision.is_tool_call:
            output = [{
                "type": "function_call", "id": f"fc_mock_{n}", "call_id": f"call_mock_{n}",
                "name": decision.tool_name, "arguments": json.dumps(decision.tool_args), "status": "completed",
            }]
            output_tokens = 5
        else:
            output = [{
                "type": "message", "id": f"msg_mock_{n}", "status": "completed", "role": "assistant",
                "content": [{"type": "output_text", "text": decision.text, "annotations": []}],
            }]
            output_tokens = self._tokens.count(decision.text or "")

        input_tokens = sum(self._tokens.count(t.text) for t in turns)
        return {
            "id": f"resp_mock_{n}",
            "object": "response",
            "created_at": int(self._clock()),
            "status": "completed",
            "model": request.get("model") or MODEL_ID,
            "output": output,
            "usage": {"input_tokens": input_tokens, "output_tokens": output_tokens,
                      "total_tokens": input_tokens + output_tokens},
        }

    @staticmethod
    def _turns(items: Any) -> List[Turn]:
        if isinstance(items, str):
            return [Turn(role="user", text=items)] if items else []
        if not isinstance(items, list):
            return []
        names: Dict[str, str] = {}
        turns = []
        for item in items:
            if not isinstance(item, dict):
                continue
            kind = item.get("type", "message")
            if kind == "function_call":
                names[item.get("call_id", "")] = item.get("name", "unknown")
                turns.append(Turn(role="assistant", text=item.get("arguments", "")))
            elif kind == "function_call_output":
                output = item.get("output")
                text = output if isinstance(output, str) else MessageText.from_content(output)
                turns.append(Turn(role="tool", text=text, tool_name=names.get(item.get("call_id", ""), "")))
            elif kind == "message" or "role" in item:
                role = item.get("role", "user")
                turns.append(Turn(role="system" if role == "developer" else role,
                                  text=MessageText.from_content(item.get("content"))))
        return turns


class ResponsesStreamEncoder:
    """Encodes a Responses API result as its streaming event sequence."""

    def events(self, response: Dict[str, Any]) -> Iterator[bytes]:
        seq = 0

        def event(kind: str, **data: Any) -> bytes:
            nonlocal seq
            seq += 1
            payload = dict(type=kind, sequence_number=seq, **data)
            return f"event: {kind}\ndata: {json.dumps(payload)}\n\n".encode()

        in_progress = dict(response, status="in_progress", output=[])
        yield event("response.created", response=in_progress)
        yield event("response.in_progress", response=in_progress)
        for index, item in enumerate(response["output"]):
            if item["type"] == "message":
                text = item["content"][0]["text"]
                yield event("response.output_item.added", output_index=index,
                            item=dict(item, status="in_progress", content=[]))
                part = {"type": "output_text", "text": "", "annotations": []}
                yield event("response.content_part.added", item_id=item["id"], output_index=index,
                            content_index=0, part=part)
                yield event("response.output_text.delta", item_id=item["id"], output_index=index,
                            content_index=0, delta=text)
                yield event("response.output_text.done", item_id=item["id"], output_index=index,
                            content_index=0, text=text)
                yield event("response.content_part.done", item_id=item["id"], output_index=index,
                            content_index=0, part=dict(part, text=text))
            else:
                yield event("response.output_item.added", output_index=index,
                            item=dict(item, status="in_progress", arguments=""))
                yield event("response.function_call_arguments.delta", item_id=item["id"],
                            output_index=index, delta=item["arguments"])
                yield event("response.function_call_arguments.done", item_id=item["id"],
                            output_index=index, arguments=item["arguments"])
            yield event("response.output_item.done", output_index=index, item=item)
        yield event("response.completed", response=response)


class MockLLMHandler(BaseHTTPRequestHandler):
    """HTTP routing for the mock. Accepts any path prefix (e.g. /v1 or /v1/v1)."""

    engine = MockCompletionEngine()
    # Not "responses": BaseHTTPRequestHandler uses that name for status messages.
    responses_engine = ResponsesEngine()
    encoder = StreamEncoder()
    responses_encoder = ResponsesStreamEncoder()
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
        path = self.path.split("?")[0].rstrip("/")
        if path.endswith("/chat/completions"):
            self._handle(self.engine.complete, self.encoder.chunks)
        elif path.endswith("/responses"):
            self._handle(self.responses_engine.respond, self.responses_encoder.events)
        else:
            self._send_error(404, f"no route for POST {self.path}")

    def _handle(self, produce, stream) -> None:
        try:
            request = self._read_json()
            body = produce(request)
        except MockError as err:
            self._send_error(err.status, err.message)
            return
        if request.get("stream"):
            self._send_stream(stream(body))
        else:
            self._send_json(200, body)

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

    def _send_stream(self, chunks: Iterator[bytes]) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        for chunk in chunks:
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
