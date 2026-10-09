"""Kavach's demo service, in Python.

    python -m ledger          # the buggy build: crashes on "amount": null
    python -m ledger --fix    # the fixed build

Both take ``--in FILE`` (default events.jsonl, next to this package) and
``--fixtures DIR``. Run from examples/ledger with the SDK on PYTHONPATH:

    PYTHONPATH=../.. python -m ledger

The build is chosen by a command-line flag and not an environment variable on
purpose: environment variables are served from the journal on replay, so a
replay of the buggy build's fixture would otherwise run the buggy code.
Replay hosts are therefore ``python -m ledger`` (old) and ``python -m ledger
--fix`` (new).

If ``kavach-recorder`` is available (PATH or $KAVACH_RECORDER) the service
records through it; otherwise it says so loudly and runs unrecorded.
"""

from __future__ import annotations

import argparse
import logging
import os
import sys

import kavach
from kavach import Input, Recorder

from .handler import Ledger

# The host command has `kavach-host` appended; it must be able to see --fix.
FIX = "--fix" in sys.argv

kavach.maybe_host(lambda: Ledger(fix=FIX))


def main() -> int:
    here = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    ap = argparse.ArgumentParser(prog="ledger")
    ap.add_argument("--fix", action="store_true", help="run the fixed handler")
    ap.add_argument("--in", dest="path", default=os.path.join(here, "events.jsonl"), help="JSON-lines file of events")
    ap.add_argument("--fixtures", default="fixtures", help="directory for journals and crash fixtures")
    args = ap.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(message)s")

    def deliver(outs):
        for o in outs:
            print(f"{o.sink:<18} {o.data.decode()}")

    rec = Recorder(Ledger(fix=args.fix), service="ledger", dir=args.fixtures, deliver=deliver)
    try:
        with open(args.path, "rb") as f:
            for line_no, line in enumerate(f, 1):
                line = line.strip()
                if not line:
                    continue
                result = rec.step(Input("file:" + os.path.basename(args.path), str(line_no), line))
                if result.kind == "panic":
                    # Stop the service, as the Go demo does. The recorder has
                    # already marked the failure and cut a fixture.
                    rec.close()
                    result.raise_for_failure()
                elif not result.ok:
                    logging.error("ledger: line %d: %s: %s", line_no, result.kind, result.message)
    finally:
        rec.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
