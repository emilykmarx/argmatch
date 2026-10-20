#!/usr/bin/env python3
"""Deduplicate the CSV output of one argmatch run.

Usage: dedup.py [input.csv] > output.csv

Reads the CSV argmatch wrote (from the file, or stdin if none is given) and
writes it to stdout with duplicate rows removed, keeping the header. Rows are
compared whole, except that map positions in the expression column are
ignored (see below). The output is sorted by needle, then haystack; rows with
the same needle and haystack stay in the order in which they first appear.
"""

import csv
import io
import re
import sys

USAGE = "usage: dedup.py [input.csv] > output.csv"

NEEDLE, HAYSTACK, EXPRESSION = 0, 1, 3  # indexes of columns

# A map entry whose key isn't a string, integer or bool is named by its
# position in the map, e.g. m[<key 3>] or m<key 3>, not by its key. A string
# key is a quoted Go literal, e.g. m["<key 3>"]; those are matched too so that
# they can be left alone.
POSITIONAL_KEY = re.compile(r'"(?:[^"\\]|\\.)*"|<key \d+>')


def normalize(row):
    """Return row with map positions in the expression replaced by <key N>.

    Rows that differ only in a map position are merged. Within one run the
    positions of an unmodified map are the same on every call, but the map may
    have been modified between calls (e.g. entries inserted, making it grow),
    which moves entries to other positions. So the same entry can show up at
    different positions, and rows that differ only in the position most likely
    describe the same entry.
    """
    if len(row) > EXPRESSION:
        row = row[:]
        row[EXPRESSION] = POSITIONAL_KEY.sub(
            lambda m: m.group() if m.group().startswith('"') else "<key N>",
            row[EXPRESSION],
        )
    return row


def main():
    if len(sys.argv) > 2:
        sys.exit(USAGE)
    if len(sys.argv) == 2:
        with open(sys.argv[1], "rb") as f:
            data = f.read()
    else:
        data = sys.stdin.buffer.read()
    # A haystack can be any bytes argmatch found a match in, e.g. a binary
    # buffer, so keep invalid UTF-8 as is (surrogateescape) and hide NUL
    # bytes, which Python's csv module (before 3.11) rejects, behind a
    # character the input doesn't contain.
    text = data.decode("utf-8", "surrogateescape")
    nul = next(chr(c) for c in range(0xE000, 0xF900) if chr(c) not in text)
    rows = csv.reader(io.StringIO(text.replace("\0", nul), newline=""))
    buf = io.StringIO(newline="")
    out = csv.writer(buf, lineterminator="\n")
    header = next(rows, None)
    if header is None:
        return
    out.writerow(header)
    seen = set()
    unique = []
    for row in rows:
        row = normalize(row)
        key = tuple(row)
        if key not in seen:
            seen.add(key)
            unique.append(row)
    # sort is stable, so ties keep their first-appearance order.
    unique.sort(key=lambda row: (row[NEEDLE], row[HAYSTACK]))
    out.writerows(unique)
    sys.stdout.buffer.write(buf.getvalue().replace(nul, "\0").encode("utf-8", "surrogateescape"))


if __name__ == "__main__":
    main()
