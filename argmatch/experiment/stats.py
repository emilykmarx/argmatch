#!/usr/bin/env python3
"""Print one tab-separated summary line for an experiment's result directory:
its name, timings (from timing.txt), whether argmatch was still checking at
the capture (its last check was under 30 s before), the number of
instrumented functions never checked (also written to unchecked.txt and
warned about on stderr), the number of raw and deduplicated rows,
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


def still_checking(last_check):
    """Return "yes" if the last check was under 30 s before the capture."""
    if not last_check or not last_check.rstrip("s").isdigit():
        return "?"
    return "yes" if int(last_check.rstrip("s")) < 30 else "no"


def unchecked(d):
    """Write the instrumented functions pod.log shows were never checked to
    unchecked.txt, warn about them on stderr, and return how many there
    are, or "?" if pod.log has no list of instrumented functions (argmatch
    built without ARGMATCH_STATS)."""
    instrumented, checked = set(), set()
    for line in open(os.path.join(d, "pod.log"), errors="replace"):
        f = line.split()
        if f and not f[0].startswith("argmatch-"):
            f = f[1:]  # a timestamp from kubectl logs --timestamps
        if len(f) >= 2 and f[0] == "argmatch-instrumented:" and not (len(f) == 3 and f[2] == "functions"):
            instrumented.add(f[1])
        elif len(f) >= 2 and f[0] == "argmatch-stats:":
            checked.add(f[1])
    if not instrumented:
        print(f"stats.py: {d}: no instrumented functions in pod.log (built without ARGMATCH_STATS?)", file=sys.stderr)
        return "?"
    missing = sorted(instrumented - checked)
    with open(os.path.join(d, "unchecked.txt"), "w") as out:
        out.writelines(fn + "\n" for fn in missing)
    if missing:
        print(f"stats.py: WARNING: {len(missing)} of {len(instrumented)} instrumented functions were never checked "
              f"(see {os.path.join(d, 'unchecked.txt')}): {', '.join(missing)}", file=sys.stderr)
    return len(missing)


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
        ("captured", result.get("captured", "?")),
        ("last_check", result.get("last_check", "?")),
        ("still_checking", still_checking(result.get("last_check"))),
        ("unchecked", unchecked(d)),
        # run_on_vm.sh's count, if any: parsing a multi-GB matches.csv takes
        # minutes and tens of GB of memory
        ("raw_rows", result.get("rows") or len(rows(os.path.join(d, "matches.csv")))),
        ("dedup_rows", len(rows(os.path.join(d, "deduped.csv")))),
        ("pairs", len(nt)),
        ("needles", len({r[0] for r in nt})),
        ("param_keys", len({r[1] for r in nt})),
        ("types", len({r[2] for r in nt})),
    ]
    print("\t".join(f"{k}={v}" for k, v in fields))


if __name__ == "__main__":
    main()
