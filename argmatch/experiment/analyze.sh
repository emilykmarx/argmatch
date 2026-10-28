#!/bin/bash
# Runs dedup.py and needle_types.py on one experiment's matches.csv and
# prints a tab-separated summary line (see stats.py).
# usage: analyze.sh RESULT_DIR PARAM_KEYS
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
dir=$1; param_keys=$2
python3 "$here/../dedup.py" "$dir/matches.csv" > "$dir/deduped.csv"
rm -f "$dir/needle_types.csv"
python3 "$here/../needle_types.py" --input "$dir/deduped.csv" --param-keys "$param_keys" --output "$dir/needle_types.csv"
python3 "$here/stats.py" "$dir"
