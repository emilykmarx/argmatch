#!/usr/bin/env python3
"""Plot an experiment's timeseries.tsv (written by watch.sh) to timeseries.png
in the same directory: checks, functions (checked, at MAXCALLS,
instrumented) and unique (needle, type) pairs, needles and types over time.

Usage: plot.py RESULT_DIR
"""

import os
import sys

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402

COLS = ["t", "checks", "fns", "full", "instrumented", "rows", "pairs", "needles", "types", "alerts"]


def main():
    d = sys.argv[1]
    rows = [dict(zip(COLS, map(float, l.split("\t")))) for l in open(os.path.join(d, "timeseries.tsv")) if l.strip()]
    if not rows:
        return
    t = [r["t"] / 60 for r in rows]
    fig, ax = plt.subplots(3, 1, figsize=(8, 9), sharex=True)
    ax[0].plot(t, [r["checks"] for r in rows], marker=".", label="checks")
    ax[0].set_ylabel("checks so far")
    ax[1].plot(t, [r["fns"] for r in rows], marker=".", label="functions checked")
    ax[1].plot(t, [r["full"] for r in rows], marker=".", label="functions at MAXCALLS")
    ax[1].plot(t, [r["instrumented"] for r in rows], linestyle="--", label="functions instrumented")
    ax[1].set_ylabel("functions")
    ax[2].plot(t, [r["pairs"] for r in rows], marker=".", label="(needle, type) pairs")
    ax[2].plot(t, [r["needles"] for r in rows], marker=".", label="needles")
    ax[2].plot(t, [r["types"] for r in rows], marker=".", label="types")
    ax[2].set_ylabel("unique")
    ax[2].set_xlabel("minutes since restart")
    first_alert = next((r["t"] / 60 for r in rows if r["alerts"] > 0), None)
    for a in ax:
        if first_alert is not None:
            a.axvline(first_alert, color="grey", linestyle=":", label="first alert (by this sample)")
        a.grid(alpha=0.3)
        a.legend(loc="lower right", fontsize=8)
    fig.suptitle(os.path.basename(os.path.normpath(d)))
    fig.tight_layout()
    fig.savefig(os.path.join(d, "timeseries.png"), dpi=100)


if __name__ == "__main__":
    main()
