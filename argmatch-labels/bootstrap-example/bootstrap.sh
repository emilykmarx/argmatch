#!/bin/bash
# Runs the two steps in ../README.md's "Bootstrapping" section, then the join,
# with Prometheus (prometheus/prombench-argmatch.Dockerfile) locally in Docker:
#   1. build with argmatch instrumenting nothing, and run the workload with
#      contexttrack recording events;
#   2. turn those events into a stack log (events_to_stacklog.py), build with
#      argmatch instrumenting its functions, and run the same workload;
# then join step 2's outputs (../join.py).
#
# usage: bootstrap.sh OUT_DIR
# OUT_DIR gets step1/ and step2/ (each run's events.jsonl, matches.csv,
# matches.calls.csv, pod.log, build.log) and join/ (joined.csv, summary.txt).
#
# The workload (see workload below): Prometheus scrapes itself every 5 s
# (prometheus.yml), and from 20 s on it gets two API queries every 5 s, and one
# config reload at 30 s; it's stopped at $WAIT seconds (default 240). Builds
# take about 5 minutes each (changing what argmatch instruments recompiles
# everything), so the whole thing takes about 20 minutes.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)    # this directory, with prometheus.yml
labels=$(cd "$here/.." && pwd)          # argmatch-labels, with join.py and events_to_stacklog.py
CT=$(cd "$here/../../.." && pwd)        # config_tracing: the Docker build context
OUT=$(mkdir -p "$1" && cd "$1" && pwd)
WAIT=${WAIT:-240}
PORT=${PORT:-19095}
STACKLOG=$CT/prometheus/argmatch-stacklog-contexttrack.txt

say() { echo "$(date -u +%T) $*"; }

# build TAG ARGMATCH_ARG=VALUE...: builds the image with the experiment's
# argmatch settings, overridden by the arguments.
build() {
    local tag=$1; shift
    local -A a=(
        [MATCHFILE]=/src/argmatch-matches.txt [MATCH]= [FUNC]= [STACKLOG]=
        [RECVONLY]=1 [DEPTH]=10 [MAXELEMS]=64 [MAXCALLS]=30 [IGNORELIBS]=false
        [IGNORETYPES]= [MAXMEM]=0 [MAXTIME]=0 [OUT]=/argmatch-out/matches.csv [STATS]=true
    )
    for kv in "$@"; do a[${kv%%=*}]=${kv#*=}; done
    local args=()
    for k in "${!a[@]}"; do args+=(--build-arg "ARGMATCH_$k=${a[$k]}"); done
    say "building $tag: $*"
    (cd "$CT" && timeout 2400 docker build --progress=plain -f prometheus/prombench-argmatch.Dockerfile \
        --build-arg VERSION=v3.2.1 "${args[@]}" -t "$tag" .) > "$OUT/$tag.build.log" 2>&1 ||
        { say "build of $tag failed; see $OUT/$tag.build.log"; tail -20 "$OUT/$tag.build.log"; exit 1; }
}

# workload TAG DIR: runs the image with contexttrack on, sends the workload, and
# leaves the outputs and the pod's log in DIR.
workload() {
    local tag=$1 dir=$2 name=bootstrap-$$
    mkdir -p "$dir" && chmod 777 "$dir" # the container runs as nobody
    say "running $tag for ${WAIT}s"
    timeout 60 docker run -d --rm --name "$name" -p "$PORT:9090" \
        -v "$dir:/argmatch-out" -v "$here/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
        -e CONFTAMER_EVENTS=/argmatch-out/events.jsonl "$tag" \
        --config.file=/etc/prometheus/prometheus.yml --storage.tsdb.path=/prometheus --web.enable-lifecycle > /dev/null
    trap "timeout 60 docker stop $name > /dev/null 2>&1 || true" EXIT
    local start=$(date +%s) reloaded=0 t
    while t=$(( $(date +%s) - start )); [ $t -lt "$WAIT" ]; do
        if [ $t -ge 20 ]; then
            timeout 10 curl -s -o /dev/null "localhost:$PORT/api/v1/query?query=up" || true
            timeout 10 curl -s -o /dev/null "localhost:$PORT/api/v1/query_range?query=rate(prometheus_http_requests_total[1m])&start=$(( $(date +%s) - 600 ))&end=$(date +%s)&step=15" || true
        fi
        if [ $t -ge 30 ] && [ $reloaded = 0 ]; then
            timeout 60 curl -s -X POST -o /dev/null "localhost:$PORT/-/reload" && reloaded=1 || say "reload failed"
        fi
        sleep 5
    done
    timeout 60 docker logs "$name" > "$dir/pod.log" 2>&1
    timeout 60 docker stop "$name" > /dev/null
    trap - EXIT
    [ -s "$dir/events.jsonl" ] || { say "no events in $dir/events.jsonl; see $dir/pod.log"; exit 1; }
    say "$tag: $(wc -l < "$dir/events.jsonl") events, $(grep -m1 -o 'argmatch-instrumented: [0-9]* functions' "$dir/pod.log" || echo 'no argmatch-instrumented line')"
}

# 1. argmatch instruments nothing (it needs -func or -stacklog, so name a
# function that doesn't exist); contexttrack records events.
build bootstrap-step1:latest FUNC=main.argmatchNone
workload bootstrap-step1:latest "$OUT/step1"

# 2. argmatch instruments the functions in step 1's chains.
timeout 300 python3 "$labels/events_to_stacklog.py" "$OUT/step1/events.jsonl" > "$STACKLOG"
cp "$STACKLOG" "$OUT/step1/stacklog.txt"
build bootstrap-step2:latest STACKLOG=/src/$(basename "$STACKLOG")
workload bootstrap-step2:latest "$OUT/step2"
[ -s "$OUT/step2/matches.calls.csv" ] || { say "no calls in $OUT/step2/matches.calls.csv; see $OUT/step2/pod.log"; exit 1; }

# Join step 2's outputs.
mkdir -p "$OUT/join"
timeout 900 python3 "$labels/join.py" "$OUT/step2/matches.calls.csv" "$OUT/step2/events.jsonl" "$OUT/join" > "$OUT/join/summary.txt"
say "done; see $OUT/join/summary.txt"
head -2 "$OUT/join/summary.txt"
