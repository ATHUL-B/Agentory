"""Demo on-call MCP server: deterministic incident data over MCP Streamable HTTP.

Stands in for real on-call integrations (deploy history, Kubernetes events,
metrics) so agents can be demonstrated and tested end to end without accounts.
Speaks the subset of MCP (protocol 2025-06-18) that tool use needs:
initialize, notifications/initialized, ping, tools/list and tools/call, over
POST /mcp with JSON responses. Standard library only.
"""

import json
import os
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, List, Optional

PROTOCOL_VERSION = "2025-06-18"
SERVER_INFO = {"name": "agentory-oncall-demo", "version": "1.0.0"}

# JSON-RPC 2.0 error codes.
INVALID_REQUEST = -32600
METHOD_NOT_FOUND = -32601
INVALID_PARAMS = -32602


class RpcError(Exception):
    def __init__(self, code: int, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


# The incident the demo tells: checkout broke right after a deploy.
INCIDENT_DATA = {
    "checkout": {
        "deploys": [
            {"version": "v2.31.0", "at": "12 minutes ago", "by": "alice", "change": "switch payment client to pooled connections"},
            {"version": "v2.30.4", "at": "2 days ago", "by": "bob", "change": "copy fix"},
        ],
        "events": [
            "Warning BackOff: Back-off restarting failed container checkout (x14)",
            "Warning Unhealthy: Readiness probe failed: connection pool exhausted",
        ],
        "error_rate": {"current": "18.4%", "baseline": "0.3%", "since": "11 minutes ago"},
    },
    "ledger": {
        "deploys": [{"version": "v1.9.2", "at": "6 days ago", "by": "carol", "change": "dependency bump"}],
        "events": [],
        "error_rate": {"current": "0.2%", "baseline": "0.2%", "since": ""},
    },
}


class Tool:
    def __init__(self, name: str, description: str, run: Callable[[Dict[str, Any]], str]) -> None:
        self.name = name
        self.description = description
        self.run = run

    def definition(self) -> Dict[str, Any]:
        return {
            "name": self.name,
            "description": self.description,
            "inputSchema": {
                "type": "object",
                "properties": {"service": {"type": "string", "description": "Service name, e.g. checkout"}},
                "required": ["service"],
            },
        }


class OnCallTools:
    """The demo tool set over INCIDENT_DATA."""

    def __init__(self, data: Optional[Dict[str, Any]] = None) -> None:
        self.data = data if data is not None else INCIDENT_DATA
        self.tools = {t.name: t for t in [
            Tool("get_recent_deploys", "Recent deploys of a service, newest first", self._deploys),
            Tool("get_pod_events", "Recent warning events for a service's pods", self._events),
            Tool("get_error_rate", "Current and baseline error rate of a service", self._errors),
        ]}

    def _service(self, args: Dict[str, Any]) -> Dict[str, Any]:
        service = str(args.get("service") or "").strip()
        if not service:
            raise RpcError(INVALID_PARAMS, "argument 'service' is required")
        if service not in self.data:
            raise RpcError(INVALID_PARAMS, f"unknown service {service!r}; known: {', '.join(sorted(self.data))}")
        return self.data[service]

    def _deploys(self, args: Dict[str, Any]) -> str:
        deploys = self._service(args)["deploys"]
        return "\n".join(f"{d['version']} {d['at']} by {d['by']}: {d['change']}" for d in deploys)

    def _events(self, args: Dict[str, Any]) -> str:
        events = self._service(args)["events"]
        return "\n".join(events) if events else "no warning events"

    def _errors(self, args: Dict[str, Any]) -> str:
        e = self._service(args)["error_rate"]
        text = f"error rate {e['current']} (baseline {e['baseline']})"
        return text + (f", elevated since {e['since']}" if e["since"] else "")


class MCPServer:
    """Handles MCP JSON-RPC messages."""

    def __init__(self, tools: Optional[OnCallTools] = None) -> None:
        self.tools = tools or OnCallTools()

    def handle(self, message: Any) -> Optional[Dict[str, Any]]:
        """Returns the response, or None for notifications."""
        if not isinstance(message, dict) or message.get("jsonrpc") != "2.0" or "method" not in message:
            return _error(None, INVALID_REQUEST, "not a JSON-RPC 2.0 message")
        if "id" not in message:
            return None  # notification, e.g. notifications/initialized
        req_id, method, params = message["id"], message["method"], message.get("params") or {}
        try:
            return {"jsonrpc": "2.0", "id": req_id, "result": self._dispatch(method, params)}
        except RpcError as err:
            return _error(req_id, err.code, err.message)

    def _dispatch(self, method: str, params: Dict[str, Any]) -> Dict[str, Any]:
        if method == "initialize":
            return {"protocolVersion": PROTOCOL_VERSION, "capabilities": {"tools": {"listChanged": False}},
                    "serverInfo": SERVER_INFO}
        if method == "ping":
            return {}
        if method == "tools/list":
            return {"tools": [t.definition() for t in self.tools.tools.values()]}
        if method == "tools/call":
            tool = self.tools.tools.get(params.get("name", ""))
            if tool is None:
                raise RpcError(INVALID_PARAMS, f"unknown tool {params.get('name')!r}")
            try:
                text = tool.run(params.get("arguments") or {})
            except RpcError as err:
                # Tool failures are results the model can read, not protocol errors.
                return {"content": [{"type": "text", "text": err.message}], "isError": True}
            return {"content": [{"type": "text", "text": text}], "isError": False}
        raise RpcError(METHOD_NOT_FOUND, f"method {method!r} not supported")


def _error(req_id: Any, code: int, message: str) -> Dict[str, Any]:
    return {"jsonrpc": "2.0", "id": req_id, "error": {"code": code, "message": message}}


class Handler(BaseHTTPRequestHandler):
    server_impl = MCPServer()
    server_version = "agentory-oncall-mcp/1"

    def do_GET(self) -> None:  # noqa: N802
        if self.path.split("?")[0] == "/healthz":
            self._json(200, {"status": "ok"})
        else:
            # No server-initiated stream: Streamable HTTP lets servers decline GET.
            self.send_response(405)
            self.send_header("Allow", "POST")
            self.end_headers()

    def do_DELETE(self) -> None:  # noqa: N802
        self.send_response(200)
        self.end_headers()

    def do_POST(self) -> None:  # noqa: N802
        if self.path.split("?")[0].rstrip("/") != "/mcp":
            self._json(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(length) or b"null")
        except json.JSONDecodeError:
            self._json(400, _error(None, -32700, "invalid JSON"))
            return
        if isinstance(body, list):
            responses: List[Dict[str, Any]] = [r for r in map(self.server_impl.handle, body) if r is not None]
            if responses:
                self._json(200, responses)
            else:
                self._accepted()
            return
        response = self.server_impl.handle(body)
        if response is None:
            self._accepted()
            return
        headers = {}
        if isinstance(body, dict) and body.get("method") == "initialize":
            headers["Mcp-Session-Id"] = uuid.uuid4().hex
        self._json(200, response, headers)

    def log_message(self, fmt: str, *args: Any) -> None:
        print(json.dumps({"msg": fmt % args}), flush=True)

    def _accepted(self) -> None:
        self.send_response(202)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def _json(self, status: int, payload: Any, headers: Optional[Dict[str, str]] = None) -> None:
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(data)


def build_server(host: str = "0.0.0.0", port: int = 8080) -> ThreadingHTTPServer:
    return ThreadingHTTPServer((host, port), Handler)


def main() -> None:
    port = int(os.environ.get("PORT", "8080"))
    print(json.dumps({"msg": f"on-call MCP demo listening on :{port}/mcp"}), flush=True)
    build_server(port=port).serve_forever()


if __name__ == "__main__":
    main()
