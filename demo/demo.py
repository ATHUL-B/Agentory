#!/usr/bin/env python3
"""Laptop-side helper for the Agentory demo (demo/run.sh).

  ask    send an A2A message/send to an agent through the agent gateway and
         print the answer (fails if an --expect substring is missing)
  trace  fetch a trace from Jaeger and print it as a tree of agents, tool calls
         and model calls (fails if an --expect-service is missing)

Both talk to port-forwarded URLs that run.sh sets up. Standard library only.
"""

import argparse
import json
import sys
import time
import urllib.error
import urllib.request
import uuid
from dataclasses import dataclass, field
from typing import Any, Callable, Dict, List, Optional, Sequence, Tuple

# The gateway answers 503 for a few seconds after a new route is accepted.
RETRY_STATUSES = (502, 503, 504)
# Marks a span whose parent is not in the trace: the parent belongs to the caller
# or to the agent gateway, neither of which exports spans yet (backlog 6d).
VIA_GATEWAY = "⇢ "
# Bifrost's per-plugin hook spans: real, but noise in a demo.
HIDDEN_SPAN_PREFIXES = ("plugin.",)

Fetch = Callable[[str, Optional[bytes], Dict[str, str], float], Tuple[int, bytes]]


def http_fetch(url: str, data: Optional[bytes], headers: Dict[str, str], timeout: float) -> Tuple[int, bytes]:
    req = urllib.request.Request(url, data=data, method="POST" if data is not None else "GET", headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as err:
        return err.code, err.read()
    except (urllib.error.URLError, TimeoutError, ConnectionError) as err:
        return 0, str(err).encode()


class DemoError(Exception):
    """A demo step did not produce what it should have."""


# --------------------------------------------------------------------------- A2A

@dataclass
class Answer:
    text: str
    context_id: str
    metadata: Dict[str, Any]


class A2AClient:
    def __init__(self, gateway_url: str, fetch: Fetch = http_fetch, attempts: int = 20,
                 sleep: Callable[[float], None] = time.sleep, timeout: float = 120.0) -> None:
        self.gateway_url = gateway_url.rstrip("/")
        self._fetch = fetch
        self.attempts = attempts
        self._sleep = sleep
        self.timeout = timeout

    def ask(self, agent: str, text: str, context_id: Optional[str] = None, trace_id: Optional[str] = None) -> Answer:
        namespace, _, name = agent.partition("/")
        if not name:
            raise DemoError(f"agent must be <namespace>/<name>, got {agent!r}")
        message: Dict[str, Any] = {"role": "user", "messageId": str(uuid.uuid4()),
                                   "parts": [{"kind": "text", "text": text}]}
        if context_id:
            message["contextId"] = context_id
        body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "message/send",
                           "params": {"message": message}}).encode()
        headers = {"Content-Type": "application/json"}
        if trace_id:
            # A root context from "the caller" (this laptop): its span is not exported.
            headers["traceparent"] = f"00-{trace_id}-{uuid.uuid4().hex[:16]}-01"
        url = f"{self.gateway_url}/agents/{namespace}/{name}/"
        status, raw = 0, b""
        for attempt in range(self.attempts):
            status, raw = self._fetch(url, body, headers, self.timeout)
            if status not in RETRY_STATUSES and status != 0:
                break
            if attempt + 1 < self.attempts:
                self._sleep(1.0)
        if status != 200:
            raise DemoError(f"{agent}: HTTP {status or 'unreachable'}: {raw[:200].decode(errors='replace')}")
        reply = json.loads(raw)
        if "error" in reply:
            raise DemoError(f"{agent}: {reply['error'].get('message')}")
        result = reply.get("result") or {}
        text = "\n".join(p.get("text", "") for p in result.get("parts") or [] if p.get("kind") == "text")
        return Answer(text.strip(), result.get("contextId", ""), result.get("metadata") or {})


# --------------------------------------------------------------------------- traces

