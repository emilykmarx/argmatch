#!/usr/bin/env python3
"""Write contexttrack events' chains as a stack log for argmatch's -stacklog,
so that argmatch instruments the functions in them.

usage: events_to_stacklog.py events.jsonl > stacks.log

Each event (every line of events.jsonl) becomes one connection log in the
plain-text format stacklog.ParseOneConn reads: a "conn.log (send): BEGIN
STACKS" line, the event's stack and then its ancestry, and a "CONTENTS:"
line. argmatch then picks the functions out of the frames as it does for any
stack log (stacklog.FrameFunc, EnclosingFns, IgnoreFn).
"""

import json
import sys


def main():
    if len(sys.argv) != 2:
        sys.exit(__doc__.strip().splitlines()[2])
    out = sys.stdout
    for line in open(sys.argv[1]):
        if not line.strip():
            continue
        e = json.loads(line)
        out.write("conn.log (send): BEGIN STACKS\n")
        out.write(e["stack"].rstrip("\n") + "\n")
        out.write(e["ancestry"].rstrip("\n") + "\n")
        out.write("CONTENTS: \n")


if __name__ == "__main__":
    main()
