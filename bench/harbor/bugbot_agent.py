"""Harbor installed-agent wrapper for bugbot.

Copies a prebuilt static bugbot binary into the task container, writes a
bugbot.yaml that points every role at the model given with `-m`, scans
/testbed restricted to the task's scope globs (/task/scope.txt), and exports
open findings to /logs/agent/findings.json for the verifier.

The task instruction is not forwarded: bugbot hunts unprompted; the scope
file carries the only task-specific input.

Run from the repo root:
  PYTHONPATH=bench/harbor harbor run -p <dataset> \
      -a bugbot_agent:BugbotAgent -m minimax/MiniMax-M3
"""

import json
import re
import shlex
import tempfile
from pathlib import Path
from typing import Literal, override

from pydantic import Field

from harbor.agents.installed.base import BaseInstalledAgent
from harbor.agents.model_connection import ModelConnectionSpec
from harbor.agents.options import InstalledAgentOptions
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

HERE = Path(__file__).resolve().parent
REMOTE_BIN = "/installed-agent/bugbot"
REMOTE_RUN = "/installed-agent/run"
LOGS = "/logs/agent"

# Provider prefix of `-m provider/model` -> (base_url, api key env var).
PROVIDERS = {
    "minimax": ("https://api.minimax.io/v1", "MINIMAX_API_KEY"),
}

SPEND_RE = re.compile(r"^Spend: input=(\d+) output=(\d+)", re.M)
CACHE_RE = re.compile(r"^Cache: read=(\d+)", re.M)
# `bugbot status` reads status.json, which the scan keeps current, so it
# still reports the run's spend after a deadline kill.
STATUS_SPEND_RE = re.compile(r"run spend:\s+in=(\d+) out=(\d+) total=\d+ tokens(?: \(cached (\d+)\))?")


class BugbotOptions(InstalledAgentOptions):
    binary: str = Field(
        default=str(HERE / "bin" / "bugbot"),
        description="Host path of a linux/amd64 static bugbot binary.",
    )
    base_url: str | None = Field(default=None, description="Override the provider base URL.")
    api_key_env: str | None = Field(default=None, description="Override the API key env var name.")
    structured_output: bool = Field(default=True, description="Provider schema-constrained output.")
    # 3M degraded every lens on a 19-file django scope (9.2M spent, 7.6M of it
    # cache reads); a benchmark should measure the pipeline, not the budget.
    per_cycle_tokens: int = Field(default=15_000_000, description="budgets.per_cycle_tokens.")
    concurrency: int = Field(default=4, description="scan --concurrency.")
    # Kill the scan before Harbor's agent timeout (task.toml: 7200s) so the
    # findings and spend recorded so far are still exported.
    scan_timeout_sec: int = Field(default=6900, description="Wall-clock deadline for the scan.")
    # Finder-exec bench mode. The SWE-bench task images run under rootless
    # podman and hold no nested podman or bwrap, so bugbot cannot isolate
    # finder probes inside them; scan.finder_exec is the arm under test.
    finder_exec: Literal["off", "auto", "on"] = Field(
        default="on",
        description="scan.finder_exec written to bugbot.yaml (the A/B arm).",
    )
    sandbox_backend: str = Field(
        default="unsandboxed",
        description="sandbox.backend written to bugbot.yaml.",
    )
    conda_env: str = Field(
        default="",
        description="Activate this /opt/miniconda3 env before the scan so run_code sees the project env (SWE-bench: testbed).",
    )


