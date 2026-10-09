"""Where the Kavach contract (`spec/`) lives.

Set ``KAVACH_SPEC_DIR`` to the repository's ``spec`` directory. The default is
the main checkout this SDK was developed against.
"""

import os
from pathlib import Path

# SPEC: the conformance files live outside this worktree until they are merged;
# after the merge, point KAVACH_SPEC_DIR at the repo's own spec/ (or change this).
DEFAULT_SPEC_DIR = "/Users/koustav/code/kavach-labs/kavach/spec"


def spec_dir() -> Path:
    return Path(os.environ.get("KAVACH_SPEC_DIR", DEFAULT_SPEC_DIR))
