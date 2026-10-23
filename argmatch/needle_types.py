#!/usr/bin/env python3
"""Summarize argmatch output by needle and first type.

Usage: needle_types.py --input IN.csv --param-keys PARAM_KEYS --output OUT.csv

The input is argmatch's CSV, usually after dedup.py. Rows are grouped by
needle and first types column (the parameter's own type, or the closure or
interface step it starts with), and each group becomes one output row, in the
order in which the groups first appear. The output columns are:

  needle       the needle
  param_keys   the config keys the needle stands for, from the param keys file
  type         the first types column
  expressions  the group's distinct expressions, comma-separated, in order of
               first appearance, summarized as described below
  functions    the group's distinct functions, comma-separated, in order of
               first appearance

Expressions are summarized before they're compared, so that ones that differ
only in incidental details are listed once:

- Map positions become <key N>, as in dedup.py.
- Integers of BIG_DIGITS or more digits outside quoted strings become <X>,
  e.g. collectorsByID[2732491404422401902] -> collectorsByID[<X>]. These are
  usually hashes or IDs used as map keys, which differ between entries and
  runs.
- A run of two or more consecutive copies of the same sequence of steps
  becomes (steps)+, e.g. t.watcher.writer.watcher.writer.storeClient ->
  t(.watcher.writer)+.storeClient. These come from cycles in the program's
  data, followed until -depth ran out. A sequence counts only if it contains a
  field (.name), so a slice of slices (a[0][0]) is left alone. An expression
  that has just one copy where a listed one has (steps)+ is dropped, e.g.
  t.watcher.writer.storeClient when t(.watcher.writer)+.storeClient is listed.

An expression can itself contain a comma (e.g. in a string map key), so the
expressions list can't always be split on commas unambiguously.

The output file must not exist yet.

Each line of the param keys file is a needle, a colon and its config keys,
e.g. "OpenMetricsText1.0.0:global.scrape_protocols,scrape_configs.scrape_protocols".
Lines are split on their last colon, since needles can contain colons (e.g.
"http://127.0.0.1:3128"). It's an error if a needle in the input isn't in the
file, or if the file has a line without a colon or the same needle twice.
"""

import argparse
import os
import re
import sys

from dedup import normalize, read_csv, write_csv


# Integers with at least this many digits are replaced by <X>.
BIG_DIGITS = 7

# A quoted Go string, or a run of BIG_DIGITS or more digits not inside an
# identifier (e.g. not the 1 in func1); strings are matched so that they can be
# left alone.
BIG_NUMBER = re.compile(r'"(?:[^"\\]|\\.)*"|(?<![\w])\d{%d,}(?![\w])' % BIG_DIGITS)

# One step of an expression: the root name, a field (.name), or a bracketed
# part ([...], <key ...>), whose quoted strings may contain any characters.
STEP = re.compile(
    r'\.[^.\[<]+'
    r'|\[(?:"(?:[^"\\]|\\.)*"|[^\]"])*\]'
    r'|<(?:"(?:[^"\\]|\\.)*"|[^>"])*>'
    r'|^[^.\[<]+'
)


def collapse_cycles(steps):
    """Replace each run of two or more consecutive copies of the same
    sequence of steps (containing a field) by one step, "(sequence)+".
    Shorter sequences are collapsed first, and the result is collapsed again
    until nothing changes, so cycles within cycles are collapsed too."""
    changed = True
    while changed:
        changed = False
        for length in range(1, len(steps) // 2 + 1):
            out, i = [], 0
            while i < len(steps):
                seq = steps[i:i + length]
                copies = 1
                while steps[i + copies * length:i + (copies + 1) * length] == seq:
                    copies += 1
                if copies >= 2 and len(seq) == length and any(s.startswith(".") for s in seq):
                    out.append("(" + "".join(seq) + ")+")
                    i += copies * length
                    changed = True
                else:
                    out.append(steps[i])
                    i += 1
            steps = out
    return steps


def summarize(expr):
    """Return expr with big numbers replaced by <X> and repeated sequences of
    steps collapsed (see the module docstring)."""
    expr = BIG_NUMBER.sub(lambda m: m.group() if m.group().startswith('"') else "<X>", expr)
    steps = STEP.findall(expr)
    if "".join(steps) != expr:
        return expr  # couldn't split it into steps; leave it as is
    return "".join(collapse_cycles(steps))


# A collapsed sequence, as written by collapse_cycles; its content has no
# parentheses unless it contains a nested collapsed sequence.
CYCLE = re.compile(r"\(([^()]*)\)\+")


def drop_covered(exprs):
    """Return exprs without those that are another of them with each (steps)+
    written out once."""
    covered = set()
    for e in exprs:
        once = e
        while CYCLE.search(once):
            once = CYCLE.sub(r"\1", once)
        if once != e:
            covered.add(once)
    return [e for e in exprs if e not in covered]


def read_param_keys(path):
    """Return the param keys file as a dict from needle to config keys."""
    param_keys = {}
    with open(path, encoding="utf-8") as f:
        for n, line in enumerate(f, 1):
            line = line.rstrip("\r\n")
            if not line:
                continue
            if ":" not in line:
                sys.exit(f"needle_types.py: {path}:{n}: no colon")
            needle, keys = line.rsplit(":", 1)
            if needle in param_keys:
                sys.exit(f"needle_types.py: {path}:{n}: needle {needle!r} appears twice")
            param_keys[needle] = keys
    return param_keys


def main():
    p = argparse.ArgumentParser(
        description="Summarize argmatch output by needle and first type."
    )
    p.add_argument("--input", required=True, help="argmatch CSV, usually after dedup.py")
    p.add_argument("--param-keys", required=True, help="file of needle:config keys lines")
    p.add_argument("--output", required=True, help="CSV file to write; must not exist yet")
    args = p.parse_args()
    in_path, param_keys_path, out_path = args.input, args.param_keys, args.output
    if not os.path.isfile(param_keys_path):
        sys.exit(f"needle_types.py: {param_keys_path}: no such file")
    param_keys = read_param_keys(param_keys_path)
    with open(in_path, "rb") as f:
        rows = read_csv(f.read())
    if not rows:
        sys.exit(f"needle_types.py: {in_path}: empty")
    header = rows[0]
    needle, types = header.index("needle"), header.index("types")
    function, expression = header.index("function"), header.index("expression")
    missing = {row[needle] for row in rows[1:]} - param_keys.keys()
    if missing:
        sys.exit(f"needle_types.py: needles not in {param_keys_path}: {sorted(missing)}")
    # (needle, first type) -> its distinct expressions and functions; dicts
    # keep insertion order, so groups and their lists stay in the order in
    # which they first appear.
    groups = {}
    for row in rows[1:]:
        key = (row[needle], row[types] if len(row) > types else "")
        exprs, fns = groups.setdefault(key, ({}, {}))
        exprs[summarize(normalize(row, expression)[expression])] = None
        fns[row[function]] = None
    out = [["needle", "param_keys", "type", "expressions", "functions"]]
    for (n, t), (exprs, fns) in groups.items():
        out.append([n, param_keys[n], t, ",".join(drop_covered(list(exprs))), ",".join(fns)])
    try:
        f = open(out_path, "xb")  # "x": don't overwrite an existing file
    except FileExistsError:
        sys.exit(f"needle_types.py: {out_path} already exists")
    with f:
        write_csv(out, f)


if __name__ == "__main__":
    main()
