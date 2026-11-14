"""Tests for the Claude Code harness runner. Run: python3 -m unittest discover -s harness/claude-code"""

import json
import os
import subprocess
import tempfile
import unittest

from runner import (
    DEFAULT_TOOLS,
    MAX_RESULT_BYTES,
    ClaudeCode,
    Runner,
    TaskConfig,
    TaskResult,
    Tracing,
    Workspace,
    write_result,
)

ENV = {"AGENTORY_TASK": "fix-1", "AGENTORY_TASK_MESSAGE": "Fix the test", "AGENTORY_MODEL": "mock/mock-model"}


class TaskConfigTest(unittest.TestCase):
    def test_from_env(self):
        cfg = TaskConfig.from_env(dict(ENV, AGENTORY_HARNESS_MAX_TURNS="5"))
        self.assertEqual((cfg.task, cfg.message, cfg.model, cfg.max_turns), ("fix-1", "Fix the test", "mock/mock-model", 5))
        self.assertEqual(cfg.allowed_tools, DEFAULT_TOOLS)
        self.assertEqual(cfg.workspace, "/workspace")

    def test_requires_message_and_model(self):
        for missing in ("AGENTORY_TASK_MESSAGE", "AGENTORY_MODEL"):
            env = dict(ENV)
            env[missing] = "  "
            with self.subTest(missing=missing), self.assertRaises(ValueError):
                TaskConfig.from_env(env)


class ClaudeCodeTest(unittest.TestCase):
    def test_command_is_unattended_and_structured(self):
        cmd = ClaudeCode().command(TaskConfig.from_env(dict(ENV, AGENTORY_HARNESS_MAX_TURNS="3")))
        self.assertEqual(cmd[:4], ["claude", "--bare", "-p", "Fix the test"])
        pairs = dict(zip(cmd[4::2], cmd[5::2]))
        self.assertEqual(pairs["--output-format"], "json")
        self.assertEqual(pairs["--model"], "mock/mock-model")
        self.assertEqual(pairs["--permission-mode"], "acceptEdits")
        self.assertEqual(pairs["--permission-prompts"], "none")
        self.assertEqual(pairs["--allowedTools"], DEFAULT_TOOLS)
        self.assertEqual(pairs["--max-turns"], "3")

    def test_mcp_tools_from_the_gateway(self):
        env = dict(ENV, AGENTORY_MCP_ENABLED="true", AGENTORY_MCP_URL="http://gw:8080/mcp/team/coder")
        cmd = ClaudeCode().command(TaskConfig.from_env(env))
        config = json.loads(cmd[cmd.index("--mcp-config") + 1])
        self.assertEqual(config, {"mcpServers": {"agentory": {"type": "http", "url": "http://gw:8080/mcp/team/coder"}}})
        self.assertEqual(cmd[cmd.index("--allowedTools") + 1], DEFAULT_TOOLS + ",mcp__agentory")

    def test_no_mcp_unless_enabled(self):
        env = dict(ENV, AGENTORY_MCP_ENABLED="false", AGENTORY_MCP_URL="http://gw:8080/mcp/team/coder")
        cmd = ClaudeCode().command(TaskConfig.from_env(env))
        self.assertNotIn("--mcp-config", cmd)
        self.assertEqual(cmd[cmd.index("--allowedTools") + 1], DEFAULT_TOOLS)

    def test_environment_pins_every_model_and_carries_trace(self):
        tracing = Tracing(rand=lambda n: "a" * (2 * n))
        env = ClaudeCode.environment(TaskConfig.from_env(ENV), {"ANTHROPIC_API_KEY": "sk-bf-x"}, tracing)
        for name in ("ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL",
                     "ANTHROPIC_DEFAULT_OPUS_MODEL"):
            self.assertEqual(env[name], "mock/mock-model", name)
        self.assertEqual(env["ANTHROPIC_CUSTOM_HEADERS"], "traceparent: 00-" + "a" * 32 + "-" + "a" * 16 + "-01")
        self.assertEqual(env["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"], "1")
        self.assertEqual(env["ANTHROPIC_API_KEY"], "sk-bf-x", "the agent's key passes through")

    def test_run_parses_json_and_flags_nonzero_exit(self):
        def fake(stdout, code):
            return lambda *a, **k: subprocess.CompletedProcess(a, code, stdout=stdout, stderr="boom")
        ok = ClaudeCode(run=fake(json.dumps({"result": "done", "is_error": False}), 0)).run(TaskConfig.from_env(ENV), {})
        self.assertEqual(ok, {"result": "done", "is_error": False})
        bad = ClaudeCode(run=fake(json.dumps({"result": "partial"}), 1)).run(TaskConfig.from_env(ENV), {})
        self.assertTrue(bad["is_error"])
        garbage = ClaudeCode(run=fake("not json", 2)).run(TaskConfig.from_env(ENV), {})
        self.assertTrue(garbage["is_error"])
        self.assertIn("exited 2", garbage["result"])
        self.assertIn("boom", garbage["result"])


