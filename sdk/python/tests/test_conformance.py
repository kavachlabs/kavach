"""Runs the shared contract: host transcripts and SDK recorder cases.

The contract lives in the repository's `spec/`; KAVACH_SPEC_DIR overrides it.
"""

import os
import subprocess
import sys
import unittest
from pathlib import Path

from kavach.conformance.recorder_case import run_case
from kavach.conformance.spec import spec_dir

PKG_ROOT = Path(__file__).resolve().parent.parent
SPEC = spec_dir()
CASES = sorted((SPEC / "recorder" / "sdk").glob("*.json"))


@unittest.skipUnless((SPEC / "host" / "run.py").exists(), f"no spec at {SPEC} (set KAVACH_SPEC_DIR)")
class HostTranscripts(unittest.TestCase):
    def test_all_transcripts(self):
        env = {**os.environ, "PYTHONPATH": str(PKG_ROOT)}
        p = subprocess.run(
            [sys.executable, str(SPEC / "host" / "run.py"), "--host", f"{sys.executable} -m kavach.conformance",
             "--pass-env", "PYTHONPATH"],
            capture_output=True, text=True, env=env, cwd=str(PKG_ROOT), timeout=120,
        )
        self.assertEqual(p.returncode, 0, p.stdout + p.stderr)
        self.assertIn("transcripts passed", p.stdout)


@unittest.skipUnless(CASES, f"no recorder cases at {SPEC} (set KAVACH_SPEC_DIR)")
class RecorderCases(unittest.TestCase):
    def test_cases_exist(self):
        self.assertGreaterEqual(len(CASES), 7)


def _make(path: Path):
    def test(self):
        result = run_case(path)
        self.assertTrue(result.get("pass"), f"{path.name}: {result.get('error')}")

    return test


for _p in CASES:
    setattr(RecorderCases, "test_" + _p.stem.replace("-", "_"), _make(_p))


if __name__ == "__main__":
    unittest.main()
