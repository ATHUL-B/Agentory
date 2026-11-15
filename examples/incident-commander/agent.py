"""Incident commander: an example "bring your own container" multi-agent.

On an A2A `message/send` it:
  1. asks each specialist agent (SPECIALISTS, e.g. "payments-oncall") the same
     question over A2A, through the agent gateway (AGENTORY_A2A_URL);
  2. asks its own model, through the LLM gateway with its own virtual key, to
     turn the specialists' findings into an incident summary;
  3. answers with that summary.

Every hop carries W3C traceparent, and the agent exports its own span to the
OTel collector, so the caller, this agent, each specialist and every model call
land in one trace. It uses only what the operator injects, plus SPECIALISTS.
Standard library only, so the file can be copied as a starting point.
"""

import json
import os
import re
import secrets
import time
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, List, Optional, Tuple

CARD_PATHS = ("/.well-known/agent-card.json", "/.well-known/agent.json")
TRACEPARENT = re.compile(r"^00-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}$")

# JSON-RPC 2.0 error codes.
PARSE_ERROR = -32700
INVALID_REQUEST = -32600
METHOD_NOT_FOUND = -32601
INVALID_PARAMS = -32602
INTERNAL_ERROR = -32603

# The gateway answers 503 for a few seconds after a new route is accepted.
RETRY_STATUSES = (502, 503, 504)

SUMMARY_PROMPT = (
    "You are the incident commander. Using only the specialists' findings below, "
    "write a short incident summary: the likely cause, the evidence, and the next action.\n\n"
    "{findings}\n\nQuestion: {question}"
)

# Poster sends a JSON body and returns (status, parsed JSON or None).
Poster = Callable[[str, Dict[str, Any], Dict[str, str], float], Tuple[int, Any]]


class RpcError(Exception):
    def __init__(self, code: int, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def http_post_json(url: str, body: Dict[str, Any], headers: Dict[str, str], timeout: float) -> Tuple[int, Any]:
    req = urllib.request.Request(url, data=json.dumps(body).encode(), method="POST",
                                 headers=dict(headers, **{"Content-Type": "application/json"}))
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.load(resp)
    except urllib.error.HTTPError as err:
        return err.code, None
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as err:
        raise RpcError(INTERNAL_ERROR, f"POST {url} failed: {err}") from err


# --------------------------------------------------------------------------- tracing

@dataclass
class SpanContext:
    trace_id: str
    span_id: str

    def traceparent(self) -> str:
        return f"00-{self.trace_id}-{self.span_id}-01"

    @staticmethod
    def parse(value: Optional[str]) -> Optional["SpanContext"]:
        match = TRACEPARENT.match((value or "").strip())
        if not match or match.group(1) == "0" * 32:
            return None
        return SpanContext(match.group(1), match.group(2))


class Tracer:
    """Exports one OTLP/HTTP JSON span per request. Best effort: tracing never fails a request."""

    def __init__(self, endpoint: str, service: str, send: Optional[Callable[[str, bytes], None]] = None,
                 clock: Callable[[], int] = time.time_ns) -> None:
        self.endpoint = endpoint.rstrip("/") + "/v1/traces" if endpoint else ""
        self.service = service
        self._send = send or self._post
        self._clock = clock

    def start(self, parent: Optional[SpanContext]) -> Tuple[SpanContext, int]:
        trace_id = parent.trace_id if parent else secrets.token_hex(16)
        return SpanContext(trace_id, secrets.token_hex(8)), self._clock()

    def finish(self, name: str, ctx: SpanContext, parent: Optional[SpanContext], start_ns: int,
               attributes: Dict[str, str], error: Optional[str] = None) -> None:
        if not self.endpoint:
            return
        span = {
            "traceId": ctx.trace_id, "spanId": ctx.span_id, "name": name, "kind": 2,
            "startTimeUnixNano": str(start_ns), "endTimeUnixNano": str(self._clock()),
            "attributes": [{"key": k, "value": {"stringValue": v}} for k, v in attributes.items()],
            "status": {"code": 2, "message": error} if error else {"code": 1},
        }
        if parent:
            span["parentSpanId"] = parent.span_id
        payload = {"resourceSpans": [{
            "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": self.service}}]},
            "scopeSpans": [{"scope": {"name": "agentory.example.incident-commander"}, "spans": [span]}],
        }]}
        try:
            self._send(self.endpoint, json.dumps(payload).encode())
        except Exception:  # noqa: BLE001 - tracing is best effort
            pass

    @staticmethod
    def _post(url: str, data: bytes) -> None:
        req = urllib.request.Request(url, data=data, method="POST", headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=2):
            pass


