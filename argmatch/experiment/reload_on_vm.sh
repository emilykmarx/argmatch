#!/bin/bash
# Runs on the argmatch VM, during an experiment run: makes prometheus-meta
# reload a changed config and then its original one, as a person editing
# the config would, so that the code that only runs on a config change is
# checked (e.g. scrape.(*scrapePool).reload, which only runs for a scrape
# pool whose config changed). The config map is left as it was.
#
# The change sets one boolean (honor_labels, honor_timestamps or
# enable_compression, whichever the first scrape job doesn't set) to its
# non-default value. A boolean can't match a needle, so the change adds no
# matches of its own. After each config map update, the pod is annotated so
# the kubelet updates its copy of the file promptly, and the reload is only
# sent once the pod's file has the new content.
#
# usage: reload_on_vm.sh LOGFILE
set -euo pipefail
log=$1
CTX=kind-prombench
FILE=/etc/prometheus/config/prometheus.yaml
k() { kubectl --context $CTX "$@"; }
say() { echo "$(date -u +%FT%TZ) $*" >> "$log"; }

tmp=$(mktemp -d)
k get configmap prometheus-meta -o jsonpath='{.data.prometheus\.yaml}' > $tmp/orig.yaml
toggled=$(python3 - $tmp/orig.yaml $tmp/changed.yaml <<'EOF'
import re, sys
y = open(sys.argv[1]).read()
first = re.search(r"(?m)^([ \t]*)- job_name: *\S+\n", y)
nxt = re.search(r"(?m)^[ \t]*- job_name:", y[first.end():])
block = y[first.end():first.end() + nxt.start()] if nxt else y[first.end():]
indent = first.group(1) + "  "
for key, value in [("honor_labels", "true"), ("honor_timestamps", "false"), ("enable_compression", "false")]:
    if not re.search(rf"(?m)^[ \t]*{key}:", block):
        break
else:
    sys.exit("reload_on_vm.sh: the first job already sets every boolean to toggle")
open(sys.argv[2], "w").write(y[:first.end()] + f"{indent}{key}: {value}\n" + y[first.end():])
print(f"{key}: {value}")
EOF
)
# put_config FILE: sets the config map's prometheus.yaml to FILE's content,
# leaving its metadata alone.
put_config() {
    python3 -c 'import json, sys; print(json.dumps({"data": {"prometheus.yaml": open(sys.argv[1]).read()}}))' $1 > $tmp/patch.json
    k patch configmap prometheus-meta --type merge --patch-file $tmp/patch.json > /dev/null
}
restore() { put_config $tmp/orig.yaml; }
trap 'restore; rm -rf $tmp' EXIT

# set_config FILE: updates the config map, waits until the pod's copy of the
# file matches, and reloads.
set_config() {
    local want start=$(date +%s)
    want=$(md5sum < $1 | cut -d' ' -f1)
    put_config $1
    while :; do
        k annotate pod -l app=prometheus-meta argmatch-sync="$(date +%s%N)" --overwrite > /dev/null
        have=$(k exec deploy/prometheus-meta -c prometheus -- md5sum $FILE 2>/dev/null | cut -d' ' -f1 || true)
        [ "$have" = "$want" ] && break
        if [ $(( $(date +%s) - start )) -ge 180 ]; then say "pod's config not updated after 180s"; return 1; fi
        sleep 3
    done
    say "pod has the new config after $(( $(date +%s) - start ))s"
    k exec deploy/prometheus-meta -c prometheus -- wget -qO- --post-data= \
        localhost:9090/prometheus-meta/-/reload > /dev/null
    ok=$(k exec deploy/prometheus-meta -c prometheus -- wget -qO- localhost:9090/prometheus-meta/metrics |
        awk '/^prometheus_config_last_reload_successful / {print $2}')
    say "reloaded: prometheus_config_last_reload_successful=$ok"
    [ "$ok" = 1 ]
}

say "reloading with '$toggled' added to the first scrape job"
set_config $tmp/changed.yaml
say "reloading the original config"
set_config $tmp/orig.yaml
say "done"
