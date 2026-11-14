"""Agentory declarative runtime: an agent made of instructions and skills.

The operator runs this image for `runtime: declarative` Agents. Everything comes
from what the operator mounts and injects; the agent's author writes no code.

  * AGENTORY_RUNTIME_CONFIG   JSON written by the operator: instructions, skills, limits
  * skills                    SKILL.md bundles mounted read-only (one directory each)
  * AGENTORY_AGENT_CARD_PATH  the generated Agent Card
  * AGENTORY_LLM_BASE_URL / AGENTORY_LLM_API_KEY / AGENTORY_MODEL  the LLM gateway,
    the agent's own virtual key, and the one model that key allows
  * OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES  tracing

It serves A2A JSON-RPC `message/send`: the system prompt is the instructions
plus the skills, conversations are kept per contextId, and every request is a
trace that continues the caller's `traceparent` and is passed on to the gateway.
When the Agent lists MCP servers (config "mcp": true), its tools come from
AGENTORY_MCP_URL, the agent's MCP endpoint on the agent gateway, which only
exposes the tools the Agent is allowed. Standard library only.
"""

import json
import os
import re
import secrets
import threading
import time
import urllib.error
import urllib.request
import uuid
from collections import OrderedDict
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, List, Optional, Tuple

CARD_PATHS = ("/.well-known/agent-card.json", "/.well-known/agent.json")
TRACEPARENT = re.compile(r"^00-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$")
FRONTMATTER = re.compile(r"^---\s*\n(.*?)\n---\s*\n?(.*)$", re.S)

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


# --------------------------------------------------------------------------- config

@dataclass
class Skill:
    name: str
    description: str
    body: str


@dataclass
class RuntimeConfig:
    instructions: str
    skills: List[Skill] = field(default_factory=list)
    max_history_messages: int = 40
    max_contexts: int = 1000
    mcp: bool = False

    @classmethod
    def load(cls, path: str, loader: "SkillLoader") -> "RuntimeConfig":
        with open(path, encoding="utf-8") as f:
            doc = json.load(f)
        instructions = str(doc.get("instructions") or "").strip()
        if not instructions:
            raise ValueError("runtime config has no instructions")
        skills = [loader.load(s["name"], s["path"]) for s in doc.get("skills") or []]
        return cls(
            instructions=instructions,
            skills=skills,
            max_history_messages=int(doc.get("maxHistoryMessages") or 40),
            max_contexts=int(doc.get("maxContexts") or 1000),
            mcp=bool(doc.get("mcp")),
        )


class SkillLoader:
    """Reads a skill bundle: SKILL.md with optional YAML-ish frontmatter
    (only `name:` and `description:` are used)."""

    max_body_chars = 20_000

    def load(self, name: str, directory: str) -> Skill:
        path = os.path.join(directory, "SKILL.md")
        with open(path, encoding="utf-8") as f:
            text = f.read()
        meta, body = {}, text
        match = FRONTMATTER.match(text)
        if match:
            body = match.group(2)
            for line in match.group(1).splitlines():
                key, _, value = line.partition(":")
                if key.strip() in ("name", "description"):
                    meta[key.strip()] = value.strip().strip("\"'")
        return Skill(
            name=meta.get("name") or name,
            description=meta.get("description") or "",
            body=body.strip()[: self.max_body_chars],
        )


class PromptBuilder:
    """Builds the system prompt from the instructions and the skills."""

    @staticmethod
    def system_prompt(cfg: RuntimeConfig) -> str:
        parts = [cfg.instructions]
        if cfg.skills:
            parts.append("You have these skills. Follow a skill's instructions when the task matches it.")
            for s in cfg.skills:
                header = f"## Skill: {s.name}"
                if s.description:
                    header += f"\n{s.description}"
                parts.append(f"{header}\n\n{s.body}")
        return "\n\n".join(parts)


# --------------------------------------------------------------------------- tracing