# --------------------------------------------------------------------------- clients

class LLMClient:
    """OpenAI-compatible chat completions through the LLM gateway."""

    def __init__(self, base_url: str, api_key: str, model: str, post: Poster = http_post_json,
                 timeout: float = 60.0) -> None:
        self.url = base_url.rstrip("/") + "/chat/completions"
        self.api_key = api_key
        self.model = model
        self._post = post
        self.timeout = timeout

    def complete(self, prompt: str, traceparent: str) -> str:
        body = {"model": self.model, "messages": [{"role": "user", "content": prompt}]}
        status, data = self._post(self.url, body, {"Authorization": "Bearer " + self.api_key,
                                                   "traceparent": traceparent}, self.timeout)
        if status != 200:
            raise RpcError(INTERNAL_ERROR, f"LLM gateway returned HTTP {status}")
        try:
            return data["choices"][0]["message"]["content"] or ""
        except (KeyError, IndexError, TypeError) as err:
            raise RpcError(INTERNAL_ERROR, "unexpected LLM gateway response") from err


class A2AClient:
    """Calls other agents with A2A message/send through the agent gateway."""

    def __init__(self, gateway_url: str, namespace: str, post: Poster = http_post_json,
                 timeout: float = 90.0, attempts: int = 10, sleep: Callable[[float], None] = time.sleep) -> None:
        self.gateway_url = gateway_url.rstrip("/")
        self.namespace = namespace
        self._post = post
        self.timeout = timeout
        self.attempts = attempts
        self._sleep = sleep

    def url(self, agent: str) -> str:
        namespace, _, name = agent.rpartition("/")
        return f"{self.gateway_url}/agents/{namespace or self.namespace}/{name}/"

    def ask(self, agent: str, text: str, traceparent: str) -> str:
        body = {"jsonrpc": "2.0", "id": str(uuid.uuid4()), "method": "message/send", "params": {"message": {
            "role": "user", "messageId": str(uuid.uuid4()), "parts": [{"kind": "text", "text": text}]}}}
        status, data = 0, None
        for attempt in range(self.attempts):
            status, data = self._post(self.url(agent), body, {"traceparent": traceparent}, self.timeout)
            if status not in RETRY_STATUSES:
                break
            if attempt + 1 < self.attempts:
                self._sleep(1.0)
        if status != 200 or not isinstance(data, dict):
            raise RpcError(INTERNAL_ERROR, f"agent {agent} returned HTTP {status}")
        if "error" in data:
            raise RpcError(INTERNAL_ERROR, f"agent {agent}: {data['error'].get('message', 'error')}")
        parts = (data.get("result") or {}).get("parts") or []
        return "\n".join(p.get("text", "") for p in parts if p.get("kind") == "text").strip()


# --------------------------------------------------------------------------- agent

class IncidentCommander:
    """Fans a question out to specialist agents and summarises their findings."""

    def __init__(self, specialists: List[str], a2a: A2AClient, llm: LLMClient, tracer: Tracer,
                 name: str = "incident-commander", id_factory: Callable[[], str] = lambda: str(uuid.uuid4())) -> None:
        self.specialists = specialists
        self.a2a = a2a
        self.llm = llm
        self.tracer = tracer
        self.name = name
        self._new_id = id_factory

    def handle(self, request: Any, traceparent: Optional[str] = None) -> Dict[str, Any]:
        req_id = request.get("id") if isinstance(request, dict) else None
        try:
            if not isinstance(request, dict) or request.get("jsonrpc") != "2.0" or "method" not in request:
                raise RpcError(INVALID_REQUEST, "not a JSON-RPC 2.0 request")
            if request["method"] != "message/send":
                raise RpcError(METHOD_NOT_FOUND, f"method {request['method']!r} not supported")
            return {"jsonrpc": "2.0", "id": req_id,
                    "result": self._send(request.get("params") or {}, SpanContext.parse(traceparent))}
        except RpcError as err:
            return {"jsonrpc": "2.0", "id": req_id, "error": {"code": err.code, "message": err.message}}

    def _send(self, params: Dict[str, Any], parent: Optional[SpanContext]) -> Dict[str, Any]:
        message = params.get("message")
        if not isinstance(message, dict):
            raise RpcError(INVALID_PARAMS, "params.message is required")
        question = " ".join(p.get("text", "") for p in message.get("parts") or []
                            if isinstance(p, dict) and p.get("kind") == "text").strip()
        if not question:
            raise RpcError(INVALID_PARAMS, "message has no text parts")

        ctx, start = self.tracer.start(parent)
        error = None
        try:
            findings = [(s, self._ask(s, question, ctx)) for s in self.specialists]
            report = "\n".join(f"Findings from {s}: {answer}" for s, answer in findings)
            summary = self.llm.complete(SUMMARY_PROMPT.format(findings=report, question=question), ctx.traceparent())
        except RpcError as err:
            error = err.message
            raise
        finally:
            self.tracer.finish(f"{self.name} message/send", ctx, parent, start, {
                "a2a.method": "message/send", "agentory.specialists": ",".join(self.specialists)}, error)
        return {
            "kind": "message", "role": "agent", "messageId": self._new_id(),
            "contextId": message.get("contextId") or self._new_id(),
            "parts": [{"kind": "text", "text": summary}],
            "metadata": {"traceId": ctx.trace_id, "findings": dict(findings)},
        }

    def _ask(self, specialist: str, question: str, ctx: SpanContext) -> str:
        try:
            return self.a2a.ask(specialist, question, ctx.traceparent())
        except RpcError as err:
            # One unavailable specialist should not sink the whole incident summary.
            return f"(unavailable: {err.message})"


