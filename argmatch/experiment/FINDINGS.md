# Findings from the prombench experiments

What the runs so far have shown, for choosing argmatch's settings and for analyzing its output. [README.md](README.md) explains how to run the experiment. Each run's raw results are in `dump_ancestors/argmatch-experiment/<name>/`, and their summary lines in `summary.tsv` there.

## Fixed in every run

The functions and needles, and three settings:
- **Functions:** those in the stack log, `prometheus/argmatch-stacklog.txt` on the VM, from a prometheus-meta pod log of an earlier prombench run. 81 of them get instrumented.
- **Needles:** `prometheus/argmatch-matches.txt`, a copy of `prom-test-infra/prombench/param_values.txt`. That's 158 config values; `param_keys.txt` maps each to its config keys.
- **Settings:** `RECVONLY=1`, `EXACT=true`, `IGNORECLOSURES=true`.

## Runs before the sweep

**Best so far:** `MAXCALLS` 30, from the sweep below (depth 10, `MAXELEMS` 64, ignoring none, 360 s, viewer and reloads). Its two runs got 1,739 pairs, 137 needles and 38 types (`sweep-c30-1`, the best single run), and 1,387, 130 and 38 (`sweep-c30-2`). Both are above every other setting's runs. The gap between them is the 7 needles that depend on startup timing (see the sweep section).

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
| `reload-d10-e64-c3` (the sweep's starting point) | same | 240 s, viewer and config reloads | 1,298 | 130 | 35 |

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

`reload-d10-e64-c3` matched 130 of 158 needles. Of the 28 it missed, most can't be matched exactly under the fixed settings, because Prometheus doesn't keep the config's string as it is. They fall into these groups ([STRINGISH_TYPES.md](STRINGISH_TYPES.md) explains how Prometheus decodes each type):

| Needles | How Prometheus stores them | Would `EXACT=false` match? |
|---|---|---|
| `TLS12`, `TLS13` | **Not as a string:** `config.TLSVersion`, a `uint16`. argmatch only compares strings, `[]byte` and byte arrays. | No. Nothing would, short of comparing numbers. |
| 11 relabel and metric regexes, e.g. `(.+)`, `^$`, `go_gc_.+`, `up\|scrape_.+`, `prombench-(\d+);test-pr-\d+` | **A string that contains the needle:** `relabel.Regexp` compiles `^(?s:` + needle + `)$`, and `regexp.Regexp` keeps that text (e.g. `^(?s:(.+))$`). | Yes: it reports values that contain a needle. |
| `http://127.0.0.1:3128`, `https://kubernetes.default.svc:443` | **Strings that the needle contains:** `config.URL` holds a `*url.URL` with the parts in separate fields (`Scheme` `http`, `Host` `127.0.0.1:3128`). | No: no stored string contains the whole needle. It would need needles split into their parts. |
| `10.0.0.0/8`, `172.16.0.0/12`, `.svc`, `.cluster.local` | `no_proxy` entries, probably kept joined into one string, or parsed. Not checked. | Yes if they're kept joined into one string. |
| The PEM certificate and private key | The needles contain escaped `\n`, while the stored values have real newlines. | No, but needles with real newlines would match exactly. |

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

Started 2026-10-29 on the VM with `sweep_on_vm.sh`, in tmux session `sweep`. Each run changes one setting from `reload-d10-e64-c3`, with the viewer and reloads on and a 240 s wait (360 s for the `MAXCALLS` runs). Results are in `dump_ancestors/argmatch-experiment/summary.tsv` and `sweep-*/`, copied from the VM before it was shut down, without the raw `matches.csv` files (48 GB). Stopped at 03:01 UTC: `sweep-d15-2` was stuck like `sweep-d15-1` and was killed, and the `DEPTH` 20 and `MAXELEMS` 256 runs weren't started.

| Run | Change | Pairs | Needles | Types | Notes |
|---|---|---|---|---|---|
| `sweep-base-1` | none (repeat of `reload-d10-e64-c3`) | 1,305 | 130 | 34 | last check 76 s after restart |
| `sweep-c10-1` | `MAXCALLS` 10 | 1,342 | 130 | 38 | 626 checks (base: 217); `still_checking=yes`, last check 343 s after restart |
| `sweep-c10-2` | `MAXCALLS` 10 | 1,363 | 130 | 38 | 639 checks; same 38 types as `sweep-c10-1`; last check 313 s after restart |
| **`sweep-c30-1`** | `MAXCALLS` 30 | **1,739** | **137** | 38 | 1,299 checks; all 7 "might be reachable" needles found; `still_checking=yes`, last check 342 s after restart |
| `sweep-c30-2` | `MAXCALLS` 30 | 1,387 | 130 | 38 | missed the 7: all 30 `testReady` calls came before the config was loaded; `still_checking=yes`, last check 350 s after restart |
| `sweep-d15-1` | `DEPTH` 15 | — | — | — | failed: hit the sweep's 1,200 s timeout before capturing; `matches.csv` was over 29.5 GB |
| `sweep-d15-2` | `DEPTH` 15 | — | — | — | killed after 12 minutes: ready after 131 s, first config reload blocked for 7.5 minutes |
| `sweep-c30-i5-1` | `MAXCALLS` 30, `CALLINTERVAL` 5s | 1,307 | 130 | 32 | 1,021 checks (`sweep-c30-2`: 1,322); lost 6 client-go types; `still_checking=yes` |
| `sweep-c30-i5-2` | `MAXCALLS` 30, `CALLINTERVAL` 5s | 1,264 | 130 | 30 | repeats `sweep-c30-i5-1`'s drop; `still_checking=yes` |

- **Variation at the baseline is small:** `sweep-base-1` matches `reload-d10-e64-c3` (1,298 pairs, 130 needles, 35 types) within 1%. Same settings, wait and simulated use. The earlier 10–20% differences were mostly between runs that also differed in wait or extras.
- **`MAXCALLS` 10, both runs:** 1,342 and 1,363 pairs, the same 38 types in both. That's a small but repeatable gain over the baseline (1,298–1,305 pairs, 34–35 types).
- **`MAXCALLS` 10, first run:** about 3% more pairs and 4 more types than the baseline, the same 130 needles. None of the "might be reachable" needles appeared, so later calls of the same functions don't reach them, at least not up to 10 calls.
  - The 4 new types are all from client-go's Kubernetes discovery: `gentype.Client[*v1.Pod]`, `gentype.alsoLister[*v1.Endpoints,...]`, `cache.controller`, `cache.sharedIndexInformer`. No types were lost.
  - **`still_checking=yes` here doesn't mean it was still finding things.** The checks after 2 minutes were periodic: notifier sends, DNS refreshes and dashboard queries, then client-go watches being re-established (the last check, at 343 s). With `MAXCALLS` 10, such periodic functions keep being checked for minutes. A longer wait would mostly add more of the same.
- **`MAXCALLS` 30 reaches the 7 top-level config needles** (`/data/scrape_failures.log`, the two config globs, the 3 OTLP settings, `OpenMetricsText0.0.1`). It's a jump to 1,739 pairs and 137 needles, with the same 38 types as `MAXCALLS` 10.
  - **All 7 are found only through `web.Handler`, in `web.(*Handler).testReady`.** That confirms the guess under "Needles that weren't matched".
  - **`testReady` only runs at startup.** It isn't the readiness check on each request: it wraps a route's handler, and is called once per route when the routes are registered (in `web.New` and `api_v1.(*API).Register`, about 40 routes). The closure it returns is what runs on each request, and it isn't instrumented separately.
  - **So the 7 needles appear only when route registration overlaps the config being loaded,** and that's caused by argmatch itself. Each `testReady` check takes about 0.2 s, which slows the registration down, so the last routes can be registered after the config is loaded. Without argmatch, registration takes milliseconds.
    - In `sweep-c30-1`, calls 1–25 came in the 6 s before the config was loaded (61–68 rows each). Calls 26–30 came after (1,348, then 6,044 to 14,063 rows each).
    - In `sweep-c30-2`, the config took 11 s to load instead of 6, and all 30 checked calls came before it (177–219 rows each). Without the 7 needles, it got 1,387 pairs, close to `MAXCALLS` 10.
    - In `sweep-c10-2`, all 10 calls came before the config was loaded.
  - **Getting them reliably** would need `testReady` to be checked on all its calls (about 40), so `MAXCALLS` of at least 40, but even that depends on startup timing. Or it would need an instrumented function that gets the `web.Handler` later on. With `RECVONLY` and the fixed function list, there doesn't seem to be one.
  - **`-callinterval` doesn't help here.** It was added (argmatch commit 4ce9ea1) on the wrong assumption that `testReady` runs on every request. After a function is checked, its calls within the interval aren't checked and don't count towards `MAXCALLS`. All of `testReady`'s calls come within a few seconds, so with `CALLINTERVAL` 5s only the first is checked. That's confirmed in `sweep-c30-i5-1`: one `testReady` check, at startup. The setting still spreads out the checks of periodic functions, which the `sweep-c30-i5-*` runs measure.
- **`CALLINTERVAL` 5s (with `MAXCALLS` 30) is worse, not better:** 1,307 and 1,264 pairs, 32 and 30 types in the two runs, against 1,387 and 38 in `sweep-c30-2`.
  - **The interval is per function, not per receiver.** Many functions are called in a burst on different receivers: one call per informer (`sharedIndexInformer.Run`, `ListWatch.List`: 9 calls → 1), per scrape loop (`scrapeLoop.run`: 30 → 3), per remote-write shard (`shards.runShard`: 14 → 4), per route (`testReady`, `Router.handle`: 30 → 1). Only the first call of each burst is checked.
  - **The 6 types lost** are client-go's informers for Endpoints and Nodes: `gentype.Client[*v1.Endpoints]`, `gentype.Client[*v1.Node]`, the two `alsoLister`s, `cache.controller`, `cache.sharedIndexInformer`. Only one informer's calls were checked.
  - No function was checked more often than in `sweep-c30-2`. Truly periodic functions, like DNS refreshes, were checked about as often as without the interval.
  - **A per-receiver interval** (keyed by the function and its receiver's address) would keep the bursts and still spread out repeated calls on the same receiver. It wouldn't help `testReady` (one `web.Handler`). Not implemented.
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
