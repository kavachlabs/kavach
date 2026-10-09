import base64
import json
import os
import subprocess
import sys
import unittest
from pathlib import Path

PKG_ROOT = Path(__file__).resolve().parent.parent
EXAMPLE = PKG_ROOT / "examples" / "ledger"

NULL_AMOUNT = base64.b64encode(b'{"id":"e1","type":"deposit","account":"a","amount":null}').decode()


def host(*args: str) -> list[dict]:
    msgs = [
        {"t": "hello", "protocol": 1, "service": "ledger", "start": "genesis"},
        {"t": "step", "seq": "0", "source": "f", "position": "1", "data": NULL_AMOUNT},
        {"t": "end"},
    ]
    p = subprocess.run(
        [sys.executable, "-m", "ledger", *args, "kavach-host"],
        input="".join(json.dumps(m) + "\n" for m in msgs), capture_output=True, text=True,
        cwd=EXAMPLE, env={**os.environ, "PYTHONPATH": str(PKG_ROOT)}, timeout=30,
    )
    assert p.returncode == 0, p.stderr
    return [json.loads(l) for l in p.stdout.splitlines()]


class LedgerExample(unittest.TestCase):
    def test_buggy_build_panics_on_null_amount(self):
        done = host()[-1]
        self.assertEqual(done["outcome"], "panic")
        self.assertTrue(done["message"].startswith("TypeError: "))

    def test_fixed_build_rejects_it(self):
        out = host("--fix")
        self.assertEqual(out[-1], {"t": "done", "outcome": "ok"})
        self.assertEqual(out[1]["sink"], "ledger.rejections")


if __name__ == "__main__":
    unittest.main()
