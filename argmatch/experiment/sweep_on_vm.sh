#!/bin/bash
# Runs on the argmatch VM: a sweep of argmatch settings, one run each with
# run_on_vm.sh followed by analyze.sh, appending each run's summary line to
# $RESULTS/summary.tsv and its progress to $RESULTS/sweep.log. Meant to run
# in a detached tmux session, so it keeps going without a local machine:
#   tmux new -d -s sweep 'bash .../argmatch/experiment/sweep_on_vm.sh'
# Runs whose results already exist (a RESULT line and needle_types.csv) are
# skipped, so it can be restarted after an interruption. Each run is limited
# to 20 minutes.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
CT=/home/emily/projects/config_tracing
RESULTS=$CT/dump_ancestors/argmatch-experiment
PARAM_KEYS=$RESULTS/param_keys.txt # copied by watch.sh
mkdir -p $RESULTS
cp $here/view_dashboards.py $here/reload_on_vm.sh $RESULTS/
say() { echo "$(date -u +%FT%TZ) $*" | tee -a $RESULTS/sweep.log; }

# The best run so far (reload-d10-e64-c3); each run changes one setting.
BASE="DEPTH=10 MAXELEMS=64 MAXCALLS=3 IGNORELIBS=false IGNORETYPES= MAXMEM=0 MAXTIME=0"
# name|WAIT|settings changed from BASE
RUNS=(
    "sweep-base-1|240|"
    "sweep-c10-1|360|MAXCALLS=10"
    "sweep-c10-2|360|MAXCALLS=10"
    "sweep-c30-1|360|MAXCALLS=30"
    "sweep-c30-2|360|MAXCALLS=30"
    "sweep-d15-1|240|DEPTH=15"
    "sweep-d15-2|240|DEPTH=15"
    "sweep-d20-1|240|DEPTH=20"
    "sweep-d20-2|240|DEPTH=20"
    "sweep-e256-1|240|MAXELEMS=256"
    "sweep-e256-2|240|MAXELEMS=256"
)

say "sweep started: ${#RUNS[@]} runs"
for r in "${RUNS[@]}"; do
    IFS='|' read -r name wait changes <<< "$r"
    dir=$RESULTS/$name
    if grep -q '^RESULT' $dir/timing.txt 2>/dev/null && [ -s $dir/needle_types.csv ]; then
        say "$name: already done, skipping"
        continue
    fi
    say "$name: starting (WAIT=$wait, $changes)"
    # shellcheck disable=SC2086
    if ! WAIT=$wait timeout 1200 bash $here/run_on_vm.sh $name $BASE $changes > $RESULTS/$name.out 2>&1; then
        say "$name: FAILED (exit $?): $(tail -1 $RESULTS/$name.out)"
        continue
    fi
    if line=$(bash $here/analyze.sh $dir $PARAM_KEYS 2> $dir/analyze.err); then
        echo "$line" >> $RESULTS/summary.tsv
        say "$name: $(echo "$line" | tr '\t' ' ')"
        [ -s $dir/analyze.err ] && say "$name: $(tr '\n' ' ' < $dir/analyze.err)"
    else
        say "$name: analysis FAILED: $(tail -1 $dir/analyze.err)"
    fi
done
say "sweep done"
