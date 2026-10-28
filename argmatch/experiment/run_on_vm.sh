#!/bin/bash
# Runs on the argmatch VM: builds prometheus-conftamer:v3.2.1 with the given
# argmatch build args, loads it into the kind cluster's main-node, restarts
# prometheus-meta, waits until it has sent an alert, and saves argmatch's
# output and the pod's log under $RESULTS/<name>/.
#
# usage: run_on_vm.sh NAME [ARG=VALUE ...]
#   e.g. run_on_vm.sh d2-e4-c1 DEPTH=2 MAXELEMS=4 MAXCALLS=1
# Each ARG=VALUE overrides --build-arg ARGMATCH_ARG; the rest are the
# defaults below (from prometheus-test-infra/prombench/build_prom_argmatch.sh).
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

# 4. Wait for the first alert sent to alertmanager.
ready=; sent=0
while :; do
    now=$(( $(date +%s) - start ))
    if [ -z "$ready" ] && kubectl --context $CTX exec deploy/prometheus-meta -c prometheus -- \
            wget -qO- localhost:9090/prometheus-meta/-/ready > /dev/null 2>&1; then
        ready=$now; log "ready after ${ready}s"
    fi
    sent=$(kubectl --context $CTX exec deploy/prometheus-meta -c prometheus -- \
            wget -qO- localhost:9090/prometheus-meta/metrics 2>/dev/null |
        awk '/^prometheus_notifications_sent_total/ {s += $2} END {print s + 0}' || true)
    sent=${sent:-0}
    [ "${sent%.*}" -gt 0 ] && break
    if [ $now -ge $TIMEOUT ]; then log "no alert after ${now}s"; break; fi
    sleep 5
done
alert=$(( $(date +%s) - start ))
[ "${sent%.*}" -gt 0 ] && log "first alert sent after ${alert}s"

# 5. Save the output and the pod's log.
sudo cp $OUTDIR/matches.csv $dir/matches.csv 2>/dev/null || : > $dir/matches.csv
sudo chown $(id -u):$(id -g) $dir/matches.csv
kubectl --context $CTX logs deploy/prometheus-meta -c prometheus > $dir/pod.log 2>&1 || true
rows=$(( $(wc -l < $dir/matches.csv) - 1 ))
log "saved $rows rows"
echo "RESULT name=$name build=${build}s load=${load}s ready=${ready:-never}s alert=${alert}s rows=$rows" | tee -a $dir/timing.txt
