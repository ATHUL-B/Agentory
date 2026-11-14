"""Agentory harness runner for Claude Code: runs one Task, then exits.

The operator starts this as a Job per Task (JobBackend). It reads the task from
the environment, runs Claude Code headless against the LLM gateway with the
agent's own virtual key, and reports a compact JSON result through the
container's termination message (/dev/termination-log), which Kubernetes keeps
in the pod status. The operator reads it from there, so the pod needs no API
access.

Environment (set by the operator):
  AGENTORY_TASK, AGENTORY_TASK_MESSAGE      the task
  AGENTORY_MODEL                            provider/model the virtual key allows
  ANTHROPIC_BASE_URL, ANTHROPIC_API_KEY     LLM gateway and virtual key
Optional:
  AGENTORY_HARNESS_ALLOWED_TOOLS            default "Read,Edit,Write,Bash"
  AGENTORY_HARNESS_MAX_TURNS                cap on agent turns
  AGENTORY_WORKSPACE                        default /workspace
"""

import json
import os
import secrets
import subprocess
import sys
from dataclasses import dataclass, field
from typing import Callable, Dict, List, Optional

TERMINATION_LOG = "/dev/termination-log"
# Kubernetes keeps at most 4096 bytes of a termination message.
MAX_RESULT_BYTES = 4000
MAX_ARTIFACTS = 20
DEFAULT_TOOLS = "Read,Edit,Write,Bash"


@dataclass
class TaskConfig:
    task: str
    message: str
    model: str
    workspace: str = "/workspace"
    allowed_tools: str = DEFAULT_TOOLS
    max_turns: Optional[int] = None

    @classmethod
    def from_env(cls, env: Dict[str, str]) -> "TaskConfig":
        message = env.get("AGENTORY_TASK_MESSAGE", "").strip()
        if not message:
            raise ValueError("AGENTORY_TASK_MESSAGE is empty")
        model = env.get("AGENTORY_MODEL", "").strip()
        if not model:
            raise ValueError("AGENTORY_MODEL is empty")
        turns = env.get("AGENTORY_HARNESS_MAX_TURNS", "").strip()
        return cls(
            task=env.get("AGENTORY_TASK", "task"),
            message=message,
            model=model,
            workspace=env.get("AGENTORY_WORKSPACE", "/workspace"),
            allowed_tools=env.get("AGENTORY_HARNESS_ALLOWED_TOOLS", DEFAULT_TOOLS),
            max_turns=int(turns) if turns else None,
        )


@dataclass
class TaskResult:
    """What the operator copies into Task.status. Keep the keys in sync with
    operator/internal/runtime/job_backend.go (harnessResult)."""
    status: str  # "succeeded" | "failed"
    summary: str = ""
    trace_id: str = ""
    input_tokens: int = 0
    output_tokens: int = 0
    cost_usd: str = ""
    num_turns: int = 0
    artifacts: List[Dict[str, str]] = field(default_factory=list)

    def to_json(self, limit: int = MAX_RESULT_BYTES) -> str:
        """Serialises the result, trimming summary and artifacts to fit limit bytes."""
        doc = {
            "status": self.status,
            "summary": self.summary,
            "traceId": self.trace_id,
            "usage": {"inputTokens": self.input_tokens, "outputTokens": self.output_tokens, "costUsd": self.cost_usd},
            "numTurns": self.num_turns,
            "artifacts": self.artifacts[:MAX_ARTIFACTS],
        }
        data = json.dumps(doc)
        while len(data.encode()) > limit and doc["artifacts"]:
            doc["artifacts"] = doc["artifacts"][:-1]
            data = json.dumps(doc)
        if len(data.encode()) > limit:
            overflow = len(data.encode()) - limit + len(" [truncated]")
            doc["summary"] = doc["summary"][: max(0, len(doc["summary"]) - overflow)] + " [truncated]"
            data = json.dumps(doc)
        return data


class Tracing:
    """W3C trace context for the run, passed to the gateway as a header so its
    spans belong to the Task's trace."""

    def __init__(self, rand: Callable[[int], str] = secrets.token_hex) -> None:
        self.trace_id = rand(16)
        self.span_id = rand(8)

    def traceparent(self) -> str:
        return f"00-{self.trace_id}-{self.span_id}-01"


