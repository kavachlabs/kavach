"""Harbor agents for the Kavach benchmark. Load with
--agent-import-path bench.agent.harbor.agents:KavachOpenCode (PYTHONPATH=<repo root>)."""

from .opencode_live import KavachOpenCode

__all__ = ["KavachOpenCode"]
