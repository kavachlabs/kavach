"""Where the Kavach contract (`spec/`) lives.

``KAVACH_SPEC_DIR``, else the repository's own ``spec`` directory.
"""

import os
from pathlib import Path

DEFAULT_SPEC_DIR = Path(__file__).resolve().parents[4] / "spec"


def spec_dir() -> Path:
    return Path(os.environ.get("KAVACH_SPEC_DIR", DEFAULT_SPEC_DIR))
