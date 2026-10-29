#!/bin/bash
# Like run.sh, but instead of stopping at the first alert, samples the run
# every INTERVAL seconds (sample_on_vm.sh), plots the samples
# (timeseries.png), and stops once the number of checks and of unique
# (needle, type) pairs have both stayed the same for QUIET seconds, or after
# MAX seconds. ARGMATCH_STATS=true is added to the build args.
#
# usage: [INTERVAL=60] [QUIET=300] [MAX=1800] watch.sh NAME [ARG=VALUE ...]
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
name=$1; shift
INTERVAL=${INTERVAL:-60}; QUIET=${QUIET:-300}; MAX=${MAX:-1800}
VM=(gcloud compute ssh --zone us-central1-b argmatch --project conftamer)
SCP=(gcloud compute scp --zone us-central1-b --project conftamer)
REMOTE=/home/emily/projects/config_tracing/dump_ancestors/argmatch-experiment
LOCAL=$here/../../argmatch-experiment
PARAM_KEYS=/home/emily/projects/config_tracing/prom-test-infra/prombench/param_keys.txt

"${VM[@]}" --command "mkdir -p $REMOTE" > /dev/null
"${SCP[@]}" "$PARAM_KEYS" "argmatch:$REMOTE/param_keys.txt" > /dev/null
"${VM[@]}" --command "WAIT=none bash -s -- $(printf '%q ' "$name" "$@" STATS=true)" < "$here/run_on_vm.sh"
mkdir -p "$LOCAL/$name"
ts=$LOCAL/$name/timeseries.tsv
: > "$ts"
quiet_since=; last=
while :; do
    sleep "$INTERVAL"
    line=$("${VM[@]}" --command "bash -s -- $name" < "$here/sample_on_vm.sh" | tail -1)
    echo "$line" >> "$ts"
    python3 "$here/plot.py" "$LOCAL/$name"
    read t checks fns full instrumented rows pairs needles types alerts <<< "$line"
    echo "$(date +%T) t=${t}s checks=$checks fns=$fns full=$full/$instrumented rows=$rows pairs=$pairs needles=$needles types=$types alerts=$alerts"
    if [ "$instrumented" -eq 0 ]; then
        echo "no instrumented functions reported: is ARGMATCH_STATS passed to the build?" >&2
        exit 1
    fi
    key="$checks $pairs"
    if [ "$key" != "$last" ]; then last=$key; quiet_since=$t; fi
    if [ $(( t - quiet_since )) -ge "$QUIET" ]; then echo "flat for ${QUIET}s: stopping at ${t}s"; break; fi
    if [ "$t" -ge "$MAX" ]; then echo "reached ${MAX}s: stopping"; break; fi
done
"${SCP[@]}" --recurse "argmatch:$REMOTE/$name" "$LOCAL/" > /dev/null
python3 "$here/plot.py" "$LOCAL/$name"
python3 "$here/stats.py" "$LOCAL/$name" | tee -a "$LOCAL/summary.tsv"
