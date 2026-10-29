# Findings from the prombench experiments

What the runs so far have shown, for choosing argmatch's settings and for analyzing its output. [README.md](README.md) explains how to run the experiment. Each run's raw results are in `dump_ancestors/argmatch-experiment/<name>/`, and their summary lines in `summary.tsv` there.

## Fixed in every run

The functions and needles, and three settings:
- **Functions:** those in the stack log, `prometheus/argmatch-stacklog.txt` on the VM, from a prometheus-meta pod log of an earlier prombench run. 81 of them get instrumented.
- **Needles:** `prometheus/argmatch-matches.txt`, a copy of `prom-test-infra/prombench/param_values.txt`. That's 158 config values; `param_keys.txt` maps each to its config keys.
- **Settings:** `RECVONLY=1`, `EXACT=true`, `IGNORECLOSURES=true`.

## Runs so far

Pairs are rows of `needle_types.csv`: unique (needle, receiver type) pairs. "Ignoring none" means `IGNORELIBS=false IGNORETYPES= MAXMEM=0 MAXTIME=0`; the walker is the raw-memory one on `main` unless noted.

| Run | Settings | Wait, extras | Pairs | Needles | Types |
|---|---|---|---|---|---|
| `d2-e4-c1` | depth 2, `MAXELEMS` 4, `MAXCALLS` 1 (`reflect-port` build) | first alert (16 s) | 84 | 47 | 17 |
| `main-d2-e4-c1` | same, ignoring none | first alert (24 s) | 90 | 48 | 18 |
| `wait150-d2-e4-c1` | same | 150 s | 84 | 45 | 17 |
| `d10-e64-c3` | depth 10, `MAXELEMS` 64, `MAXCALLS` 3 (`reflect-port` build) | first alert (17 s) | 906 | 125 | 29 |
| `main-d10-e64-c3` | same, ignoring none | first alert (43 s) | 1,097 | 123 | 28 |
| `watch-d10-e64-c3` | same | sampled to 9.6 min | 1,005 | 125 | 33 |
| `view-d10-e64-c3` | same | 150 s, dashboard viewer | 1,331 | 127 | 34 |
| **`reload-d10-e64-c3`** (best so far) | same | 240 s, viewer and config reloads | 1,298 | 130 | 35 |

The `reflect-port` builds read the same values as `main`; only `MAXFIELDS`, `IGNORELIBS`, `IGNORETYPES`, `MAXMEM` and `MAXTIME` were missing there.

## How long a run needs

- **Waiting for the first alert is too short.** It comes 16–52 s after the restart, while checks go on later.
- **Without the viewer and reloads:** in `watch-d10-e64-c3`, every check was within 66 s of the restart, and the pair count was already final at the first sample (1 minute). The last checks were from DNS service discovery.
- **With them:** in `view-d10-e64-c3` the last check was at about 86 s, and in `reload-d10-e64-c3` at about 79 s. The default `WAIT` of 240 s leaves plenty of margin for depth 10, `MAXCALLS` 3. Raising `MAXCALLS` will likely push checks later; watch `still_checking`.
- **`MAXCALLS` isn't always reached:** at `MAXCALLS` 3, about 56–65 of the 81 functions reach it. The rest run only once or twice in a run, e.g. `ApplyConfig`, which runs at startup and on a reload.

## Coverage of the instrumented functions

- **Without simulated use,** 8 of 81 functions were never called:
  - the query API: `web/api/v1.(*API).query`, `queryRange`, `respond`;
  - `util/httputil.CompressionHandler.ServeHTTP` and `(*compressedResponseWriter).Write`;
  - the reload path: `scrape.(*scrapePool).reload` and `restartLoops`, and `common/config.(*headersRoundTripper).CloseIdleConnections`.
- **Grafana doesn't query prometheus-meta on its own,** only when a dashboard is open. The dashboard viewer (`view_dashboards.py`) covers the first five: about 10 panel queries per dashboard visit, through Grafana's two dashboards.
- **The config reloads** (`reload_on_vm.sh`) cover the last three. Prometheus only rebuilds a scrape pool, and its HTTP client, when that pool's config changed, so the reload has to change something.
- **With both, all 81 were checked.** `stats.py` now warns about any left unchecked.

## Needles that weren't matched

`reload-d10-e64-c3` matched 130 of 158 needles. Of the 28 it missed, most can't be matched exactly under the fixed settings, because Prometheus doesn't keep the config's string:

| Needles | Why not, with `EXACT=true` |
|---|---|
| 11 relabel and metric regexes, e.g. `(.+)`, `^$`, `go_gc_.+`, `up\|scrape_.+`, `prombench-(\d+);test-pr-\d+` | Kept as compiled regexps, whose stored source is anchored, e.g. `^(?s:(.+))$` |
| `TLS12`, `TLS13` | Parsed into numeric TLS versions |
| `http://127.0.0.1:3128`, `https://kubernetes.default.svc:443` | Parsed into URL structs |
| `10.0.0.0/8`, `172.16.0.0/12`, `.svc`, `.cluster.local` | `no_proxy` entries, probably kept joined into one string, or parsed |
| The PEM certificate and private key | The needles contain escaped `\n`, while the stored values have real newlines |

