#!/usr/bin/env python3
"""

usage: join.py matches.calls.csv events.jsonl OUT_DIR

Frames are compared by goroutine ID and PC offset (the "+0x..." after the
file:line), not by function and line, because the two kinds of traceback
print lines differently. A live stack (argmatch's stack, the event's stack)
gives a caller's line from its call instruction, but an ancestor's section
gives it from the return address (printAncestorTracebackFuncInfo in
runtime/traceback.go), which can be on the next line: Manager.reloader calls
reload at manager.go:161, but ancestors' sections show manager.go:158, where
the loop jumps back to. Both print the offset of the return address from the
function's entry. A live stack also prints each inlined call as its own frame,
without an offset, before the physical frame it was inlined into; an
ancestor's section prints one line per physical frame. So a frame here is a
physical frame, with the names of any inlined frames above it. "created by"
lines have the same offset in both ("in goroutine N" is dropped: only the
innermost goroutine's line has it).

Writes OUT_DIR/joined.csv and prints a summary. joined.csv has columns
message_kind, message_label (as message_graph.py labels messages) and
needle: for each joined event and call, the event's kind and label with each
distinct needle among the call's matches (read from matches.csv next to
matches.calls.csv; argmatch writes the two together). Each distinct row is
written once, so rows don't say which events or calls they came from.
"""

import collections
import csv
import json
import os
import re
import sys

# message_graph labels messages the way its graph's nodes are labelled.
sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "conftamer", "contexttrack", "analysis"))
import message_graph  # noqa: E402

csv.field_size_limit(1 << 30)

FILE_LINE = re.compile(r"^\t.*?:\d+(?: \+(0x[0-9a-f]+))?$")


def parse_frames(lines, gid, out):
    """Append the physical frames in lines (each a function line, then a file
    line) to out as (kind, gid, offset, names): kind is "frame", "created" or
    "elided", offset the PC offset ("" if none), and names the functions (the
    inlined ones, then the physical one)."""
    names = []
    i = 0
    while i < len(lines):
        fn, offset = lines[i], ""
        if i + 1 < len(lines) and (m := FILE_LINE.match(lines[i + 1])):
            offset = m.group(1) or ""
            i += 1
        i += 1
        if fn.startswith("created by "):
            fn = re.sub(r" in goroutine \d+$", "", fn[len("created by "):])
            out.append(("created", gid, offset, (fn,)))
        elif fn.startswith("..."):  # "...additional frames elided..."
            out.append(("elided", gid, "", ()))
        else:
            names.append(fn[:fn.rindex("(")] if "(" in fn else fn)
            if offset:  # a physical frame; frames without one were inlined into it
                out.append(("frame", gid, offset, tuple(names)))
                names = []


def chain(stack, ancestry):
    """Return the chain of frames, innermost first, for a stack and ancestry."""
    out = []
    first, *frames = stack.split("\n")
    gid = int(first.split()[1])
    parse_frames(frames, gid, out)
    sections = re.split(r"^\[originating from goroutine (\d+)\]:\n", ancestry, flags=re.M)
    head = sections[0].split("\n")[1:]  # after "goroutine N"
    parse_frames([l for l in head if l], gid, out)
    for i in range(1, len(sections), 2):
        parse_frames([l for l in sections[i + 1].split("\n") if l], int(sections[i]), out)
    return out


def message_labels(events):
    """Return {index in events: message label} for the events message_graph.py
    treats as messages (message_graph.messages), labelled as it labels their
    nodes."""
    msgs = message_graph.messages(events)  # the same dicts as in events
    paths = message_graph.node_path_counts(msgs)
    index = {id(ev): i for i, ev in enumerate(events)}
    return {index[id(ev)]: message_graph._msg_label(ev, paths.get(message_graph._msg_key(ev), 0))
            for _, ev in msgs}


def call_needles(calls_path):
    """Return {call ID: its matches' distinct needles, in order of first
    appearance}, from the matches.csv argmatch wrote with calls_path (its
    matches.calls.csv)."""
    base, ext = os.path.splitext(calls_path)
    assert base.endswith(".calls"), calls_path
    needles = collections.defaultdict(dict)
    with open(base[:-len(".calls")] + ext, newline="") as f:
        r = csv.reader(f)
        h = next(r)
        ni, ci = h.index("needle"), h.index("call")
        for row in r:
            if len(row) > ci:
                needles[row[ci]][row[ni]] = None
    return {c: list(n) for c, n in needles.items()}


def main():
    calls_path, events_path, out_dir = sys.argv[1:]
    calls = list(csv.DictReader(open(calls_path, newline="")))
    events = [json.loads(l) for l in open(events_path)]
    labels = message_labels(events)
    for c in calls:
        c["chain"] = chain(c["stack"], c["ancestry"])
    # Index calls by (F, its goroutine, everything older than F), the key an
    # event's frame of F must match. Names aren't compared, only kinds,
    # goroutines and offsets (see the top).
    def key(frames):
        return tuple(t[:3] for t in frames)

    index = collections.defaultdict(list)
    for c in calls:
        f = c["chain"][0]
        assert f[0] == "frame" and f[3][-1] == c["function"], (c["call"], f)
        index[(f[1], c["function"], key(c["chain"][1:]))].append(c)

    joined = set()
    rows = []
    for ei, e in enumerate(events):
        if ei not in labels:  # not a message (see message_labels)
            continue
        ch = chain(e["stack"], e["ancestry"])
        for i, (kind, gid, _, names) in enumerate(ch):
            if kind != "frame":
                continue
            older = key(ch[i + 1:])
            for fn in set(names):
                for c in index.get((gid, fn, older), []):
                    joined.add((ei, c["call"]))
                    rows.append([ei + 1, e["kind"], labels[ei], c["call"], c["function"]])

    os.makedirs(out_dir, exist_ok=True)
    with open(os.path.join(out_dir, "joined.csv"), "w", newline="") as f:
        w = csv.writer(f, lineterminator="\n")
        w.writerow(["message_kind", "message_label", "needle"])
        needles = call_needles(calls_path)
        out = {}  # distinct rows, in order of first appearance
        for event_line, kind, label, call, _ in rows:
            for needle in needles.get(call, []):
                out[(kind, label, needle)] = None
        w.writerows(out)

    print(f"{len(calls)} calls with matches; {len(events)} events, {len(labels)} of them messages (as message_graph.py counts them)")
    print(f"joined (event, call) pairs: {len(joined)}; events joined: {len({e for e, _ in joined})}; calls joined: {len({c for _, c in joined})}")
    fns = collections.Counter((r[4].rsplit("/", 1)[-1], r[1]) for r in rows)
    print("joined pairs by function and message kind:")
    for (fn, kind), n in fns.most_common(25):
        print(f"  {n:6d}  {kind:18s} {fn}")


if __name__ == "__main__":
    main()
