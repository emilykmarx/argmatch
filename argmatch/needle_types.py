#!/usr/bin/env python3
"""Reduce argmatch output to the distinct (param keys, first type) pairs.

Usage: needle_types.py --input IN.csv --param-keys PARAM_KEYS --output OUT.csv

The input is argmatch's CSV, usually after dedup.py. All columns except
needle and the first types column (the parameter's own type, or the closure
or interface step it starts with) are dropped. Each needle is replaced by the
config keys it stands for, from the param keys file, in a column renamed
param_keys. Then duplicate rows are removed, keeping the order in which they
first appear, and the result is written to the output file, which must not
exist yet.

Each line of the param keys file is a needle, a colon and its config keys,
e.g. "OpenMetricsText1.0.0:global.scrape_protocols,scrape_configs.scrape_protocols".
Lines are split on their last colon, since needles can contain colons (e.g.
"http://127.0.0.1:3128"). It's an error if a needle in the input isn't in the
file, or if the file has a line without a colon or the same needle twice.
"""

import argparse
import os
import sys

from dedup import read_csv, write_csv


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
        description="Reduce argmatch output to the distinct (needle, first type) pairs."
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
    missing = {row[needle] for row in rows[1:]} - param_keys.keys()
    if missing:
        sys.exit(f"needle_types.py: needles not in {param_keys_path}: {sorted(missing)}")
    seen = set()
    out = [["param_keys", "types"]]
    for row in rows[1:]:
        pair = (param_keys[row[needle]], row[types] if len(row) > types else "")
        if pair not in seen:
            seen.add(pair)
            out.append(list(pair))
    try:
        f = open(out_path, "xb")  # "x": don't overwrite an existing file
    except FileExistsError:
        sys.exit(f"needle_types.py: {out_path} already exists")
    with f:
        write_csv(out, f)


if __name__ == "__main__":
    main()
