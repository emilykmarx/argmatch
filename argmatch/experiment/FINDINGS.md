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

These are top-level config fields, held in the loaded `config.Config`. With `RECVONLY`, they'd be reached through a receiver holding that config (e.g. `web.Handler`). Those receivers' first calls happen at startup, before the config is applied, so with `MAXCALLS` 3 the later calls (after `ApplyConfig` or a reload) aren't checked. That's a guess, not verified.

## Variation between runs

- **Single runs of the same setting differ** by roughly 10–20% in pairs. For example, the deep setting gave 906–1,331 pairs across runs, although those runs also differed in wait and extras.
- **Why:**
  - Map entries are walked in memory order, which depends on Go's per-process hash seed. So with `MAXELEMS` below a map's size, different runs see different entries.
  - Which functions have run by the capture depends on Prometheus's timing.
- **Raw rows are a poor measure:** they're dominated by the same values reached around reference cycles (e.g. a `QueueManager`'s `watcher.writer` loops). Judge settings by pairs, needles and types.

## Next: settings sweep (planned)

Starting from `reload-d10-e64-c3`, change one setting at a time, with two runs each:
- `MAXCALLS` 10 and 30 (avoid 0: some functions run every 5 s per target);
- `DEPTH` 15 and 20;
- `MAXELEMS` 256.

Then combine whichever help, and check whether the "might be reachable" needles above appear.

The other settings are already at their most permissive: `MAXFIELDS=-1`, `IGNORELIBS=false`, empty `IGNORETYPES`, `MAXMEM=0` and `MAXTIME=0`.
