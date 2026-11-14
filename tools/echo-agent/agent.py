"""Minimal A2A agent: the reference "bring your own container" agent.

Shows the contract an Agentory container agent relies on, using only what the
operator injects:

  * AGENTORY_AGENT_CARD_PATH  the generated Agent Card, served as-is
  * AGENTORY_LLM_BASE_URL     the LLM gateway (OpenAI-compatible)
  * AGENTORY_LLM_API_KEY      the agent's own virtual key
  * AGENTORY_MODEL            provider/model the key allows

It answers A2A JSON-RPC `message/send` by asking the model to respond to the
user's text. Standard library only.
"""

import json
import os
import urllib.error
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, Optional

CARD_PATHS = ("/.well-known/agent-card.json", "/.well-known/agent.json")

# JSON-RPC 2.0 error codes.
PARSE_ERROR = -32700
INVALID_REQUEST = -32600
METHOD_NOT_FOUND = -32601
INVALID_PARAMS = -32602
INTERNAL_ERROR = -32603


class RpcError(Exception):
    def __init__(self, code: int, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


class LLMClient:
    """Calls the OpenAI-compatible chat API of the LLM gateway."""

    def __init__(self, base_url: str, api_key: str, model: str, timeout: float = 30.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.model = model
        self.timeout = timeout

    def complete(self, text: str) -> str:
        body = json.dumps({"model": self.model, "messages": [{"role": "user", "content": text}]}).encode()
        req = urllib.request.Request(
            self.base_url + "/chat/completions", data=body, method="POST",
            headers={"Content-Type": "application/json", "Authorization": "Bearer " + self.api_key},
        )
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                data = json.load(resp)
        except urllib.error.HTTPError as err:
            raise RpcError(INTERNAL_ERROR, f"LLM gateway returned HTTP {err.code}") from err
        except (urllib.error.URLError, TimeoutError) as err:
            raise RpcError(INTERNAL_ERROR, f"LLM gateway unreachable: {err}") from err
        try:
            return data["choices"][0]["message"]["content"] or ""
        except (KeyError, IndexError, TypeError) as err:
            raise RpcError(INTERNAL_ERROR, "unexpected LLM gateway response") from err


class AgentCard:
    """Loads the operator-generated card, or builds a minimal one."""

    @staticmethod
    def load(path: Optional[str], fallback_name: str) -> Dict[str, Any]:
        if path and os.path.exists(path):
            with open(path, encoding="utf-8") as f:
                return json.load(f)
        return {
            "protocolVersion": "0.3.0",
            "name": fallback_name,
            "description": "Echo agent",
            "url": "/",
            "version": "dev",
            "capabilities": {"streaming": False},
            "defaultInputModes": ["text/plain"],
            "defaultOutputModes": ["text/plain"],
            "skills": [],
        }


class EchoAgent:
    """Handles A2A JSON-RPC requests."""

    def __init__(self, llm: Callable[[str], str], id_factory: Callable[[], str] = lambda: str(uuid.uuid4())) -> None:
        self._llm = llm
        self._new_id = id_factory

    def handle(self, request: Any) -> Dict[str, Any]:
        """Returns a JSON-RPC response object for a parsed request."""
        req_id = request.get("id") if isinstance(request, dict) else None
        try:
            if not isinstance(request, dict) or request.get("jsonrpc") != "2.0" or "method" not in request:
                raise RpcError(INVALID_REQUEST, "not a JSON-RPC 2.0 request")
            if request["method"] != "message/send":
                raise RpcError(METHOD_NOT_FOUND, f"method {request['method']!r} not supported")
            return {"jsonrpc": "2.0", "id": req_id, "result": self._send(request.get("params") or {})}
        except RpcError as err:
            return {"jsonrpc": "2.0", "id": req_id, "error": {"code": err.code, "message": err.message}}

    def _send(self, params: Dict[str, Any]) -> Dict[str, Any]:
        message = params.get("message")
        if not isinstance(message, dict):
            raise RpcError(INVALID_PARAMS, "params.message is required")
        text = " ".join(
            p.get("text", "") for p in message.get("parts") or [] if isinstance(p, dict) and p.get("kind") == "text"
        ).strip()
        if not text:
            raise RpcError(INVALID_PARAMS, "message has no text parts")
        reply = self._llm(text)
        return {
            "kind": "message",
            "role": "agent",
            "messageId": self._new_id(),
            "contextId": message.get("contextId") or self._new_id(),
            "parts": [{"kind": "text", "text": reply}],
        }


class Handler(BaseHTTPRequestHandler):
    agent: EchoAgent
    card: Dict[str, Any]
    server_version = "agentory-echo-agent/1"

    def do_GET(self) -> None:  # noqa: N802
        path = self.path.split("?")[0]
        if path in CARD_PATHS:
            self._send(200, self.card)
        elif path == "/healthz":
            self._send(200, {"status": "ok"})
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path.split("?")[0] not in ("/", ""):
            self._send(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length") or 0)
        try:
            request = json.loads(self.rfile.read(length) or b"null")
        except json.JSONDecodeError:
            self._send(200, {"jsonrpc": "2.0", "id": None, "error": {"code": PARSE_ERROR, "message": "invalid JSON"}})
            return
        self._send(200, self.agent.handle(request))

    def log_message(self, fmt: str, *args: Any) -> None:
        print(json.dumps({"msg": fmt % args}), flush=True)

    def _send(self, status: int, payload: Dict[str, Any]) -> None:
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def build_server(agent: EchoAgent, card: Dict[str, Any], host: str = "0.0.0.0", port: int = 8080) -> ThreadingHTTPServer:
    handler = type("BoundHandler", (Handler,), {"agent": agent, "card": card})
    return ThreadingHTTPServer((host, port), handler)


def main() -> None:
    env = os.environ
    llm = LLMClient(env.get("AGENTORY_LLM_BASE_URL", ""), env.get("AGENTORY_LLM_API_KEY", ""),
                    env.get("AGENTORY_MODEL", "mock/mock-model"))
    card = AgentCard.load(env.get("AGENTORY_AGENT_CARD_PATH"), env.get("AGENTORY_AGENT", "echo-agent"))
    port = int(env.get("PORT", "8080"))
    server = build_server(EchoAgent(llm.complete), card, port=port)
    print(json.dumps({"msg": f"echo agent {card.get('name')} listening on :{port}"}), flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
