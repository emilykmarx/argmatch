# Comparing argmatch build args on prombench

These scripts look for argmatch settings ("build args") that work well on a real application: Prometheus's prombench benchmark, running in a kind cluster on a VM. Each run builds prometheus-meta with one set of args, runs it under a realistic load, and measures how much argmatch found.

A setting is better when the output has many unique (needle, receiver type) pairs, with variety in both needles and types. That's the number of rows `needle_types.py` writes after `dedup.py`, and the number of distinct needles and types in them.

## Before you start

- **The VM:** `argmatch` in zone `us-central1-b`, project `conftamer`, reachable with `gcloud compute ssh`. It's set up with `prometheus-test-infra/prombench/vm_setup.sh`, which also installs Playwright with headless Chromium for the dashboard viewer. The repos are cloned on the VM at the same paths as locally, under `/home/emily/projects/config_tracing`.
- **Code on the VM:**
  - `dump_ancestors` must be on the branch to test, normally `main`.
  - The VM builds argmatch from its own checkout, so push (or copy) changes there before running.
- **prombench running:** deploy it with `prometheus-test-infra/prombench/deploy.sh`. Each run only replaces the prometheus-meta pod's image; it never redeploys the rest.
- **Input files** in `prometheus/` on the VM (see `vm_setup.sh`'s manual steps):
  - `argmatch-stacklog.txt`: the stack log whose functions are instrumented;
  - `argmatch-matches.txt`: the needles.
- **Locally:**
  - `prom-test-infra/prombench/param_keys.txt` maps needles to config keys, for `needle_types.py`;
  - matplotlib, only for `watch.sh`'s plots.

## Running one setting

```sh
cd dump_ancestors/argmatch/experiment
./run.sh d10-e64-c3 DEPTH=10 MAXELEMS=64 MAXCALLS=3
```

- **The name** (`d10-e64-c3`) names the run's results directory, `dump_ancestors/argmatch-experiment/d10-e64-c3/`. Reusing a name replaces that run's results.
- **Each `ARG=VALUE`** sets `--build-arg ARGMATCH_ARG`. Unset ones keep the defaults in `run_on_vm.sh`, which come from `prometheus-test-infra/prombench/build_prom_argmatch.sh`.
- **Some defaults narrow the search:** `IGNORELIBS=true`, `IGNORETYPES=config.Config`, `MAXMEM=1GB` and `MAXTIME=10s`. To search everything, pass `IGNORELIBS=false IGNORETYPES= MAXMEM=0 MAXTIME=0`.
- **Environment variables:**
  - `WAIT=seconds` changes how long the run lasts (default 240). `WAIT=alert` stops at prometheus-meta's first alert instead, the original rule, which turned out too short.
  - `VIEW=0` turns off the simulated dashboard viewer.
  - `RELOAD=0` turns off the config reloads.

A run takes about 5 minutes: up to about 3 minutes to build, 15–30 s to load the image, then the wait.

## What a run does

`run.sh` (locally) copies the helper scripts to the VM and runs `run_on_vm.sh` there:

1. **Build** `prometheus-conftamer:v3.2.1` from `prometheus/prombench-argmatch.Dockerfile`, with the run's args plus `ARGMATCH_STATS=true`.
   - Changing `FUNC`, `STACKLOG` (or the stack log file) or `RECVONLY` recompiles everything, about 3 minutes.
   - All the other args are run-time settings, so changing them only relinks, under a minute.
   - Docker reuses its cached image when the args are the same as an earlier build.
2. **Load** the image into the kind node prometheus-meta runs on.
3. **Restart prometheus-meta** with an empty output directory: scale its deployment to 0, delete `argmatch-out/matches.csv`, scale back to 1. Scaling down first means the old pod has stopped writing and released its storage before the new one starts.
4. **Simulate a person** using prometheus-meta, as in the prombench run the stack log came from:
   - **Dashboards:** once prometheus-meta is ready, `view_dashboards.py` loads Grafana's dashboards in headless Chromium, cycling through them until the capture, so Grafana queries prometheus-meta. Without it, the query API's functions (`web/api/v1.(*API).query`, `queryRange`, `respond`, the compression handler) are never called.
   - **Config reloads:** 30 s after prometheus-meta is ready, `reload_on_vm.sh` changes the config and reloads it, then restores the original and reloads again. Without it, `scrape.(*scrapePool).reload`, `restartLoops` and `headersRoundTripper.CloseIdleConnections` are never called.
   - **How the reload change is made:** it sets one boolean in the first scrape job (e.g. `honor_labels: true`), which can't match a needle. The config map is put back as it was, even if the script fails partway.
5. **Wait** until `WAIT` seconds after the restart.
6. **Capture:** copy `matches.csv` and the pod's log, and record how long before the capture argmatch's last check was.

`run.sh` then copies the results back and runs `analyze.sh`: `dedup.py`, then `needle_types.py`, then `stats.py`, which appends one line to `argmatch-experiment/summary.tsv`.

## Results

Each run's directory, `dump_ancestors/argmatch-experiment/NAME/`, has:

| File | Contents |
|---|---|
| `args.txt` | every `ARGMATCH_*` build arg used |
| `timing.txt` | timestamped steps, ending with a `RESULT` line |
| `build.log` | the Docker build's output |
| `matches.csv` | argmatch's raw output |
| `deduped.csv`, `needle_types.csv` | after `dedup.py` and `needle_types.py` |
| `pod.log` | prometheus-meta's log, with timestamps. It includes argmatch's list of instrumented functions (`argmatch-instrumented:`) and one line per check (`argmatch-stats:`). |
| `unchecked.txt` | instrumented functions that were never checked |
| `viewer.log`, `reload.log` | what the dashboard viewer and the config reloads did |

`summary.tsv` has one tab-separated line per run, with `key=value` fields:

| Field | Meaning |
|---|---|
| `build`, `load` | seconds to build and load the image |
| `ready`, `alert` | seconds from restart until prometheus-meta was ready, and until its first alert |
| `captured` | seconds from restart until the output was copied |
| `last_check` | seconds between argmatch's last check and the capture |
| `still_checking` | `yes` if `last_check` was under 30 s: argmatch was probably still finding things, so this setting needs a longer `WAIT` |
| `unchecked` | number of instrumented functions never checked. `stats.py` also warns about them on stderr. |
| `raw_rows`, `dedup_rows` | rows in `matches.csv`, and after `dedup.py` |
| `pairs` | rows of `needle_types.csv`: unique (needle, receiver type) pairs |
| `needles`, `param_keys`, `types` | distinct values of each in `needle_types.csv` |

## Choosing how long to wait

`watch.sh` runs a setting like `run.sh`, but samples it every minute and stops once the number of checks and of pairs have both stayed the same for 5 minutes, or after 30 minutes. It plots the samples to `timeseries.png` as it goes:
- checks;
- functions checked, at `MAXCALLS` and instrumented;
- pairs, needles and types.

```sh
./watch.sh watch-d10-e64-c3 DEPTH=10 MAXELEMS=64 MAXCALLS=3 IGNORELIBS=false IGNORETYPES= MAXMEM=0 MAXTIME=0
```

That's how the default `WAIT` was chosen. At depth 10, `-maxelems 64` and `-maxcalls 3`:
- without the viewer and reloads, every check was within 66 s of the restart;
- with the viewer, within 86 s;
- the reloads add checks later on, so the default is 240 s.

Check `still_checking` in each run's summary line: a `yes` means that setting needs a longer `WAIT`.

## Things to know

- **Results vary between runs of the same setting:**
  - Map entries are walked in memory order, which depends on a hash seed Go randomizes for each process.
  - Which functions have run by the capture depends on Prometheus's timing.

  Single runs differed by about 10–20% in pairs, so compare settings over several runs if the difference is small.
- **`MAXCALLS` isn't always reached:** some functions run fewer than `MAXCALLS` times however long you wait (e.g. `ApplyConfig`, which runs at startup and on a reload). So "every function at `MAXCALLS`" isn't a usable stopping rule.
- **The VM's checkout is what's built.** Uncommitted local changes don't reach the VM until they're pushed or copied.