These would need needles in their runtime form, or `EXACT=false`.

The rest might be reachable with other settings:
- `/data/scrape_failures.log`, `/etc/prometheus/alerts/*.yml`, `/etc/prometheus/config/scrape_config_*.yml`
- `service.instance.id`, `k8s.pod.name`, `NoUTF8EscapingWithSuffixes` (OTLP settings)
- maybe `OpenMetricsText0.0.1`

These are top-level config fields, held in the loaded `config.Config`. With `RECVONLY`, they'd be reached through a receiver holding that config (e.g. `web.Handler`). Those receivers' first calls happen at startup, before the config is applied, so with `MAXCALLS` 3 the later calls (after `ApplyConfig` or a reload) aren't checked. **Confirmed by `sweep-c30-1`:** all 7 were found through `web.Handler` in `testReady`, once `MAXCALLS` was high enough to reach calls after the config was loaded (see the sweep section).

## Variation between runs

- **Single runs of the same setting differ** by roughly 10–20% in pairs. For example, the deep setting gave 906–1,331 pairs across runs, although those runs also differed in wait and extras.
- **Why:**
  - Map entries are walked in memory order, which depends on Go's per-process hash seed. So with `MAXELEMS` below a map's size, different runs see different entries.
  - Which functions have run by the capture depends on Prometheus's timing.
- **Raw rows are a poor measure:** they're dominated by the same values reached around reference cycles (e.g. a `QueueManager`'s `watcher.writer` loops). Judge settings by pairs, needles and types.

## Settings sweep (stopped after `sweep-d15-2`)

Started 2026-10-29 on the VM with `sweep_on_vm.sh`, in tmux session `sweep`. Each run changes one setting from `reload-d10-e64-c3`, with the viewer and reloads on and a 240 s wait (360 s for the `MAXCALLS` runs). Results are on the VM in `argmatch-experiment/summary.tsv` and `sweep-*/`. Stopped at 03:01 UTC: `sweep-d15-2` was stuck like `sweep-d15-1` and was killed, and the `DEPTH` 20 and `MAXELEMS` 256 runs weren't started.

| Run | Change | Pairs | Needles | Types | Notes |
|---|---|---|---|---|---|
| `sweep-base-1` | none (repeat of `reload-d10-e64-c3`) | 1,305 | 130 | 34 | last check 76 s after restart |
| `sweep-c10-1` | `MAXCALLS` 10 | 1,342 | 130 | 38 | 626 checks (base: 217); `still_checking=yes`, last check 343 s after restart |
| `sweep-c10-2` | `MAXCALLS` 10 | 1,363 | 130 | 38 | 639 checks; same 38 types as `sweep-c10-1`; last check 313 s after restart |
| **`sweep-c30-1`** | `MAXCALLS` 30 | **1,739** | **137** | 38 | 1,299 checks; all 7 "might be reachable" needles found; `still_checking=yes`, last check 342 s after restart |
| `sweep-c30-2` | `MAXCALLS` 30 | 1,387 | 130 | 38 | missed the 7: all 30 `testReady` calls came before the config was loaded; `still_checking=yes`, last check 350 s after restart |
| `sweep-d15-1` | `DEPTH` 15 | — | — | — | failed: hit the sweep's 1,200 s timeout before capturing; `matches.csv` was over 29.5 GB |
| `sweep-d15-2` | `DEPTH` 15 | — | — | — | killed after 12 minutes: ready after 131 s, first config reload blocked for 7.5 minutes |

- **Variation at the baseline is small:** `sweep-base-1` matches `reload-d10-e64-c3` (1,298 pairs, 130 needles, 35 types) within 1%. Same settings, wait and simulated use. The earlier 10–20% differences were mostly between runs that also differed in wait or extras.
- **`MAXCALLS` 10, both runs:** 1,342 and 1,363 pairs, the same 38 types in both. That's a small but repeatable gain over the baseline (1,298–1,305 pairs, 34–35 types).
- **`MAXCALLS` 10, first run:** about 3% more pairs and 4 more types than the baseline, the same 130 needles. None of the "might be reachable" needles appeared, so later calls of the same functions don't reach them, at least not up to 10 calls.
  - The 4 new types are all from client-go's Kubernetes discovery: `gentype.Client[*v1.Pod]`, `gentype.alsoLister[*v1.Endpoints,...]`, `cache.controller`, `cache.sharedIndexInformer`. No types were lost.
  - **`still_checking=yes` here doesn't mean it was still finding things.** The checks after 2 minutes were periodic: notifier sends, DNS refreshes and dashboard queries, then client-go watches being re-established (the last check, at 343 s). With `MAXCALLS` 10, such periodic functions keep being checked for minutes. A longer wait would mostly add more of the same.