class Workspace:
    """A git working tree, so the changes Claude Code makes can be listed."""

    def __init__(self, path: str, run: Callable = subprocess.run) -> None:
        self.path = path
        self._run = run

    def _git(self, *args: str) -> subprocess.CompletedProcess:
        # safe.directory: volumes are often owned by another user than the harness,
        # and git refuses to work in such a tree otherwise ("dubious ownership").
        return self._run(["git", "-c", "safe.directory=*", "-C", self.path, *args],
                         capture_output=True, text=True, check=False)

    def prepare(self) -> None:
        os.makedirs(self.path, exist_ok=True)
        if self._git("rev-parse", "--is-inside-work-tree").returncode != 0:
            self._git("init", "-q")
        self._git("config", "user.email", "harness@agentory.dev")
        self._git("config", "user.name", "Agentory harness")
        self._git("commit", "-q", "--allow-empty", "-m", "agentory: task start")

    def changed_files(self) -> List[Dict[str, str]]:
        out = self._git("status", "--porcelain", "--untracked-files=all").stdout
        files = []
        for line in out.splitlines():
            path = line[3:].strip()
            if path:
                files.append({"type": "file", "ref": path})
        return files


class ClaudeCode:
    """Runs Claude Code headless and parses its JSON result."""

    def __init__(self, run: Callable = subprocess.run, binary: str = "claude") -> None:
        self._run = run
        self.binary = binary

    def command(self, cfg: TaskConfig) -> List[str]:
        cmd = [
            self.binary, "--bare", "-p", cfg.message,
            "--output-format", "json",
            "--model", cfg.model,
            # Unattended: edits allowed, listed tools pre-approved, nothing waits for a human.
            "--permission-mode", "acceptEdits",
            "--allowedTools", cfg.allowed_tools,
            "--permission-prompts", "none",
        ]
        if cfg.max_turns:
            cmd += ["--max-turns", str(cfg.max_turns)]
        return cmd

    @staticmethod
    def environment(cfg: TaskConfig, base: Dict[str, str], tracing: Tracing) -> Dict[str, str]:
        env = dict(base)
        env.update({
            # Every model Claude Code picks resolves to the agent's model, the only one its key allows.
            "ANTHROPIC_MODEL": cfg.model,
            "ANTHROPIC_DEFAULT_HAIKU_MODEL": cfg.model,
            "ANTHROPIC_DEFAULT_SONNET_MODEL": cfg.model,
            "ANTHROPIC_DEFAULT_OPUS_MODEL": cfg.model,
            "ANTHROPIC_CUSTOM_HEADERS": "traceparent: " + tracing.traceparent(),
            # Egress is locked down to the gateways anyway; don't try anything else.
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
            "HOME": base.get("HOME") or "/tmp/home",
        })
        return env

    def run(self, cfg: TaskConfig, env: Dict[str, str]) -> Dict:
        proc = self._run(self.command(cfg), cwd=cfg.workspace, env=env, capture_output=True, text=True, check=False)
        try:
            out = json.loads(proc.stdout)
        except (json.JSONDecodeError, TypeError):
            detail = (proc.stderr or proc.stdout or "").strip()[-1500:]
            return {"is_error": True, "result": f"claude exited {proc.returncode} without a JSON result: {detail}"}
        if proc.returncode != 0:
            out["is_error"] = True
        return out


class Runner:
    def __init__(self, claude: ClaudeCode, workspace_factory: Callable[[str], Workspace] = Workspace,
                 tracing: Optional[Tracing] = None) -> None:
        self.claude = claude
        self.workspace_factory = workspace_factory
        self.tracing = tracing or Tracing()

    def run(self, cfg: TaskConfig, env: Dict[str, str]) -> TaskResult:
        ws = self.workspace_factory(cfg.workspace)
        ws.prepare()
        out = self.claude.run(cfg, self.claude.environment(cfg, env, self.tracing))
        usage = out.get("usage") or {}
        cost = out.get("total_cost_usd")
        return TaskResult(
            status="failed" if out.get("is_error") else "succeeded",
            summary=str(out.get("result") or ""),
            trace_id=self.tracing.trace_id,
            input_tokens=int(usage.get("input_tokens") or 0),
            output_tokens=int(usage.get("output_tokens") or 0),
            cost_usd=f"{cost:.6f}" if isinstance(cost, (int, float)) else "",
            num_turns=int(out.get("num_turns") or 0),
            artifacts=ws.changed_files(),
        )


def write_result(result: TaskResult, path: str = TERMINATION_LOG) -> None:
    data = result.to_json()
    try:
        with open(path, "w", encoding="utf-8") as f:
            f.write(data)
    except OSError:
        pass  # outside Kubernetes; stdout below still carries it
    print(data, flush=True)


def main() -> int:
    try:
        cfg = TaskConfig.from_env(dict(os.environ))
    except ValueError as err:
        write_result(TaskResult(status="failed", summary=f"invalid task: {err}"))
        return 2
    result = Runner(ClaudeCode()).run(cfg, dict(os.environ))
    write_result(result)
    return 0 if result.status == "succeeded" else 1


if __name__ == "__main__":
    sys.exit(main())
