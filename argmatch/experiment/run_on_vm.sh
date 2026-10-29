#!/bin/bash
# Runs on the argmatch VM: builds prometheus-conftamer:v3.2.1 with the given
# argmatch build args (and ARGMATCH_STATS=true), loads it into the kind
# cluster's main-node, restarts prometheus-meta, waits WAIT seconds (default
# 240), and saves argmatch's output and the pod's log under $RESULTS/<name>/.
# It also records how long before the capture argmatch's last check was
# (last_check); if that's under 30 s, argmatch was probably still checking,
# and the run needs a longer WAIT. In a run at depth 10, -maxelems 64,
# -maxcalls 3, every check was within 66 s of the restart (see
# watch-d10-e64-c3), so 150 s left a margin; the default is 240 s since the
# config reloads (see below) cause more checks later.
#
# Unless VIEW=0, once prometheus-meta is ready it also runs
# view_dashboards.py (copied to $RESULTS by run.sh) until the capture, so
# that Grafana queries prometheus-meta as when a person views the dashboards,
# as in the run the stack log came from. Its output goes to viewer.log.
# Unless RELOAD=0, RELOAD_AFTER seconds (default 30) after prometheus-meta
# is ready it also runs reload_on_vm.sh (copied by run.sh too), which
# reloads a changed config and then the original one, logging to reload.log.
#
# usage: run_on_vm.sh NAME [ARG=VALUE ...]
#   e.g. run_on_vm.sh d2-e4-c1 DEPTH=2 MAXELEMS=4 MAXCALLS=1
# Each ARG=VALUE overrides --build-arg ARGMATCH_ARG; the rest are the
# defaults below (from prometheus-test-infra/prombench/build_prom_argmatch.sh).
# With WAIT=alert, it waits for prometheus-meta's first alert instead (the
# original rule, which turned out too short); with WAIT=none, it stops after
# restarting prometheus-meta (writing the restart time to
# $RESULTS/<name>/start), for watch.sh to sample instead.
set -euo pipefail
name=$1; shift
CT=/home/emily/projects/config_tracing
OUTDIR=$CT/dump_ancestors/argmatch-out
RESULTS=$CT/dump_ancestors/argmatch-experiment
CTX=kind-prombench
TIMEOUT=${TIMEOUT:-3600} # seconds to wait for an alert