class BugbotAgent(BaseInstalledAgent):
    MODEL_CONNECTION = ModelConnectionSpec(passthrough=True)
    options_model = BugbotOptions

    @staticmethod
    @override
    def name() -> str:
        return "bugbot"

    @override
    def version(self) -> str | None:
        # bugbot has no --version; build.sh records the commit it built.
        stamp = Path(self._opts().binary).with_name("bugbot.version")
        return stamp.read_text().strip() if stamp.is_file() else None

    @override
    def get_version_command(self) -> str | None:
        return None

    def _opts(self) -> BugbotOptions:
        assert isinstance(self.options, BugbotOptions)
        return self.options

    def _provider(self) -> tuple[str, str, str]:
        """Return (model, base_url, api_key_env) from `-m provider/model`."""
        if not self.model_name or "/" not in self.model_name:
            raise ValueError("bugbot agent needs -m provider/model, e.g. minimax/MiniMax-M3")
        provider, model = self.model_name.split("/", 1)
        base_url, key_env = PROVIDERS.get(provider, (None, None))
        o = self._opts()
        base_url = o.base_url or base_url
        key_env = o.api_key_env or key_env
        if not base_url or not key_env:
            raise ValueError(
                f"unknown provider {provider!r}: pass --ak base_url=... --ak api_key_env=..."
            )
        return model, base_url, key_env

    def _bugbot_yaml(self, model: str, base_url: str, key_env: str, globs: list[str]) -> str:
        # JSON is valid YAML, and bugbot's loader rejects unknown keys, so
        # emit exactly the keys we mean. scan.include replaces the default
        # "**/*"; the default excludes stay in force.
        role = {"provider": "bench", "model": model}
        cfg = {
            "providers": {
                "bench": {
                    "type": "openai-compatible",
                    "base_url": base_url,
                    "api_key_env": key_env,
                    "structured_output": self._opts().structured_output,
                }
            },
            "roles": {"finder": role, "verifier": role, "reproducer": role},
            "budgets": {"per_cycle_tokens": self._opts().per_cycle_tokens, "per_day_tokens": 0},
            "scan": {"include": globs, "finder_exec": self._opts().finder_exec},
            "sandbox": {"backend": self._opts().sandbox_backend},
            "storage": {"path": f"{REMOTE_RUN}/state.db"},
            "report": {"dir": f"{REMOTE_RUN}/reports"},
        }
        return json.dumps(cfg, indent=2)

    @override
    async def install(self, environment: BaseEnvironment) -> None:
        binary = Path(self._opts().binary)
        if not binary.is_file():
            raise FileNotFoundError(
                f"bugbot binary not found at {binary}; build it with bench/harbor/build.sh"
            )
        await environment.upload_file(binary, REMOTE_BIN)
        await self.exec_as_root(environment, command=f"chmod 755 {REMOTE_BIN}")

    @override
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        model, base_url, key_env = self._provider()
        api_key = self._get_env(key_env)
        if not api_key:
            raise ValueError(f"{key_env} is not set on the host")

        await self.exec_as_root(environment, command=f"mkdir -p {REMOTE_RUN}")
        with tempfile.TemporaryDirectory() as tmp:
            # Download rather than `cat`: exec stdout can carry engine noise
            # (podman's compose-provider banner), which would become globs.
            scope_file = Path(tmp) / "scope.txt"
            try:
                await environment.download_file("/task/scope.txt", scope_file)
            except Exception as exc:
                raise RuntimeError("task has no /task/scope.txt; bugbot needs a scan scope") from exc
            globs = [g.strip() for g in scope_file.read_text().splitlines() if g.strip()]
            if not globs:
                raise RuntimeError("/task/scope.txt is empty; bugbot needs a scan scope")

            local = Path(tmp) / "bugbot.yaml"
            local.write_text(self._bugbot_yaml(model, base_url, key_env, globs))
            await environment.upload_file(local, f"{REMOTE_RUN}/bugbot.yaml")

        bb = f"{REMOTE_BIN} --config {REMOTE_RUN}/bugbot.yaml"
        script = (
            f"cd {REMOTE_RUN} && cp bugbot.yaml {LOGS}/bugbot.yaml && "
            + (f". /opt/miniconda3/bin/activate {shlex.quote(self._opts().conda_env)}; " if self._opts().conda_env else "")
            + "git config --global --add safe.directory /testbed; "
            f"git -C /testbed ls-files | wc -l > {LOGS}/tracked_files; "
            f"timeout -k 60 {self._opts().scan_timeout_sec} "
            f"{bb} scan --target /testbed --force --concurrency {self._opts().concurrency} "
            f"> {LOGS}/scan.log 2>&1; echo $? > {LOGS}/scan.exit; "
            f"{bb} status > {LOGS}/status.txt 2>&1; "
            f"{bb} report list --json > {LOGS}/findings.json 2> {LOGS}/report.err; "
            # bugbot prints JSON null for an empty list; normalize so the
            # verifier sees a valid empty result rather than a missing one.
            f"grep -qx null {LOGS}/findings.json && echo '[]' > {LOGS}/findings.json; "
            # state.db + transcripts let a run be autopsied after the container is gone.
            f"cp {REMOTE_RUN}/state.db {LOGS}/ 2>/dev/null; "
            f"tar -czf {LOGS}/transcripts.tgz -C {REMOTE_RUN}/.bugbot transcripts 2>/dev/null; "
            f"cat {LOGS}/scan.exit"
        )
        await self.exec_as_agent(
            environment, command=script, env={key_env: api_key}, cwd=REMOTE_RUN
        )

    @override
    def populate_context_post_run(self, context: AgentContext) -> None:
        log = self.logs_dir / "scan.log"
        status = self.logs_dir / "status.txt"
        text = log.read_text(errors="replace") if log.is_file() else ""
        if m := SPEND_RE.search(text):
            context.n_input_tokens = int(m.group(1))
            context.n_output_tokens = int(m.group(2))
            c = CACHE_RE.search(text)
            context.n_cache_tokens = int(c.group(1)) if c else 0
        elif status.is_file() and (m := STATUS_SPEND_RE.search(status.read_text(errors="replace"))):
            context.n_input_tokens = int(m.group(1))
            context.n_output_tokens = int(m.group(2))
            context.n_cache_tokens = int(m.group(3) or 0)
        exit_file = self.logs_dir / "scan.exit"
        meta = dict(context.metadata or {})
        if exit_file.is_file():
            meta["scan_exit"] = exit_file.read_text().strip()
        meta["finder_exec"] = self._opts().finder_exec
        meta["sandbox_backend"] = self._opts().sandbox_backend
        context.metadata = meta
