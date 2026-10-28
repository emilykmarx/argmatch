#!/usr/bin/env python3
"""Print one tab-separated summary line for an experiment's result directory:
its name, timings (from timing.txt), the number of raw and deduplicated rows,
and, in needle_types.csv, the number of rows (unique (needle, receiver type)
pairs) and of distinct needles, param keys and types.

Usage: stats.py RESULT_DIR
"""

import os
import re
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from dedup import read_csv  # noqa: E402


def rows(path):
    with open(path, "rb") as f:
        return read_csv(f.read())[1:]


def main():
    d = sys.argv[1]
    timing = open(os.path.join(d, "timing.txt")).read()
    result = dict(re.findall(r"(\w+)=(\S+)", timing.splitlines()[-1])) if "RESULT" in timing else {}
    nt = rows(os.path.join(d, "needle_types.csv"))
    fields = [
        ("name", os.path.basename(os.path.normpath(d))),
        ("build", result.get("build", "?")),
        ("load", result.get("load", "?")),
        ("ready", result.get("ready", "?")),
        ("alert", result.get("alert", "?")),
        ("raw_rows", len(rows(os.path.join(d, "matches.csv")))),
        ("dedup_rows", len(rows(os.path.join(d, "deduped.csv")))),
        ("pairs", len(nt)),
        ("needles", len({r[0] for r in nt})),
        ("param_keys", len({r[1] for r in nt})),
        ("types", len({r[2] for r in nt})),
    ]
    print("\t".join(f"{k}={v}" for k, v in fields))


if __name__ == "__main__":
    main()