class FakeWorkspace:
    def __init__(self, path):
        self.prepared = False

    def prepare(self):
        self.prepared = True

    def changed_files(self):
        return [{"type": "file", "ref": "hello.txt"}]


class FakeClaude(ClaudeCode):
    def __init__(self, out):
        super().__init__()
        self.out = out
        self.env = None

    def run(self, cfg, env):
        self.env = env
        return self.out


class RunnerTest(unittest.TestCase):
    def test_success_result(self):
        claude = FakeClaude({"result": "Created hello.txt", "is_error": False, "num_turns": 2,
                             "total_cost_usd": 0.00075, "usage": {"input_tokens": 75, "output_tokens": 15}})
        result = Runner(claude, FakeWorkspace, Tracing(rand=lambda n: "b" * (2 * n))).run(TaskConfig.from_env(ENV), {})
        self.assertEqual(result.status, "succeeded")
        self.assertEqual((result.input_tokens, result.output_tokens, result.cost_usd), (75, 15, "0.000750"))
        self.assertEqual(result.trace_id, "b" * 32)
        self.assertEqual(result.artifacts, [{"type": "file", "ref": "hello.txt"}])
        self.assertIn("ANTHROPIC_CUSTOM_HEADERS", claude.env)

    def test_failure_result(self):
        result = Runner(FakeClaude({"result": "model not found", "is_error": True}), FakeWorkspace).run(
            TaskConfig.from_env(ENV), {})
        self.assertEqual(result.status, "failed")
        self.assertEqual(result.cost_usd, "")


class TaskResultTest(unittest.TestCase):
    def test_fits_termination_message_limit(self):
        result = TaskResult(status="succeeded", summary="x" * 10_000,
                            artifacts=[{"type": "file", "ref": f"dir/file-{i}.txt"} for i in range(500)])
        data = result.to_json()
        self.assertLessEqual(len(data.encode()), MAX_RESULT_BYTES)
        doc = json.loads(data)
        self.assertTrue(doc["summary"].endswith("[truncated]"))
        self.assertEqual(doc["status"], "succeeded")

    def test_shape(self):
        doc = json.loads(TaskResult(status="failed", summary="s", trace_id="t", input_tokens=1, output_tokens=2,
                                    cost_usd="0.1", num_turns=3).to_json())
        self.assertEqual(doc, {"status": "failed", "summary": "s", "traceId": "t",
                               "usage": {"inputTokens": 1, "outputTokens": 2, "costUsd": "0.1"},
                               "numTurns": 3, "artifacts": []})

    def test_write_result_to_file(self):
        with tempfile.TemporaryDirectory() as d:
            path = os.path.join(d, "termination-log")
            write_result(TaskResult(status="succeeded", summary="ok"), path)
            with open(path) as f:
                self.assertEqual(json.load(f)["summary"], "ok")


class WorkspaceTest(unittest.TestCase):
    """Runs real git in a temp dir."""

    def test_lists_new_and_modified_files(self):
        with tempfile.TemporaryDirectory() as d:
            ws = Workspace(d)
            ws.prepare()
            self.assertEqual(ws.changed_files(), [])
            with open(os.path.join(d, "hello.txt"), "w") as f:
                f.write("hi")
            os.makedirs(os.path.join(d, "pkg"))
            with open(os.path.join(d, "pkg", "mod.py"), "w") as f:
                f.write("x = 1")
            self.assertEqual(sorted(a["ref"] for a in ws.changed_files()), ["hello.txt", "pkg/mod.py"])

    def test_prepare_is_safe_on_an_existing_repo(self):
        with tempfile.TemporaryDirectory() as d:
            Workspace(d).prepare()
            Workspace(d).prepare()
            self.assertTrue(os.path.isdir(os.path.join(d, ".git")))


if __name__ == "__main__":
    unittest.main()