declare -A args=(
    [MATCH]= [MATCHFILE]=/src/argmatch-matches.txt
    [FUNC]= [STACKLOG]=/src/argmatch-stacklog.txt
    [OUT]=/argmatch-out/matches.csv [RECVONLY]=1 [EXACT]=true [IGNORECLOSURES]=true
    [DEPTH]=2 [MAXFIELDS]=-1 [MAXELEMS]=4 [MAXCALLS]=1
    [IGNORELIBS]=true [IGNORETYPES]=config.Config [MAXMEM]=1GB [MAXTIME]=10s
    [STATS]=true
)
for kv in "$@"; do args[${kv%%=*}]=${kv#*=}; done
buildargs=()
for k in "${!args[@]}"; do buildargs+=(--build-arg "ARGMATCH_$k=${args[$k]}"); done

dir=$RESULTS/$name
mkdir -p $dir
: > $dir/timing.txt
log() { echo "$(date -u +%FT%TZ) $*" | tee -a $dir/timing.txt; }
for k in $(printf '%s\n' "${!args[@]}" | sort); do echo "ARGMATCH_$k=${args[$k]}"; done > $dir/args.txt
log "run $name: $*"

# 1. Build.
t=$(date +%s)
cd $CT
docker build -q -f prometheus/prombench-argmatch.Dockerfile --build-arg VERSION=v3.2.1 \
    "${buildargs[@]}" -t prometheus-conftamer:v3.2.1 . > $dir/build.log 2>&1
build=$(( $(date +%s) - t )); log "built in ${build}s"

# 2. Load into the node(s) prometheus-meta can run on.
t=$(date +%s)
tar=$(mktemp /tmp/prom-image.XXXX.tar)
docker save prometheus-conftamer:v3.2.1 -o $tar
for n in $(kubectl --context $CTX get nodes -l cloud.google.com/gke-nodepool=main-node -o name | sed 's#node/##'); do
    docker exec -i $n ctr -n k8s.io images import - < $tar > /dev/null
done
rm -f $tar
load=$(( $(date +%s) - t )); log "loaded in ${load}s"

# 3. Restart prometheus-meta with an empty output directory. Scaling to 0
# first means the old pod is gone (no writes to matches.csv, TSDB unlocked)
# before the new one starts.
kubectl --context $CTX scale deploy/prometheus-meta --replicas=0 > /dev/null
kubectl --context $CTX wait --for=delete pod -l app=prometheus-meta --timeout=600s > /dev/null 2>&1 || true
sudo rm -f $OUTDIR/matches.csv
start=$(date +%s)
kubectl --context $CTX scale deploy/prometheus-meta --replicas=1 > /dev/null
log "prometheus-meta restarted"
echo $start > $dir/start
WAIT=${WAIT:-240}
if [ "$WAIT" = none ]; then
    echo "RESULT name=$name build=${build}s load=${load}s" | tee -a $dir/timing.txt
    exit 0
fi

# Simulated dashboard viewer (see above), stopped on exit.
viewer_pids=()
trap 'kill "${viewer_pids[@]}" 2>/dev/null || true' EXIT
start_viewer() {
    kubectl --context $CTX port-forward svc/grafana 3000:80 > $dir/port-forward.log 2>&1 &
    viewer_pids+=($!)
    sleep 2
    python3 $RESULTS/view_dashboards.py --url http://localhost:3000/grafana \
        --duration $(( WAIT - ready )) > $dir/viewer.log 2>&1 &
    viewer_pids+=($!)
    log "dashboard viewer started"
}

# 4. Wait WAIT seconds (or, with WAIT=alert, for the first alert sent to
# alertmanager), noting when prometheus-meta is ready and first alerts.
ready=; sent=0; alert=
while :; do
    now=$(( $(date +%s) - start ))
    if [ -z "$ready" ] && kubectl --context $CTX exec deploy/prometheus-meta -c prometheus -- \
            wget -qO- localhost:9090/prometheus-meta/-/ready > /dev/null 2>&1; then
        ready=$now; log "ready after ${ready}s"
        if [ "${VIEW:-1}" != 0 ] && [ "$WAIT" != alert ]; then start_viewer; fi
    fi
    if [ -n "$ready" ] && [ "${RELOAD:-1}" != 0 ] && [ "$WAIT" != alert ] && [ -z "${reload_started:-}" ] &&
            [ $now -ge $(( ready + ${RELOAD_AFTER:-30} )) ]; then
        bash $RESULTS/reload_on_vm.sh $dir/reload.log &
        reload_pid=$!; reload_started=1
        log "config reload started"
    fi
    sent=$(kubectl --context $CTX exec deploy/prometheus-meta -c prometheus -- \
            wget -qO- localhost:9090/prometheus-meta/metrics 2>/dev/null |
        awk '/^prometheus_notifications_sent_total/ {s += $2} END {print s + 0}' || true)
    sent=${sent:-0}
    if [ -z "$alert" ] && [ "${sent%.*}" -gt 0 ]; then alert=$now; log "first alert sent after ${alert}s"; fi
    if [ "$WAIT" = alert ]; then
        [ -n "$alert" ] && break
        if [ $now -ge $TIMEOUT ]; then log "no alert after ${now}s"; break; fi
    elif [ $now -ge "$WAIT" ]; then
        break
    fi
    sleep 5
done
captured=$(( $(date +%s) - start ))
if [ -n "${reload_pid:-}" ]; then
    # Let it finish (it restores the config map), but not forever.
    for _ in $(seq 60); do kill -0 $reload_pid 2>/dev/null || break; sleep 5; done
    wait $reload_pid 2>/dev/null && log "config reloads done" || log "config reload FAILED: $(tail -1 $dir/reload.log 2>/dev/null)"
fi
if [ ${#viewer_pids[@]} -gt 0 ]; then
    kill "${viewer_pids[@]}" 2>/dev/null || true
    wait 2>/dev/null || true
    log "viewer: $(tail -1 $dir/viewer.log)"
fi

# 5. Save the output and the pod's log.
sudo cp $OUTDIR/matches.csv $dir/matches.csv 2>/dev/null || : > $dir/matches.csv
sudo chown $(id -u):$(id -g) $dir/matches.csv
kubectl --context $CTX logs --timestamps deploy/prometheus-meta -c prometheus > $dir/pod.log 2>&1 || true
rows=$(( $(wc -l < $dir/matches.csv) - 1 ))
# Seconds between argmatch's last check and the capture.
last=$(grep ' argmatch-stats: ' $dir/pod.log | tail -1 | cut -d' ' -f1)
last_check=never
[ -n "$last" ] && last_check=$(( $(date +%s) - $(date -d "$last" +%s) ))
log "saved $rows rows after ${captured}s; last check ${last_check}s before"
echo "RESULT name=$name build=${build}s load=${load}s ready=${ready:-never}s alert=${alert:-never}s captured=${captured}s last_check=${last_check}s rows=$rows" | tee -a $dir/timing.txt
