"""Runs one SDK recorder case (spec/recorder/sdk/README.md) through this SDK.

    python -m kavach.conformance.recorder_case <case.json> [--fake-recorder PATH]

Starts the SDK's `Recorder` for the conformance handler with the fake recorder
as its recorder command, performs the case's actions, closes it, and reports
the fake's verdict. Exit status 0 if the case passes.
"""

from __future__ import annotations

import argparse
import base64
import json
import sys
import tempfile
from pathlib import Path

from kavach import GatewayError, Input, Recorder

from .handler import AnyGateway, Conformance
from .spec import spec_dir


class _Answers:
    """Serves one step's scripted answers, in order, per kind."""

    def __init__(self):
        self.q: dict[str, list] = {}

    def load(self, answers: dict) -> None:
        self.q = {k: list(v) for k, v in answers.items()}

    def pop(self, kind: str):
        try:
            return self.q[kind].pop(0)
        except (KeyError, IndexError):
            raise AssertionError(f"the handler read a {kind} the case has no answer for") from None

    def clock(self) -> int:
        return int(self.pop("clock"))

    def rand(self, n: int) -> bytes:
        data = base64.b64decode(self.pop("rand"))
        assert len(data) == n, f"case rand answer is {len(data)} bytes, handler asked for {n}"
        return data

    def gateway(self, request: bytes) -> bytes:
        a = self.pop("gateway")
        if "error" in a:
            raise GatewayError(a["error"])
        return base64.b64decode(a["response"])

    def config(self, key: str) -> bytes | None:
        a = self.pop("config")
        return None if a.get("unset") else base64.b64decode(a["value"])


def run_case(case_path: Path, fake_recorder: Path | None = None) -> dict:
    """Run a case; returns the fake recorder's result.json contents."""
    case = json.loads(Path(case_path).read_text())
    fake = fake_recorder or spec_dir() / "recorder" / "sdk" / "fake_recorder.py"
    handler = Conformance()
    if "snapshot" in case:
        handler.restore(case["snapshot"].encode("ascii"))
    ans = _Answers()
    flags = {k: v.encode() for k, v in case.get("flags", {}).items()}
    with tempfile.TemporaryDirectory(prefix="kavach-case-") as tmp:
        result_path = Path(tmp) / "result.json"
        rec = Recorder(
            handler,
            service=case["open"]["service"],
            start=case["open"]["start"],
            snapshots=case["open"]["snapshots"],
            recorder_command=[sys.executable, str(fake), str(case_path), str(result_path)],
            gateways=AnyGateway(ans.gateway),
            config=ans.config,
            config_source="case",
            flags=(lambda: flags) if flags else None,
            clock_ns=ans.clock,
            random_bytes=ans.rand,
            required=True,
        )
        for action in case["actions"]:
            if "step" in action:
                s = action["step"]
                ans.load(action.get("answers", {}))
                rec.step(Input(s["source"], s["position"], base64.b64decode(s["data"])))
            elif "flush" in action:
                rec.flush(durable=action["flush"].get("durable", False))
        rec.close()
        if not result_path.exists():
            return {"pass": False, "error": "the fake recorder wrote no result (did the SDK close it?)", "frames": []}
        return json.loads(result_path.read_text())


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("case", type=Path)
    ap.add_argument("--fake-recorder", type=Path)
    args = ap.parse_args(argv)
    result = run_case(args.case, args.fake_recorder)
    ok = bool(result.get("pass"))
    print(f"{'PASS' if ok else 'FAIL'}  {args.case.name}")
    if not ok:
        print(result.get("error"), file=sys.stderr)
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
