#!/bin/bash
# Runs one experiment: builds and runs prometheus-meta on the argmatch VM
# with the given argmatch build args (see run_on_vm.sh), copies the results
# back to dump_ancestors/argmatch-experiment/NAME, runs dedup.py and
# needle_types.py on them, and appends a line to summary.tsv there.
#
# usage: [WAIT=seconds|alert] [VIEW=0] [RELOAD=0] run.sh NAME [ARG=VALUE ...]
#   e.g. run.sh d2-e4-c1 DEPTH=2 MAXELEMS=4 MAXCALLS=1
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
name=$1
VM=(gcloud compute ssh --zone us-central1-b argmatch --project conftamer)
REMOTE=/home/emily/projects/config_tracing/dump_ancestors/argmatch-experiment
LOCAL=$here/../../argmatch-experiment
PARAM_KEYS=/home/emily/projects/config_tracing/prom-test-infra/prombench/param_keys.txt

"${VM[@]}" --command "mkdir -p $REMOTE" > /dev/null
gcloud compute scp --zone us-central1-b --project conftamer "$here/view_dashboards.py" "$here/reload_on_vm.sh" "argmatch:$REMOTE/" > /dev/null
"${VM[@]}" --command "WAIT=${WAIT:-240} VIEW=${VIEW:-1} RELOAD=${RELOAD:-1} bash -s -- $(printf '%q ' "$@")" < "$here/run_on_vm.sh"
mkdir -p "$LOCAL"
rm -rf "$LOCAL/$name"
gcloud compute scp --recurse --zone us-central1-b --project conftamer "argmatch:$REMOTE/$name" "$LOCAL/" > /dev/null
"$here/analyze.sh" "$LOCAL/$name" "$PARAM_KEYS" | tee -a "$LOCAL/summary.tsv"
