#!/usr/bin/env python3
"""Deduplicate and group/sort the CSV output of one argmatch run.

Usage: dedup.py [input.csv] > output.csv

Reads the CSV argmatch wrote (from the file, or stdin if none is given) and
writes it to stdout with duplicate rows removed, keeping the header. Rows are
compared whole, except that map positions in the expression column are
ignored (see below).

The output is grouped by needle, and the groups are ordered by how many
distinct haystacks the needle matched, fewest first (ties by needle). Within a
group, rows are sorted by haystack; rows with the same needle and haystack stay
in the order in which they first appear.

Columns are found by name in the header, since argmatch leaves some out: with
exact matching there's no haystack column (every haystack is its needle, so
groups are just sorted by needle), and with closures ignored there's no closure
column.
"""

import csv
import io
import re
import sys

USAGE = "usage: dedup.py [input.csv] > output.csv"

# A map entry whose key isn't a string, integer or bool is named by its
# position in the map, e.g. m[<key 3>] or m<key 3>, not by its key. A string
# key is a quoted Go literal, e.g. m["<key 3>"]; those are matched too so that
# they can be left alone.
POSITIONAL_KEY = re.compile(r'"(?:[^"\\]|\\.)*"|<key \d+>')


def normalize(row, expression):
    """Return row with map positions in the expression (the column at index
    expression) replaced by <key N>.

    Rows that differ only in a map position are merged. Within one run the
    positions of an unmodified map are the same on every call, but the map may
    have been modified between calls (e.g. entries inserted, making it grow),
    which moves entries to other positions. So the same entry can show up at
    different positions, and rows that differ only in the position most likely
    describe the same entry.
    """
    if len(row) > expression:
        row = row[:]
        row[expression] = POSITIONAL_KEY.sub(
            lambda m: m.group() if m.group().startswith('"') else "<key N>",
            row[expression],
        )
    return row


# A haystack can be any bytes argmatch found a match in, e.g. a binary buffer
# (CSVs written before argmatch escaped haystacks have them raw), so CSVs are
# read and written as bytes: invalid UTF-8 is kept as is (surrogateescape),
# and NUL bytes, which Python's csv module (before 3.11) rejects, are hidden
# behind a character the data doesn't contain while the csv module runs.


def _unused_char(text):
    return next(chr(c) for c in range(0xE000, 0xF900) if chr(c) not in text)


def read_csv(data):
    """Return the rows of CSV data (bytes)."""
    text = data.decode("utf-8", "surrogateescape")
    nul = _unused_char(text)
    rows = csv.reader(io.StringIO(text.replace("\0", nul), newline=""))
    return [[field.replace(nul, "\0") for field in row] for row in rows]


def write_csv(rows, out):
    """Write rows as CSV to the binary file out."""
    nul = _unused_char("".join(field for row in rows for field in row))
    buf = io.StringIO(newline="")
    csv.writer(buf, lineterminator="\n").writerows(
        [field.replace("\0", nul) for field in row] for row in rows
    )
    text = buf.getvalue().replace(nul, "\0")
    out.write(text.encode("utf-8", "surrogateescape"))


def main():
    if len(sys.argv) > 2:
        sys.exit(USAGE)
    if len(sys.argv) == 2:
        with open(sys.argv[1], "rb") as f:
            data = f.read()
    else:
        data = sys.stdin.buffer.read()
    rows = iter(read_csv(data))
    header = next(rows, None)
    if header is None:
        return
    needle, expression = header.index("needle"), header.index("expression")
    # Without a haystack column, every haystack is its needle.
    haystack = header.index("haystack") if "haystack" in header else needle
    seen = set()
    unique = []
    for row in rows:
        row = normalize(row, expression)
        key = tuple(row)
        if key not in seen:
            seen.add(key)
            unique.append(row)
    haystacks = {}  # needle -> its distinct haystacks
    for row in unique:
        haystacks.setdefault(row[needle], set()).add(row[haystack])
    # sort is stable, so ties keep their first-appearance order.
    unique.sort(key=lambda row: (len(haystacks[row[needle]]), row[needle], row[haystack]))
    write_csv([header] + unique, sys.stdout.buffer)


if __name__ == "__main__":
    main()