@dataclass
class SpanNode:
    service: str
    name: str
    span_id: str
    parent_id: str
    start_ns: int
    end_ns: int
    attributes: Dict[str, Any]
    error: bool
    children: List["SpanNode"] = field(default_factory=list)

    @property
    def duration_ms(self) -> int:
        return max(0, (self.end_ns - self.start_ns) // 1_000_000)

    @property
    def hidden(self) -> bool:
        return self.name.startswith(HIDDEN_SPAN_PREFIXES)

    def detail(self) -> str:
        a = self.attributes
        op = a.get("gen_ai.operation.name")
        bits: List[str] = []
        if op == "chat":
            if a.get("bifrost.virtual_key.name"):
                bits.append("key " + str(a["bifrost.virtual_key.name"]))
            if a.get("gen_ai.usage.input_tokens") is not None:
                bits.append(f"{a['gen_ai.usage.input_tokens']}→{a.get('gen_ai.usage.output_tokens', 0)} tokens")
            if a.get("gen_ai.response.finish_reason"):
                bits.append(str(a["gen_ai.response.finish_reason"]))
        elif op == "invoke_agent" and a.get("gen_ai.usage.input_tokens") is not None:
            bits.append(f"{a['gen_ai.usage.input_tokens']}→{a.get('gen_ai.usage.output_tokens', 0)} tokens total")
        if self.error:
            bits.append("ERROR")
        return " · ".join(bits)


class Trace:
    """A Jaeger v3 (OTLP JSON) trace, rendered as a tree across services."""

    def __init__(self, nodes: List[SpanNode]) -> None:
        self.nodes = nodes
        by_id = {n.span_id: n for n in nodes}
        self.roots: List[SpanNode] = []
        for n in sorted(nodes, key=lambda n: n.start_ns):
            parent = by_id.get(n.parent_id)
            (parent.children if parent else self.roots).append(n)

    @staticmethod
    def from_otlp(doc: Dict[str, Any]) -> "Trace":
        nodes = []
        for rs in (doc.get("result") or doc).get("resourceSpans") or []:
            resource = {a["key"]: _value(a["value"]) for a in (rs.get("resource") or {}).get("attributes") or []}
            service = str(resource.get("service.name", "?"))
            for ss in rs.get("scopeSpans") or []:
                for s in ss.get("spans") or []:
                    nodes.append(SpanNode(
                        service, s.get("name", ""), s.get("spanId", ""), s.get("parentSpanId", ""),
                        int(s.get("startTimeUnixNano") or 0), int(s.get("endTimeUnixNano") or 0),
                        {a["key"]: _value(a["value"]) for a in s.get("attributes") or []},
                        (s.get("status") or {}).get("code") in (2, "STATUS_CODE_ERROR")))
        return Trace(nodes)

    @property
    def services(self) -> List[str]:
        return sorted({n.service for n in self.nodes})

    def render(self) -> str:
        lines: List[str] = []
        for root in self.roots:
            first = VIA_GATEWAY if root.parent_id else ""
            self._render(root, first, " " * len(first), lines)
        hidden = sum(1 for n in self.nodes if n.hidden)
        if hidden:
            lines.append(f"(+{hidden} gateway plugin spans hidden)")
        if any(r.parent_id for r in self.roots):
            lines.append(f"({VIA_GATEWAY.strip()} parent span not exported: the caller's, or the agent gateway's in between "
                         "(backlog 6d); same trace, separate branch)")
        return "\n".join(lines)

    def _render(self, node: SpanNode, first: str, rest: str, lines: List[str]) -> None:
        detail = node.detail()
        lines.append(f"{first}{node.service:<20} {node.name}  {node.duration_ms}ms" + (f"  [{detail}]" if detail else ""))
        visible = [c for c in node.children if not c.hidden]
        for i, child in enumerate(visible):
            last = i == len(visible) - 1
            self._render(child, rest + ("└─ " if last else "├─ "), rest + ("   " if last else "│  "), lines)


def _value(v: Dict[str, Any]) -> Any:
    for key in ("stringValue", "intValue", "doubleValue", "boolValue"):
        if key in v:
            return v[key]
    return None


class JaegerClient:
    def __init__(self, url: str, fetch: Fetch = http_fetch, sleep: Callable[[float], None] = time.sleep) -> None:
        self.url = url.rstrip("/")
        self._fetch = fetch
        self._sleep = sleep

    def wait_for(self, trace_id: str, services: Sequence[str], timeout_s: int = 30) -> Trace:
        """Polls until every expected service has reported spans (exports are batched)."""
        trace = Trace([])
        for attempt in range(max(1, timeout_s)):
            status, raw = self._fetch(f"{self.url}/api/v3/traces/{trace_id}", None, {}, 10.0)
            if status == 200:
                trace = Trace.from_otlp(json.loads(raw))
                if trace.nodes and all(s in trace.services for s in services):
                    return trace
            if attempt + 1 < timeout_s:
                self._sleep(1.0)
        if not trace.nodes:
            raise DemoError(f"trace {trace_id} not found in Jaeger")
        missing = [s for s in services if s not in trace.services]
        raise DemoError(f"trace {trace_id}: no spans from {', '.join(missing)} (have: {', '.join(trace.services) or 'none'})")


# --------------------------------------------------------------------------- CLI

def main(argv: Optional[List[str]] = None, out=sys.stdout, fetch: Fetch = http_fetch,
         sleep: Callable[[float], None] = time.sleep) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    ask = sub.add_parser("ask", help="send an A2A message to an agent")
    ask.add_argument("--gateway", required=True, help="agent gateway URL, e.g. http://localhost:18080")
    ask.add_argument("--agent", required=True, help="<namespace>/<agent>")
    ask.add_argument("--text", required=True)
    ask.add_argument("--context", help="A2A contextId (same id = same conversation)")
    ask.add_argument("--trace", help="32-hex trace id to start the trace with")
    ask.add_argument("--expect", action="append", default=[],
                     help="substring the answer (or a specialist's finding) must contain")
    ask.add_argument("--show-findings", action="store_true", help="print metadata.findings (multi-agent replies)")
    tr = sub.add_parser("trace", help="print a trace from Jaeger as a tree")
    tr.add_argument("--jaeger", required=True, help="Jaeger query URL, e.g. http://localhost:16686")
    tr.add_argument("--id", required=True)
    tr.add_argument("--expect-service", action="append", default=[])
    tr.add_argument("--wait", type=int, default=30, help="seconds to wait for spans")
    args = parser.parse_args(argv)

    try:
        if args.command == "ask":
            answer = A2AClient(args.gateway, fetch, sleep=sleep).ask(args.agent, args.text, args.context, args.trace)
            if args.show_findings:
                for agent, finding in (answer.metadata.get("findings") or {}).items():
                    print(f"  {agent} found: {finding}", file=out)
            print(answer.text, file=out)
            # Findings count too: a multi-agent answer may summarise them away.
            haystack = "\n".join([answer.text, *map(str, (answer.metadata.get("findings") or {}).values())])
            missing = [e for e in args.expect if e not in haystack]
            if missing:
                raise DemoError(f"answer is missing {missing!r}")
        else:
            trace = JaegerClient(args.jaeger, fetch, sleep).wait_for(args.id, args.expect_service, args.wait)
            print(trace.render(), file=out)
    except DemoError as err:
        print(f"demo check failed: {err}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