# --------------------------------------------------------------------------- HTTP

class Handler(BaseHTTPRequestHandler):
    agent: IncidentCommander
    card: Dict[str, Any]
    server_version = "agentory-incident-commander/1"

    def do_GET(self) -> None:  # noqa: N802
        path = self.path.split("?")[0]
        if path in CARD_PATHS:
            self._reply(200, self.card)
        elif path == "/healthz":
            self._reply(200, {"status": "ok"})
        else:
            self._reply(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if self.path.split("?")[0] not in ("/", ""):
            self._reply(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length") or 0)
        try:
            request = json.loads(self.rfile.read(length) or b"null")
        except json.JSONDecodeError:
            self._reply(200, {"jsonrpc": "2.0", "id": None, "error": {"code": PARSE_ERROR, "message": "invalid JSON"}})
            return
        self._reply(200, self.agent.handle(request, self.headers.get("traceparent")))

    def log_message(self, fmt: str, *args: Any) -> None:
        print(json.dumps({"msg": fmt % args}), flush=True)

    def _reply(self, status: int, payload: Dict[str, Any]) -> None:
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def load_card(path: Optional[str], name: str) -> Dict[str, Any]:
    if path and os.path.exists(path):
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    return {"protocolVersion": "0.3.0", "name": name, "description": "Incident commander", "url": "/",
            "version": "dev", "capabilities": {"streaming": False}, "defaultInputModes": ["text/plain"],
            "defaultOutputModes": ["text/plain"], "skills": []}


def build_server(agent: IncidentCommander, card: Dict[str, Any], host: str = "0.0.0.0",
                 port: int = 8080) -> ThreadingHTTPServer:
    handler = type("BoundHandler", (Handler,), {"agent": agent, "card": card})
    return ThreadingHTTPServer((host, port), handler)


def from_env(env: Dict[str, str]) -> IncidentCommander:
    name = env.get("AGENTORY_AGENT", "incident-commander")
    specialists = [s.strip() for s in env.get("SPECIALISTS", "").split(",") if s.strip()]
    if not specialists:
        raise SystemExit("SPECIALISTS must name at least one agent, e.g. SPECIALISTS=payments-oncall")
    tracer = Tracer("" if env.get("OTEL_SDK_DISABLED") == "true" else env.get("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
                    env.get("OTEL_SERVICE_NAME", name))
    return IncidentCommander(
        specialists,
        A2AClient(env.get("AGENTORY_A2A_URL", ""), env.get("AGENTORY_NAMESPACE", "default")),
        LLMClient(env.get("AGENTORY_LLM_BASE_URL", ""), env.get("AGENTORY_LLM_API_KEY", ""),
                  env.get("AGENTORY_MODEL", "mock/mock-model")),
        tracer, name=name)


def main() -> None:
    env = dict(os.environ)
    agent = from_env(env)
    card = load_card(env.get("AGENTORY_AGENT_CARD_PATH"), agent.name)
    port = int(env.get("PORT", "8080"))
    print(json.dumps({"msg": f"{agent.name} listening on :{port}", "specialists": agent.specialists}), flush=True)
    build_server(agent, card, port=port).serve_forever()


if __name__ == "__main__":
    main()
