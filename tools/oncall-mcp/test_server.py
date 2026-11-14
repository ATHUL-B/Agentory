"""Tests for the demo on-call MCP server. Run: python3 -m unittest discover -s tools/oncall-mcp"""

import json
import threading
import unittest
import urllib.error
import urllib.request

from server import INVALID_PARAMS, INVALID_REQUEST, METHOD_NOT_FOUND, MCPServer, build_server


def call(server, method, params=None, req_id=1):
    msg = {"jsonrpc": "2.0", "id": req_id, "method": method}
    if params is not None:
        msg["params"] = params
    return server.handle(msg)


class MCPServerTest(unittest.TestCase):
    def setUp(self):
        self.server = MCPServer()

    def test_initialize_and_list(self):
        init = call(self.server, "initialize", {"protocolVersion": "2025-06-18"})["result"]
        self.assertEqual(init["serverInfo"]["name"], "agentory-oncall-demo")
        self.assertIn("tools", init["capabilities"])
        tools = call(self.server, "tools/list", {})["result"]["tools"]
        self.assertEqual([t["name"] for t in tools], ["get_recent_deploys", "get_pod_events", "get_error_rate"])
        self.assertEqual(tools[0]["inputSchema"]["required"], ["service"])

    def test_tools_tell_the_incident(self):
        def text(name, service="checkout"):
            res = call(self.server, "tools/call", {"name": name, "arguments": {"service": service}})["result"]
            self.assertFalse(res["isError"])
            return res["content"][0]["text"]
        self.assertTrue(text("get_recent_deploys").startswith("v2.31.0 12 minutes ago by alice"))
        self.assertIn("connection pool exhausted", text("get_pod_events"))
        self.assertIn("18.4%", text("get_error_rate"))
        self.assertEqual(text("get_pod_events", "ledger"), "no warning events")

    def test_tool_errors_are_results(self):
        res = call(self.server, "tools/call", {"name": "get_error_rate", "arguments": {"service": "nope"}})["result"]
        self.assertTrue(res["isError"])
        self.assertIn("unknown service", res["content"][0]["text"])
        missing = call(self.server, "tools/call", {"name": "get_error_rate", "arguments": {}})["result"]
        self.assertTrue(missing["isError"])

    def test_protocol_errors(self):
        self.assertEqual(call(self.server, "tools/call", {"name": "rm_rf"})["error"]["code"], INVALID_PARAMS)
        self.assertEqual(call(self.server, "resources/list")["error"]["code"], METHOD_NOT_FOUND)
        self.assertEqual(self.server.handle({"id": 1})["error"]["code"], INVALID_REQUEST)
        self.assertIsNone(self.server.handle({"jsonrpc": "2.0", "method": "notifications/initialized"}))


class HttpTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.httpd = build_server("127.0.0.1", 0)
        cls.url = f"http://127.0.0.1:{cls.httpd.server_address[1]}"
        threading.Thread(target=cls.httpd.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.httpd.shutdown()
        cls.httpd.server_close()

    def post(self, body):
        req = urllib.request.Request(self.url + "/mcp", data=json.dumps(body).encode(), method="POST",
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=5) as r:
            return r.status, dict(r.headers), r.read()

    def test_initialize_returns_session(self):
        status, headers, body = self.post({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {}})
        self.assertEqual(status, 200)
        self.assertTrue(headers.get("Mcp-Session-Id"))
        self.assertEqual(json.loads(body)["id"], 1)

    def test_notification_is_accepted(self):
        status, _, body = self.post({"jsonrpc": "2.0", "method": "notifications/initialized"})
        self.assertEqual((status, body), (202, b""))

    def test_get_is_declined_and_health_works(self):
        with self.assertRaises(urllib.error.HTTPError) as ctx:
            urllib.request.urlopen(self.url + "/mcp", timeout=5)
        self.assertEqual(ctx.exception.code, 405)
        with urllib.request.urlopen(self.url + "/healthz", timeout=5) as r:
            self.assertEqual(r.status, 200)


if __name__ == "__main__":
    unittest.main()