@dataclass
class SpanContext:
    trace_id: str
    span_id: str

    def traceparent(self) -> str:
        return f"00-{self.trace_id}-{self.span_id}-01"


SPAN_KIND_INTERNAL = 1
SPAN_KIND_SERVER = 2


class Tracer:
    """Minimal OpenTelemetry tracer exporting OTLP/HTTP JSON (no SDK needed).
    Export failures are ignored: tracing must never break a request."""

    def __init__(self, endpoint: str, service: str, resource_attrs: Dict[str, str],
                 send: Optional[Callable[[str, bytes], None]] = None, clock=time.time_ns) -> None:
        self.endpoint = endpoint.rstrip("/") + "/v1/traces" if endpoint else ""
        self.service = service
        self.resource_attrs = resource_attrs
        self._send = send or self._post
        self._clock = clock

    @staticmethod
    def parse_resource_attributes(value: str) -> Dict[str, str]:
        attrs = {}
        for pair in (value or "").split(","):
            key, sep, val = pair.partition("=")
            if sep and key.strip():
                attrs[key.strip()] = val.strip()
        return attrs

    @staticmethod
    def context_from(traceparent: Optional[str]) -> Optional[SpanContext]:
        match = TRACEPARENT.match((traceparent or "").strip())
        if not match or match.group(1) == "0" * 32:
            return None
        return SpanContext(trace_id=match.group(1), span_id=match.group(2))

    def start(self, name: str, parent: Optional[SpanContext], attributes: Dict[str, Any],
              kind: int = SPAN_KIND_SERVER) -> "Span":
        trace_id = parent.trace_id if parent else secrets.token_hex(16)
        return Span(self, name, SpanContext(trace_id, secrets.token_hex(8)),
                    parent.span_id if parent else "", attributes, self._clock(), kind)

    def export(self, span: "Span") -> None:
        if not self.endpoint:
            return
        attrs = dict(self.resource_attrs, **{"service.name": self.service})
        payload = {"resourceSpans": [{
            "resource": {"attributes": [_attr(k, v) for k, v in attrs.items()]},
            "scopeSpans": [{"scope": {"name": "agentory.declarative-runtime"}, "spans": [span.to_otlp()]}],
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


def _attr(key: str, value: Any) -> Dict[str, Any]:
    if isinstance(value, bool):
        return {"key": key, "value": {"boolValue": value}}
    if isinstance(value, int):
        return {"key": key, "value": {"intValue": str(value)}}
    return {"key": key, "value": {"stringValue": str(value)}}


class Span:
    def __init__(self, tracer: Tracer, name: str, context: SpanContext, parent_span_id: str,
                 attributes: Dict[str, Any], start_ns: int, kind: int = SPAN_KIND_SERVER) -> None:
        self.tracer = tracer
        self.kind = kind
        self.name = name
        self.context = context
        self.parent_span_id = parent_span_id
        self.attributes = dict(attributes)
        self.start_ns = start_ns
        self.end_ns = start_ns
        self.error: Optional[str] = None

    def end(self, error: Optional[str] = None) -> None:
        self.end_ns = self.tracer._clock()
        self.error = error
        self.tracer.export(self)

    def to_otlp(self) -> Dict[str, Any]:
        span = {
            "traceId": self.context.trace_id,
            "spanId": self.context.span_id,
            "name": self.name,
            "kind": self.kind,
            "startTimeUnixNano": str(self.start_ns),
            "endTimeUnixNano": str(self.end_ns),
            "attributes": [_attr(k, v) for k, v in self.attributes.items()],
            "status": {"code": 2, "message": self.error} if self.error else {"code": 1},
        }
        if self.parent_span_id:
            span["parentSpanId"] = self.parent_span_id
        return span


# --------------------------------------------------------------------------- model and tools

class ChatClient:
    """OpenAI-compatible chat client for the LLM gateway."""

    def __init__(self, base_url: str, api_key: str, model: str, timeout: float = 60.0,
                 post: Optional[Callable[[str, bytes, Dict[str, str], float], Dict[str, Any]]] = None) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.model = model
        self.timeout = timeout
        self._post = post or self._http_post

    def complete(self, messages: List[Dict[str, Any]], traceparent: str,
                 tools: Optional[List[Dict[str, Any]]] = None) -> Tuple[Dict[str, Any], Dict[str, Any]]:
        body: Dict[str, Any] = {"model": self.model, "messages": messages}
        if tools:
            body["tools"] = tools
        headers = {"Content-Type": "application/json", "Authorization": "Bearer " + self.api_key,
                   "traceparent": traceparent}
        data = self._post(self.base_url + "/chat/completions", json.dumps(body).encode(), headers, self.timeout)
        try:
            return data["choices"][0]["message"], data.get("usage") or {}
        except (KeyError, IndexError, TypeError) as err:
            raise RpcError(INTERNAL_ERROR, "unexpected LLM gateway response") from err

    @staticmethod
    def _http_post(url: str, data: bytes, headers: Dict[str, str], timeout: float) -> Dict[str, Any]:
        req = urllib.request.Request(url, data=data, method="POST", headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return json.load(resp)
        except urllib.error.HTTPError as err:
            raise RpcError(INTERNAL_ERROR, f"LLM gateway returned HTTP {err.code}") from err
        except (urllib.error.URLError, TimeoutError) as err:
            raise RpcError(INTERNAL_ERROR, f"LLM gateway unreachable: {err}") from err


class ToolProvider:
    """Tools the agent may call. The default has none."""

    def definitions(self) -> List[Dict[str, Any]]:
        return []

    def call(self, name: str, arguments: str, traceparent: str = "") -> str:
        raise RpcError(INTERNAL_ERROR, f"tool {name!r} is not available")


class MCPClient:
    """Minimal MCP client over Streamable HTTP (protocol 2025-06-18): one session,
    JSON or server-sent-event responses."""

    protocol_version = "2025-06-18"

    def __init__(self, url: str, timeout: float = 30.0,
                 post: Optional[Callable[[str, bytes, Dict[str, str], float], Tuple[int, Dict[str, str], str]]] = None) -> None:
        self.url = url
        self.timeout = timeout
        self._post = post or self._http_post
        self._session: Optional[str] = None
        self._initialized = False
        self._ids = 0
        self._lock = threading.Lock()

    def _request(self, method: str, params: Optional[Dict[str, Any]], traceparent: str, notify: bool = False) -> Any:
        body: Dict[str, Any] = {"jsonrpc": "2.0", "method": method}
        if params is not None:
            body["params"] = params
        if not notify:
            self._ids += 1
            body["id"] = self._ids
        headers = {"Content-Type": "application/json", "Accept": "application/json, text/event-stream",
                   "MCP-Protocol-Version": self.protocol_version}
        if self._session:
            headers["Mcp-Session-Id"] = self._session
        if traceparent:
            headers["traceparent"] = traceparent
        status, resp_headers, text = self._post(self.url, json.dumps(body).encode(), headers, self.timeout)
        session = {k.lower(): v for k, v in resp_headers.items()}.get("mcp-session-id")
        if session:
            self._session = session
        if notify:
            return None
        if status >= 400:
            raise RpcError(INTERNAL_ERROR, f"MCP {method} returned HTTP {status}: {text[:200]}")
        message = self._parse(text, resp_headers, body["id"])
        if "error" in message:
            raise RpcError(INTERNAL_ERROR, f"MCP {method}: {message['error'].get('message', 'error')}")
        return message.get("result") or {}

    @staticmethod
    def _parse(text: str, headers: Dict[str, str], request_id: int) -> Dict[str, Any]:
        ctype = {k.lower(): v for k, v in headers.items()}.get("content-type", "")
        if "text/event-stream" in ctype:
            for line in text.splitlines():
                if line.startswith("data:"):
                    try:
                        msg = json.loads(line[5:].strip())
                    except json.JSONDecodeError:
                        continue
                    if isinstance(msg, dict) and msg.get("id") == request_id:
                        return msg
            raise RpcError(INTERNAL_ERROR, "MCP response stream had no reply")
        try:
            return json.loads(text)
        except json.JSONDecodeError as err:
            raise RpcError(INTERNAL_ERROR, "MCP response is not JSON") from err

    def _ensure_session(self, traceparent: str) -> None:
        if self._initialized:
            return
        self._request("initialize", {"protocolVersion": self.protocol_version, "capabilities": {},
                                     "clientInfo": {"name": "agentory-declarative-runtime", "version": "1"}}, traceparent)
        self._request("notifications/initialized", None, traceparent, notify=True)
        self._initialized = True

    def list_tools(self, traceparent: str = "") -> List[Dict[str, Any]]:
        with self._lock:
            self._ensure_session(traceparent)
            return list(self._request("tools/list", {}, traceparent).get("tools") or [])

    def call_tool(self, name: str, arguments: Dict[str, Any], traceparent: str = "") -> Dict[str, Any]:
        with self._lock:
            self._ensure_session(traceparent)
            return self._request("tools/call", {"name": name, "arguments": arguments}, traceparent)

    def reset(self) -> None:
        """Forgets the session, e.g. after the gateway restarted."""
        with self._lock:
            self._session, self._initialized = None, False

    @staticmethod
    def _http_post(url: str, data: bytes, headers: Dict[str, str], timeout: float) -> Tuple[int, Dict[str, str], str]:
        req = urllib.request.Request(url, data=data, method="POST", headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return resp.status, dict(resp.headers), resp.read().decode()
        except urllib.error.HTTPError as err:
            return err.code, dict(err.headers or {}), err.read().decode(errors="replace")
        except (urllib.error.URLError, TimeoutError) as err:
            raise RpcError(INTERNAL_ERROR, f"MCP gateway unreachable: {err}") from err


class MCPToolProvider(ToolProvider):
    """The agent's MCP tools, as listed by the agent gateway (already filtered
    to the Agent's allow-list). Tool failures are returned to the model as
    text so it can recover, instead of failing the whole request."""

    def __init__(self, client: MCPClient) -> None:
        self.client = client
        self._tools: Optional[List[Dict[str, Any]]] = None

    def definitions(self) -> List[Dict[str, Any]]:
        if self._tools is None:
            try:
                self._tools = self.client.list_tools()
            except RpcError as err:
                print(json.dumps({"msg": "MCP tools unavailable", "error": err.message}), flush=True)
                self.client.reset()
                return []  # retried on the next request
        return [{"type": "function", "function": {
            "name": t["name"], "description": t.get("description", ""),
            "parameters": t.get("inputSchema") or {"type": "object", "properties": {}},
        }} for t in self._tools]

    def call(self, name: str, arguments: str, traceparent: str = "") -> str:
        try:
            args = json.loads(arguments or "{}")
        except json.JSONDecodeError:
            return f"Error: arguments for {name} are not valid JSON"
        try:
            result = self.client.call_tool(name, args if isinstance(args, dict) else {}, traceparent)
        except RpcError as err:
            self.client.reset()
            return "Error: " + err.message
        text = "\n".join(c.get("text", "") for c in result.get("content") or [] if c.get("type") == "text")
        return ("Error: " + text) if result.get("isError") else text


class ConversationStore:
    """Per-contextId message history, bounded in size and number of contexts."""

    def __init__(self, max_messages: int, max_contexts: int) -> None:
        self.max_messages = max_messages
        self.max_contexts = max_contexts
        self._data: "OrderedDict[str, List[Dict[str, Any]]]" = OrderedDict()
        self._lock = threading.Lock()

    def history(self, context_id: str) -> List[Dict[str, Any]]:
        with self._lock:
            msgs = self._data.get(context_id, [])
            if context_id in self._data:
                self._data.move_to_end(context_id)
            return list(msgs)

    def append(self, context_id: str, *messages: Dict[str, Any]) -> None:
        with self._lock:
            msgs = self._data.setdefault(context_id, [])
            msgs.extend(messages)
            del msgs[: max(0, len(msgs) - self.max_messages)]
            self._data.move_to_end(context_id)
            while len(self._data) > self.max_contexts:
                self._data.popitem(last=False)


# --------------------------------------------------------------------------- agent

class DeclarativeAgent:
    """Answers A2A message/send from the instructions, skills and history."""

    max_tool_rounds = 5

    def __init__(self, cfg: RuntimeConfig, chat: ChatClient, tracer: Tracer, tools: Optional[ToolProvider] = None,
                 store: Optional[ConversationStore] = None, agent_name: str = "agent",
                 id_factory: Callable[[], str] = lambda: str(uuid.uuid4())) -> None:
        self.cfg = cfg
        self.chat = chat
        self.tracer = tracer
        self.tools = tools or ToolProvider()
        self.store = store or ConversationStore(cfg.max_history_messages, cfg.max_contexts)
        self.agent_name = agent_name
        self.system_prompt = PromptBuilder.system_prompt(cfg)
        self._new_id = id_factory

    def handle(self, request: Any, traceparent: Optional[str] = None) -> Dict[str, Any]:
        req_id = request.get("id") if isinstance(request, dict) else None
        span = self.tracer.start("invoke_agent " + self.agent_name, Tracer.context_from(traceparent), {
            "gen_ai.operation.name": "invoke_agent",
            "gen_ai.agent.name": self.agent_name,
            "gen_ai.request.model": self.chat.model,
        })
        try:
            if not isinstance(request, dict) or request.get("jsonrpc") != "2.0" or "method" not in request:
                raise RpcError(INVALID_REQUEST, "not a JSON-RPC 2.0 request")
            span.attributes["a2a.method"] = request["method"]
            if request["method"] != "message/send":
                raise RpcError(METHOD_NOT_FOUND, f"method {request['method']!r} not supported")
            result = self._send(request.get("params") or {}, span)
            span.end()
            return {"jsonrpc": "2.0", "id": req_id, "result": result}
        except RpcError as err:
            span.end(error=err.message)
            return {"jsonrpc": "2.0", "id": req_id, "error": {"code": err.code, "message": err.message}}

    def _send(self, params: Dict[str, Any], span: Span) -> Dict[str, Any]:
        message = params.get("message")
        if not isinstance(message, dict):
            raise RpcError(INVALID_PARAMS, "params.message is required")
        text = " ".join(
            p.get("text", "") for p in message.get("parts") or [] if isinstance(p, dict) and p.get("kind") == "text"
        ).strip()
        if not text:
            raise RpcError(INVALID_PARAMS, "message has no text parts")
        context_id = message.get("contextId") or self._new_id()
        span.attributes["gen_ai.conversation.id"] = context_id

        user = {"role": "user", "content": text}
        messages = [{"role": "system", "content": self.system_prompt}, *self.store.history(context_id), user]
        reply, usage = self._run_model(messages, span)
        self.store.append(context_id, user, {"role": "assistant", "content": reply})
        span.attributes["gen_ai.usage.input_tokens"] = int(usage.get("prompt_tokens") or 0)
        span.attributes["gen_ai.usage.output_tokens"] = int(usage.get("completion_tokens") or 0)
        return {
            "kind": "message",
            "role": "agent",
            "messageId": self._new_id(),
            "contextId": context_id,
            "parts": [{"kind": "text", "text": reply}],
        }

    def _run_model(self, messages: List[Dict[str, Any]], span: Span) -> Tuple[str, Dict[str, Any]]:
        tools = self.tools.definitions()
        total = {"prompt_tokens": 0, "completion_tokens": 0}
        for _ in range(self.max_tool_rounds + 1):
            reply, usage = self.chat.complete(messages, span.context.traceparent(), tools)
            for k in total:
                total[k] += int(usage.get(k) or 0)
            calls = reply.get("tool_calls") or []
            if not calls or not tools:
                return str(reply.get("content") or ""), total
            messages = [*messages, reply]
            for call in calls:
                fn = call.get("function") or {}
                result = self._call_tool(fn.get("name", ""), fn.get("arguments") or "{}", call.get("id"), span)
                messages.append({"role": "tool", "tool_call_id": call.get("id"), "content": result})
        raise RpcError(INTERNAL_ERROR, f"no answer after {self.max_tool_rounds} tool rounds")

    def _call_tool(self, name: str, arguments: str, call_id: Optional[str], parent: Span) -> str:
        """Runs one tool call in its own execute_tool span (OTel GenAI conventions)."""
        span = self.tracer.start("execute_tool " + name, parent.context, {
            "gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": name,
            "gen_ai.tool.call.id": call_id or "",
        }, kind=SPAN_KIND_INTERNAL)
        result = self.tools.call(name, arguments, span.context.traceparent())
        # Tool providers report failures as "Error: ..." text for the model.
        span.end(error=result[:200] if result.startswith("Error:") else None)
        return result


# --------------------------------------------------------------------------- HTTP

class Handler(BaseHTTPRequestHandler):
    agent: DeclarativeAgent
    card: Dict[str, Any]
    server_version = "agentory-declarative-runtime/1"

    def do_GET(self) -> None:  # noqa: N802
        path = self.path.split("?")[0]
        if path in CARD_PATHS:
            self._send(200, self.card)
        elif path in ("/healthz", "/readyz"):
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
        self._send(200, self.agent.handle(request, self.headers.get("traceparent")))

    def log_message(self, fmt: str, *args: Any) -> None:
        print(json.dumps({"msg": fmt % args}), flush=True)

    def _send(self, status: int, payload: Dict[str, Any]) -> None:
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
    return {"protocolVersion": "0.3.0", "name": name, "description": "Agentory declarative agent", "url": "/",
            "version": "dev", "capabilities": {"streaming": False}, "defaultInputModes": ["text/plain"],
            "defaultOutputModes": ["text/plain"], "skills": []}


def build_server(agent: DeclarativeAgent, card: Dict[str, Any], host: str = "0.0.0.0",
                 port: int = 8080) -> ThreadingHTTPServer:
    handler = type("BoundHandler", (Handler,), {"agent": agent, "card": card})
    return ThreadingHTTPServer((host, port), handler)


def main() -> None:
    env = os.environ
    name = env.get("AGENTORY_AGENT", "agent")
    cfg = RuntimeConfig.load(env.get("AGENTORY_RUNTIME_CONFIG", "/etc/agentory/runtime/config.json"), SkillLoader())
    tracer = Tracer(
        endpoint="" if env.get("OTEL_SDK_DISABLED") == "true" else env.get("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
        service=env.get("OTEL_SERVICE_NAME", name),
        resource_attrs=Tracer.parse_resource_attributes(env.get("OTEL_RESOURCE_ATTRIBUTES", "")),
    )
    chat = ChatClient(env.get("AGENTORY_LLM_BASE_URL", ""), env.get("AGENTORY_LLM_API_KEY", ""),
                      env.get("AGENTORY_MODEL", "mock/mock-model"))
    tools: ToolProvider = ToolProvider()
    if cfg.mcp and env.get("AGENTORY_MCP_URL"):
        tools = MCPToolProvider(MCPClient(env["AGENTORY_MCP_URL"]))
    agent = DeclarativeAgent(cfg, chat, tracer, tools=tools, agent_name=name)
    port = int(env.get("PORT", "8080"))
    server = build_server(agent, load_card(env.get("AGENTORY_AGENT_CARD_PATH"), name), port=port)
    print(json.dumps({"msg": f"declarative agent {name} listening on :{port}",
                      "skills": [s.name for s in cfg.skills], "mcp": isinstance(tools, MCPToolProvider)}), flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
