"""Deterministic sampling used by the problem-sourcing pass.

Two draws, both with fixed seeds and both over sorted id lists so that the
result does not depend on file or dict order:

  msb    uniform sample of 200 instance ids from the whole Multi-SWE-bench
         release (every *.jsonl under the dataset root).
         python3 -I sample.py msb <dir-with-jsonl> > msb_sample.txt

  audit  10% (rounded up) of the problems the first pass ruled out, which are
         then given the full verified pass to estimate the first-pass error
         rate. Input is a TSV of first-pass verdicts: id<TAB>cand|out<TAB>...
         python3 -I sample.py audit <first-pass.tsv> <seed>

Seeds used: MSB_SEED = 20261010; audit seeds 1 (DeepSWE), 2 (SWE-bench
Multilingual), 3 (Multi-SWE-bench).
"""
import json
import math
import os
import random
import sys

MSB_SEED = 20261010
MSB_N = 200


def msb_ids(root):
    ids = []
    for dirpath, _, files in os.walk(root):
        for f in files:
            if not f.endswith(".jsonl"):
                continue
            with open(os.path.join(dirpath, f)) as fh:
                for line in fh:
                    line = line.strip()
                    if line:
                        d = json.loads(line)
                        ids.append(d.get("instance_id") or f"{d['org']}__{d['repo']}-{d['number']}")
    return sorted(set(ids))


def audit(tsv, seed):
    outs = []
    with open(tsv) as fh:
        for line in fh:
            parts = line.rstrip("\n").split("\t")
            if len(parts) > 1 and parts[1] == "out":
                outs.append(parts[0])
    outs = sorted(outs)
    k = math.ceil(len(outs) * 0.10)
    return random.Random(seed).sample(outs, k)


if __name__ == "__main__":
    if sys.argv[1] == "msb":
        ids = msb_ids(sys.argv[2])
        print(f"# population {len(ids)} seed {MSB_SEED}", file=sys.stderr)
        for i in sorted(random.Random(MSB_SEED).sample(ids, MSB_N)):
            print(i)
    elif sys.argv[1] == "audit":
        for i in sorted(audit(sys.argv[2], int(sys.argv[3]))):
            print(i)