- **`MAXCALLS` 30 reaches the 7 top-level config needles** (`/data/scrape_failures.log`, the two config globs, the 3 OTLP settings, `OpenMetricsText0.0.1`). It's a jump to 1,739 pairs and 137 needles, with the same 38 types as `MAXCALLS` 10.
  - **All 7 are found only through `web.Handler`, in `web.(*Handler).testReady`,** the readiness wrapper around every HTTP request. That confirms the guess under "Needles that weren't matched".
  - **What matters is how many calls happen before the config is loaded.** In `sweep-c30-1`, `testReady` was called 25 times in the 6 s before the config was loaded, finding 61–68 rows each. Calls 26–30 came after, finding 1,348 rows, then 6,044 to 14,063 rows each. In `sweep-c10-2` all 10 calls happened before the config was loaded.
  - **So it depends on timing.** The number of calls before the config is loaded depends on how long startup takes, and on the HTTP requests that arrive meanwhile (probes, scrapes of prometheus-meta). A run with a slower start could use up 30 calls before then and miss the 7 needles.
  - **That's what happened in `sweep-c30-2`.** The config took 11 s to load, against 6 s in `sweep-c30-1`. `testReady` was checked back to back, about 5 times a second (each check takes about 0.2 s), so all 30 calls happened in the 6 s before the config loaded, finding 177–219 rows each. Without those 7 needles it's 1,387 pairs, close to `MAXCALLS` 10.
  - **So a higher `MAXCALLS` alone isn't a reliable way to get them.** The number needed scales with startup time (about 5 calls per second of startup), and every extra call of the periodic functions makes the output bigger.
  - **Possible argmatch change:** let checks of a function be spread out in time, for example at most one check per function every N seconds, or a check on the first call and then on calls at increasing intervals. With a gap of a few seconds, the first handful of `testReady` checks would cover the time after the config loaded, whatever the startup time.
  - **Implemented locally as `-callinterval`** (`CALLINTERVAL`, a duration, default 0), uncommitted. After a function is checked, its calls in the next interval aren't checked and don't count towards `MAXCALLS`. It's a run-time setting, so changing it only relinks. It won't reach the VM until the sweep ends. To try next: `MAXCALLS` 30 with `CALLINTERVAL` 5s, twice, against `sweep-c30-*`.
  - **These later calls are expensive:** about 2 s and 14,000 raw rows each. That's most of the jump in raw rows, from 1.6 million to 5.7 million.
  - **Analysis cost:** at `MAXCALLS` 30, `matches.csv` is about 4 GB. Analyzing a run took about 10 minutes, so each run took 18 minutes instead of 8. `stats.py` alone used 31 GB of memory, because it parsed the whole file just to count rows. Locally, `stats.py` now takes the count from `run_on_vm.sh` instead; that change won't reach the VM until the sweep ends.
- **`DEPTH` 15 (with `MAXELEMS` 64, `MAXCALLS` 3) is too expensive to run.**
  - Prometheus took 57 s to become ready (6–24 s at depth 10).
  - The two config reloads blocked for 10.5 and 5 minutes, since the checks on the reload path ran that long. `run_on_vm.sh` waits for the reloads before capturing, so the capture only started at 17 minutes, and the 1,200 s timeout killed it partway through copying.
  - The partial `matches.csv` was 29.5 GB, against 0.1–4 GB at depth 10. `sweep-d15-2` wrote 500 MB in its first 2 minutes.
  - **So the walk grows steeply past depth 10,** as expected from up to `MAXELEMS`^depth work. Depth 15 would need a much smaller `MAXELEMS`, or `MAXTIME`/`MAXMEM` caps, which would make it a different setting. Depth 20 can only be worse.
  - **Script bugs this exposed,** fixed locally (not on the VM until the sweep ends):
    - `sweep_on_vm.sh` logged `FAILED (exit 0)`, because `$?` after `if !` is always 0.
    - `run_on_vm.sh` meant to give the reloads at most 5 minutes, but then waited for them without a limit.
    - `run_on_vm.sh` copied `matches.csv` only after the reloads finished, while argmatch kept writing to it, so the copy didn't match the recorded capture time.

## Planned sweep

Starting from `reload-d10-e64-c3`, change one setting at a time, with two runs each:
- `MAXCALLS` 10 and 30 (avoid 0: some functions run every 5 s per target);
- `DEPTH` 15 and 20;
- `MAXELEMS` 256.

Then combine whichever help, and check whether the "might be reachable" needles above appear.

The other settings are already at their most permissive: `MAXFIELDS=-1`, `IGNORELIBS=false`, empty `IGNORETYPES`, `MAXMEM=0` and `MAXTIME=0`.
