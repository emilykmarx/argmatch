#!/bin/bash
# Runs on the argmatch VM: takes one sample of a running experiment started
# with WAIT=none run_on_vm.sh NAME, and prints it as a tab-separated line:
# seconds since restart, checks so far, distinct functions checked,
# functions checked MAXCALLS times, functions instrumented, raw rows, and
# after dedup.py and needle_types.py the unique (needle, type) pairs and
# distinct needles and types, then alerts sent. Needs the program built with
# ARGMATCH_STATS=true, and $RESULTS/param_keys.txt.
#
# usage: sample_on_vm.sh NAME
set -uo pipefail
name=$1
CT=/home/emily/projects/config_tracing
RESULTS=$CT/dump_ancestors/argmatch-experiment
A=$CT/dump_ancestors/argmatch
CTX=kind-prombench
dir=$RESULTS/$name
now=$(( $(date +%s) - $(cat $dir/start) ))
maxcalls=$(sed -n 's/^ARGMATCH_MAXCALLS=//p' $dir/args.txt)

kubectl --context $CTX logs --timestamps deploy/prometheus-meta -c prometheus > $dir/pod.log 2>/dev/null
read checks fns full instrumented < <(awk -v max="$maxcalls" '
    $2 == "argmatch-instrumented:" && $4 == "functions" { n = $3 }
    $2 == "argmatch-stats:" { checks++; c[$3]++ }
    END { for (f in c) { fns++; if (max > 0 && c[f] >= max) full++ }
          print checks + 0, fns + 0, full + 0, n + 0 }' $dir/pod.log)

sudo cp $CT/dump_ancestors/argmatch-out/matches.csv $dir/matches.csv 2>/dev/null && sudo chown $(id -u):$(id -g) $dir/matches.csv
rows=$(( $(wc -l < $dir/matches.csv) - 1 ))
python3 $A/dedup.py $dir/matches.csv > $dir/deduped.csv
rm -f $dir/needle_types.csv
python3 $A/needle_types.py --input $dir/deduped.csv --param-keys $RESULTS/param_keys.txt --output $dir/needle_types.csv
read pairs needles types < <(python3 -c "
import sys; sys.path.insert(0, '$A')
from dedup import read_csv
r = read_csv(open('$dir/needle_types.csv', 'rb').read())[1:]
print(len(r), len({x[0] for x in r}), len({x[2] for x in r}))")
alerts=$(kubectl --context $CTX exec deploy/prometheus-meta -c prometheus -- \
        wget -qO- localhost:9090/prometheus-meta/metrics 2>/dev/null |
    awk '/^prometheus_notifications_sent_total/ {s += $2} END {print s + 0}' || true)
printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' $now $checks $fns $full $instrumented $rows $pairs $needles $types ${alerts:-0}
