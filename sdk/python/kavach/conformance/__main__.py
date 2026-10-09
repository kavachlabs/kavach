"""``python -m kavach.conformance``: the conformance host (run with ``kavach-host`` appended)."""

import sys

import kavach

from .handler import AnyGateway, Conformance

kavach.maybe_host(Conformance, gateways=AnyGateway())
sys.stderr.write(
    "usage: python -m kavach.conformance kavach-host   (speaks the host protocol on stdin/stdout)\n"
    "       run it through spec/host/run.py, see the README\n"
)
sys.exit(2)
