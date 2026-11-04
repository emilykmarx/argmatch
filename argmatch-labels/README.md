<style>
pre, code { font-size: 0.8em; }
pre, table pre { white-space: pre !important; overflow-x: auto; overflow-wrap: normal !important; word-break: normal !important; }
.scrape { color: #e36209; font-weight: bold; }
.run { color: #8a3ffc; font-weight: bold; }
.arrow pre { color: gray; }
/* like a table header, but no padding or border, so the line keeps its height */
.rowhead { display: inline-block; width: 100%; text-align: left; font-weight: bold; background: rgba(128, 128, 128, 0.25); }
</style>

# Joining argmatch and contexttrack output
Join argmatch's output (matches to a value found in an argument's contents, aka "calls") with
contexttrack's output (HTTP message sends/receives, aka "events").

Each call/message is assumed to have output from runtime.Stack() with GODEBUG=tracebackancestors,
split into a "stack" (the stack of the goroutine that logged the call/message) and
an "ancestry" (where the logging goroutine was created, and its ancestors' stacks
when they created their children).

A "chain" is the stack plus the ancestry, ordered from newest
(for argmatch: the checked function "F"; for contexttrack: the function that logged the message)
to oldest (program start).

See the argmatch [README](../argmatch/README.md#call-ids) for how it identifies chains (to limit the length of the matches file, it writes chain info to a separate file).

## Join condition
A call and a message join if both are true:
1. The message's chain has a frame running F (same function, any line)
2. Every frame older than that frame in the message's chain is "identical" to every
   frame older than F in the call's chain. "Identical" means same goroutine ID and code location.

## Output
After joining the messages and calls, output csv:
- `message_label`: the message label as contexttrack would apply it, by calling `analysis/message_graph.py`
     - e.g. `{api_id: github.com/prometheus, method: GET, path: /metrics}`
- `message_kind`: as in contexttrack
     - e.g. `Request sent`
- `needle`: the value argmatch found

NOTE this drops the following:
- Any messages that don't join a call, and vice versa - i.e. (where "lineage" is the joined portion of the chain)
     - Any messages that don't share a lineage with any matches
     - Any matches that don't share a lineage with any messages
          - e.g. matches in web/api/v1.(*API).Register, which has access to the API but doesnt
- Any events contexttrack does not treat as messages (in `analysis/message_graph.py`), e.g. ones without a context ID.

## Join example
The "scrape request sent" message below joins argmatch matches in these functions:
- `targetScraper.scrape`, `scrapeLoop.scrapeAndReport`, `scrapeLoop.run`
   - Match found in the same goroutine that logged the message (the one running `http.Do()`) - i.e. joined by a frame in the `http.Do()` goroutine's stack
- `Manager.Run`, `Manager.reloader`, `Manager.reload`, `scrapePool.Sync`, `scrapePool.sync`
   - Match found in the ancestors of the `http.Do()` goroutine - i.e. joined by a frame in an ancestor of the `http.Do()` goroutine

Below are example matches from `targetScraper.scrape` (call ID 57) and `Manager.Run` (call ID 17) (with some frames elided).

In the chains for Message (left) and `targetScraper.scrape` in argmatch (right), the frames before the orange one are identical.

Same for `Manager.Run` in argmatch with the purple frame.

<table>
<tr><th></th><th>Message Label</th><th>Argmatch Matches</th></tr>
<tr valign="top"><td></td><td>

<!-- message (Scrape request sent) -->
<pre>{
 &quot;kind&quot;: &quot;Request sent&quot;,
 &quot;request_id&quot;: {
  &quot;method&quot;: &quot;GET&quot;,
  &quot;host&quot;: &quot;localhost:9090&quot;,
  &quot;path&quot;: &quot;/metrics&quot;
 },

</td><td>


<!-- MATCHES (tagged by call ID) - first one in "types" is the recv -->
<pre>
**matches.csv**
needle,function,expression,call,types

<span class="rowhead">targetScraper.Scrape</span>
/metrics,scrape.(*targetScraper).scrape,s.Target.scrapeConfig.MetricsPath,57,scrape.targetScraper,...
__address__,scrape.(*targetScraper).scrape,"s.Target.tLabels<key ""__address__"">",57,scrape.targetScraper,...

<span class="rowhead">Manager.Run</span>
/metrics,scrape.(*Manager).Run,&quot;m.scrapeConfigs[&quot;&quot;prombench&quot;&quot;].MetricsPath&quot;,17,scrape.Manager,...
OpenMetricsText1.0.0,scrape.(*Manager).Run,&quot;m.scrapeConfigs[&quot;&quot;prombench&quot;&quot;].ScrapeProtocols[0]&quot;,17,scrape.Manager,...

<!-- call ID => function,ancestry,stack -->
**matches.calls.csv**
call,function,ancestry,stack

57,scrape.(*targetScraper).scrape,...
17,scrape.(*Manager).Run,...

</td></tr>
<tr><th></th><th>Message Chain</th><th>Argmatch Chain</th></tr>
<tr valign="top"><td class="arrow">

<pre>
  │
  │
  ▼
older frames</pre>

</td><td>

<!-- message Chain: Stack (some http frames removed), then Ancestry -->
<pre> goroutine 486 [running]:
 net/http.(*Client).Do(...)
  /usr/local/go/src/net/http/client.go:593
<span class="scrape"> scrape.(*targetScraper).scrape(...) 🟧
  /src/scrape/scrape.go:820</span>
 scrape.(*scrapeLoop).scrapeAndReport(...)
  /src/scrape/scrape.go:1426
 scrape.(*scrapeLoop).run(...)
  /src/scrape/scrape.go:1351
created by scrape.(*scrapePool).sync in goroutine 619
  /src/scrape/scrape.go:597
[originating from goroutine 619]:
scrape.(*scrapePool).sync(...)
  /src/scrape/scrape.go:597
scrape.(*scrapePool).Sync(...)
  /src/scrape/scrape.go:474
scrape.(*Manager).reload.func1(...)
  /src/scrape/manager.go:203
created by scrape.(*Manager).reload
  /src/scrape/manager.go:201
[originating from goroutine 467]:
scrape.(*Manager).reload(...)
  /src/scrape/manager.go:201
scrape.(*Manager).reloader(...)
  /src/scrape/manager.go:158
created by scrape.(*Manager).Run
  /src/scrape/manager.go:122
[originating from goroutine 339]:
<span class="run"> scrape.(*Manager).Run(...) 🟪
  /src/scrape/manager.go:122</span>
main.main.func17(...)
  /src/cmd/prometheus/main.go:1082
github.com/oklog/run.(*Group).Run.func1(...)
  /go/pkg/mod/github.com/oklog/run@v1.1.0/group.go:38
created by github.com/oklog/run.(*Group).Run
  /go/pkg/mod/github.com/oklog/run@v1.1.0/group.go:37
[originating from goroutine 1]:
github.com/oklog/run.(*Group).Run(...)
  /go/pkg/mod/github.com/oklog/run@v1.1.0/group.go:36
main.main.func34(...)
  /src/cmd/prometheus/main.go:1370
main.main.func34(...)
  /src/cmd/prometheus/main.go:1370</pre>

</td><td>

<!-- targetScraper.scrape and Manager.Run Chains, padded so their colored frames line up with the Message Chain's -->
<pre>

<span class="rowhead">targetScraper.scrape</span>
goroutine 486 [running]:
<span class="scrape">scrape.(*targetScraper).scrape(...) 🟧
  /src/scrape/scrape.go:806</span>
scrape.(*scrapeLoop).scrapeAndReport(...)
  /src/scrape/scrape.go:1426
scrape.(*scrapeLoop).run(...)
  /src/scrape/scrape.go:1351
(Rest is the same as Message Chain, starting with sync in goroutine 619)
















<span class="rowhead">Manager.Run</span>
goroutine 339 [running]:
<span class="run">scrape.(*Manager).Run(...) 🟪
  /src/scrape/manager.go:121</span>
main.main.func17(...)
  /src/cmd/prometheus/main.go:1082
github.com/oklog/run.(*Group).Run.func1(...)
  /go/pkg/mod/github.com/oklog/run@v1.1.0/group.go:38
created by github.com/oklog/run.(*Group).Run in goroutine 1
  /go/pkg/mod/github.com/oklog/run@v1.1.0/group.go:37
[originating from goroutine 1]:
github.com/oklog/run.(*Group).Run(...)
  /go/pkg/mod/github.com/oklog/run@v1.1.0/group.go:36
main.main.func34(...)
  /src/cmd/prometheus/main.go:1370
main.main.func34(...)
  /src/cmd/prometheus/main.go:1370</pre>

</td></tr>
</table>

## Other possible join conditions

- With this join policy, a match can join with no message. In these cases we've seen thus far, this seems correct
- If later we find we're missing joins we want, we can revisit a looser policy that allows more divergence between the chains.
     - I.e., some older prefix matches, but the *call* has a newer postfix that doesn't appear in a message ancestry.
     - (Note the message ancestry will generally always have a newer postfix, since logging happens at a low layer - see join example above.)
- But thus far it seems looser rules at best add redundant joins, and at worst add joins we don't want.
- E.g. we tried requiring that the postfix is in the same goroutine:
     - Joins calls whose match info is already captured by different calls (redundant)
     - Ends up treating matches for web responses as influencing the corresponding request (unwanted - they should influence the response, and do under the current policy)

## Running argmatch and contexttrack together

The join logic assumes argmatch and contexttrack were running in the same process and the same binary, for two reasons:
- It compares goroutine IDs, which are per-process
- It compares code locations by PC offset, which can change depending on the version of argmatch the binary was built with

### Bootstrapping

argmatch decides what to instrument when the program is compiled (from a stack log, `-stacklog`), but the functions come from contexttrack's chains, which only exist once the program has run. So it takes two runs, both built with argmatch and contexttrack (see above):
1. contexttrack records chains, while argmatch does nothing
2. argmatch instruments the functions recorded in the first run
     - [events_to_stacklog.py](events_to_stacklog.py) parses events into a stack log

For an example, see [bootstrap-example](bootstrap-example), which builds Prometheus and runs it twice in a standalone container with a workload including scrapes, queries, and config reload.
