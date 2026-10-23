"""Baseline agents adapted for rootless container engines.

Under rootless podman the agent's in-container user maps to a subordinate
host uid, so files it writes under /logs/agent (Claude Code session JSONL)
are unreadable by the host-side Harbor process. Harbor then loses token
usage and the trajectory, and skips secret scrubbing of those files. These
subclasses make the agent logs world-readable before Harbor reads them.

  harbor run ... -a baselines:ClaudeCodeReadable -m MiniMax-M3
"""

from typing import override

from harbor.agents.installed.claude_code import ClaudeCode
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext


class ClaudeCodeReadable(ClaudeCode):
    @staticmethod
    @override
    def name() -> str:
        return "claude-code"

    @override
    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        try:
            await super().run(instruction, environment, context)
        finally:
            await environment.exec(
                command=f"chmod -R a+rX {self.environment_logs_dir.as_posix()}",
                user="root",
            )
