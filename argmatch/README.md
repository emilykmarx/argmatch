# argmatch

`argmatch` is a `go build -toolexec` wrapper. It makes a Go program report each call to a given function whose receiver or arguments contain a given string. For each match it shows the named types on the path to the match.

## Usage

From this directory, to run it on the example in [testdata/example/main.go](testdata/example/main.go):

```sh
# Build the wrapper (once, or after changing it)
go build -o argmatch .

# Build the example with the wrapper, then run it
A=$PWD/argmatch
(cd testdata/example && go build -toolexec "$A -func main.ID.F -match needle -depth 5" -o /tmp/example .) && /tmp/example
```

The settings can also come from the environment, which is how to use it with a build that runs `go build` itself (`GOFLAGS` values can't contain spaces):

```sh
ARGMATCH_FUNC=main.ID.F ARGMATCH_MATCH=needle ARGMATCH_DEPTH=5 ARGMATCH_OUT=/tmp/argmatch.txt GOFLAGS=-toolexec=$A go build .
```

To check against several strings, put them in a file, one per line, and pass it with `-matchfile` (env `ARGMATCH_MATCHFILE`). Every value is checked against every string, and each output line names the string that matched. `-matchfile` can be combined with `-match`. The file's path must be absolute, because the go command runs the wrapper in each package's directory. Lines are used exactly as written, including leading and trailing spaces; blank lines and lines starting with `#` are ignored, so a match string can't start with `#`. The strings are read at build time and linked into the program, so after editing the file, rebuild (which only relinks) or override them when the program runs (see "How it works").

```sh
printf 'needle\needl\n' > /tmp/matches.txt
# -exact=false so that "needl" matches "needle" too
(cd testdata/example && go build -toolexec "$A -func main.ID.F -matchfile /tmp/matches.txt -exact=false" -o /tmp/example .) && /tmp/example
```

To check every function that appears in a log of connection stacktraces, pass the log with `-stacklog` (env `ARGMATCH_STACKLOG`), instead of or as well as `-func`. Like `-matchfile`, the path must be absolute and the log is read at build time. The log is parsed with `stacklog`, the parser shared with conftamer's `parse` package, from `../../golang.org-x-tools/gopls/conftamer/stacklog`. Functions are chosen the same way `parse` chooses them:

- A closure (`pkg.F.func1`) means the function that defines it (`pkg.F`). See the notes below for the details.
- Functions that `stacklog.IgnoreFn` ignores are skipped. That's the standard library, gRPC, `golang.org/x/net`, `klauspost/compress`, `main.main` and some k8s client packages. A function given with `-func` is always checked.
- Functions without parameters or a receiver are skipped, since there's nothing to check.

```sh
(cd testdata/example && go build -toolexec "$A -stacklog $PWD/../stacks.log -match needle" -o /tmp/example .) && /tmp/example
```

Parsing a large log takes about a second, and the go command runs the wrapper once per package. So the list of functions is cached in `~/.cache/argmatch/`, keyed on the log's path, size and modification time.

The wrapper path must be absolute, which is why it's set from `$PWD`. Matches are printed while the program runs, not during the build. By default they go to stderr. With `-out file` (env `ARGMATCH_OUT`) the program writes them to that file instead, so they can't be interleaved with the program's own stderr output. The file is overwritten when the program first calls an instrumented function, so each run replaces the previous run's output. A run that never calls one leaves the old file alone. Give each process its own `-out` file: two processes writing to the same one (e.g. two copies of the program, or two containers with the same output directory mounted) overwrite each other's output. A relative path is resolved against the program's working directory when it runs, and if the file can't be opened, output falls back to stderr. `-depth` defaults to 5, so you can leave it off.

Matches are exact by default: a value is reported only if it equals a match string. `-exact=false` (env `ARGMATCH_EXACT=false`) also reports values that contain one (see "What is matched" below).

With `-recvonly` (env `ARGMATCH_RECVONLY=1`), only method receivers are checked, not parameters. Functions without a receiver are then skipped, including any named with `-func` or found in `-stacklog`.

Matches are written as CSV, one row per match, with these columns:

| Column | Contents |
|---|---|
| `needle` | the match string |
| `haystack` | only with `-exact=false`: the whole value that matched, with non-printable characters, invalid UTF-8 and backslashes escaped as in a Go string literal (`\x00`, `\n`, `\\`), since it can be a binary buffer. With exact matching it would always equal `needle`, so it's left out. |
| `function` | the instrumented function |
| `expression` | the path from the parameter to the value, e.g. `o.P.Name` |
| `types` and every column after it | one per step of the path from the parameter down to the match, in order |

So with the defaults the columns are `needle,function,expression,types`, and with `-exact=false` they're `needle,haystack,function,expression,types`. Scripts reading the output should find columns by name in the header.

A step of the path is one of:
- a named type, e.g. `main.Outer`;
- an interface, written with its concrete type first, e.g. `main.ID5 (implements main.Iface)`.

In expressions, a map entry is named by its key when the key is a string, integer or bool: `m["config.file"]` for the value and `m<key "config.file">` for the key itself. Other keys (e.g. structs) are named by their position, `m[<key 0>]`. Entries are walked in memory order, which depends on a hash seed Go randomizes for each map, so positions, and which entries are within `-maxelems`, can differ between runs. Expressions containing quotes are quoted in the CSV, with the inner quotes doubled, e.g. `"h<key ""needle"">"`.

Each call of an instrumented function writes its own rows, so a function called often repeats the same rows. [dedup.py](dedup.py) removes the duplicates from one run's CSV: `./dedup.py matches.csv > deduped.csv` (or read from stdin). It also merges rows that differ only in a map position, writing it as `<key N>`, and groups the rows by needle, ordering the groups by how many distinct haystacks the needle matched (fewest first) and sorting each group by haystack. Without a `haystack` column (exact matching), that's simply sorting by needle.

Rows have as many step columns as their path has steps, so rows differ in length. The header names only the first step column (`types`). The header is written once, at the top of the output. Errors always go to stderr as plain text, never into the CSV.

Output for `F` in `testdata/example/main.go`:

```
needle,function,expression,types
needle,main.ID.F,id_recvr,main.ID
needle,main.ID.F,id_arg,main.ID2
needle,main.ID.F,"h<key ""needle"">",main.Hier
needle,main.ID.F,in.Name,main.Named,main.ID3
needle,main.ID.F,o.P.Name,main.Outer,main.Inner,main.ID4
needle,main.ID.F,i,main.ID5 (implements main.Iface)
done
```

`arr []string` correctly gives no output.

The variables captured by func values (closures and method values) aren't checked, so `G` in the example reports nothing. `-ignoreclosures=false` is refused: argmatch reads values using type information from `reflect`, which doesn't describe captured variables. (The delve version, tagged `delve-walker`, could check them; see [IMPLEMENTATIONS.md](IMPLEMENTATIONS.md).)

## How it works

The program is never stopped or attached to. All instrumentation happens at compile time:

- **Compile time ([main.go](main.go)):** in the package that contains the target function, a call is inserted at the top of the function. It passes the address of the receiver and each argument, boxed so the static type survives (an interface argument stays an interface). The edit adds no lines, so line numbers in panics and debug info stay correct. Packages are otherwise compiled as usual, with optimizations.
- **Settings:** only `-func`, `-stacklog` and `-recvonly` change which code is instrumented. All the other settings only change what the helper does when the program runs: the match strings, `-exact`, `-ignoreclosures`, `-ignorelibs`, `-ignoretypes`, `-depth`, `-maxelems`, `-maxfields`, `-maxcalls`, `-maxtime`, `-maxmem` and `-out`. So they aren't compiled into the instrumented packages. The wrapper gives them to the linker instead (`-X argmatch/rt.encoded=…`, in a response file), and the go command's cache key for compiling leaves them out. Changing them relinks the program and reuses every compiled package.
- **Overriding at run time:** the program can override the run-time settings with environment variables when it starts, with no rebuild, e.g. `ARGMATCH_DEPTH=20 ./program`. They're the same `ARGMATCH_*` variables the wrapper reads, and only those set to a non-empty value count. Setting `ARGMATCH_MATCH` or `ARGMATCH_MATCHFILE` replaces the match strings, and `ARGMATCH_MATCHFILE` is then read when the program starts, so it must exist where the program runs. The program says on stderr which variables it used (`argmatch: settings from the environment: …`), and which it couldn't parse. The helper's `settings` package ([settings/settings.go](settings/settings.go)) defines the settings, and is shared by the wrapper and the helper.
- **Run time ([rt/rt.go](rt/rt.go)):** a small helper package linked into the program walks each argument on the calling goroutine, without touching the program's data directly:
  - **Reading:** every value's bytes are read through `/proc/self/mem`, so a bad pointer or a value that's being written gives a read error or wrong bytes, not a crash. A struct takes one read, and its fields are decoded from the buffer at their offsets; pointers, string contents and slice contents take one more read each.
  - **Types:** `reflect` supplies only type layout (sizes, field offsets, element types), from the binary's read-only type descriptors. An interface's dynamic type is a type pointer read from memory, and it's used only if it points into the executable's read-only memory.
  - **Maps** are decoded from the runtime's map layout (Go 1.24 and later): the header, then either one 8-slot group or a directory of tables of groups, with full slots found from their control bytes. The layout is checked against the map's type descriptor first, so a different layout is skipped rather than misread.
  - **Searching as it reads:** argmatch checks each string as it's read and keeps only the current path, so memory stays small however much it walks. Checks don't share a lock, so a slow check only delays the call it's checking.

## Tested

`go test .` builds the example under argmatch and checks the matches for `F` three ways: with `-match needle` against the output above, with a match file, and with `-stacklog testdata/stacks.log` and no `-func`. It also checks that exact matching and ignoring closures are the defaults, that `-ignoreclosures=false` is refused, `-recvonly`, `-maxelems`, `-maxfields`, `-maxcalls`, `-maxtime`, `-maxmem`, `-ignorelibs`, `-ignoretypes`, map decoding (small and table-backed maps, indirect keys and values, basic and struct keys, nested maps, a map in an interface; [testdata/maps](testdata/maps/main.go)), overriding settings with environment variables at run time, and that changing only run-time settings relinks without recompiling. `go test ./settings` checks the settings encoding and environment parsing. It takes a few seconds (longer with a cold build cache) and is skipped by `go test -short`.

Also tested by hand:

- **Prometheus** at `-depth 10 -maxelems 64 -maxcalls 3` with the prombench stack log: ready after 1 s, about 32 MiB, and 1.4 s spent in 70 checks (the slowest, `web.(*Handler).Run`, 177 ms with 167k reads of about 5,000 maps). It didn't crash in 20 runs, where the earlier reflect walker crashed in about half.
- **Edge cases** (with the reflect walker this one replaced, on the `reflect-port` branch): unnamed and `_` parameters, a generic receiver, `any` holding a plain string, wrapped errors, integer map keys, byte arrays as map values, pointers to pointers, and two goroutines calling the function at the same time.
- **Byte slices:** `[]byte` and byte arrays are matched like strings. A plain `[]byte` argument prints nothing, but a named `type B []byte`, a `[]byte` field in a named struct, and a `[]byte` in an `any` are reported.

## Notes

- **Matching the program's build:** the helper depends only on the standard library, `settings` and `stacklog`. It's built with the outer `go build`'s `-tags`, `-race`, `-msan`, `-asan`, `-trimpath`, `-buildmode`, `-pgo` and `-installsuffix` flags, which keeps the standard library packages it shares with the program identical, as the linker requires.
- **What `-depth` counts:** nesting of structs, maps, arrays and slices, following the rules of delve's `MaxVariableRecurse`, so `-depth` means what it did in the delve version. Pointers don't count as a level, except a pointer to a pointer or to an interface, so `-depth 2` reaches `l.X.X.N` in `L1{L2{L3{N}}}` and `-depth 1` doesn't. Strings are always checked once reached.
- **Work per check, `-depth` and `-maxelems`:** a check walks up to `-maxelems` (env `ARGMATCH_MAXELEMS`, default 64) elements of each array, slice and map, so the work can grow roughly as maxelems^depth. Memory stays small, since nothing is kept but the current path and the rows found. Matches beyond the first `-maxelems` elements of a collection are missed. For maps that's the first entries in memory order, as in the delve version.
- **`-maxfields`** (env `ARGMATCH_MAXFIELDS`, default -1, meaning all) limits struct fields the same way: only the first `-maxfields` fields of each struct are walked, in declaration order. Fields after that, and anything reached through them, aren't searched, so a match in a struct's last fields is missed even if it's shallow.
- **`-ignorelibs` and `-ignoretypes`:** some struct types aren't walked beyond the value itself.
  - **Which types:** with `-ignorelibs` (env `ARGMATCH_IGNORELIBS`, default true), any type `stacklog.IgnoreFn` would ignore if it were a function name, i.e. standard library types (`log/slog.Logger`, `net/http.Request`, ...) and those in `stacklog.IGNORE_LIBS` (gRPC, `golang.org/x/net`, ...). `-ignoretypes` (env `ARGMATCH_IGNORETYPES`) adds a comma-separated list of types, each a full name or its end after a slash, e.g. `config.Config` for `github.com/prometheus/prometheus/config.Config`.
  - **Only structs:** a named map or slice type such as `net/http.Header` still has its elements walked.
  - **What's missed:** matches inside ignored types. That includes standard library wrappers around a program's own data, e.g. a `sync/atomic.Pointer[config.Config]` or a `net/url.URL`'s host.
- **`-maxtime` and `-maxmem`** (env `ARGMATCH_MAXTIME`, `ARGMATCH_MAXMEM`; default 0, meaning no limit) cap each check. They take a duration (`10s`) and a size (`1GB`, `512MiB`, or bytes). Once a check has taken `-maxtime`, or used `-maxmem`, every further read fails, so the walk stops. argmatch reports the matches found so far, and prints to stderr which cap was hit and which arguments, if any, weren't walked at all. Memory used is the larger of the Go heap's growth since the check started (live objects plus garbage not yet freed, for the whole program) and the bytes the check has read, checked every 16 reads. With the raw walker, checks are fast enough that the caps are rarely needed; they were added for the delve version.
- **`-maxcalls`** (env `ARGMATCH_MAXCALLS`, default 3): only the first `-maxcalls` calls of each function are checked. Later calls skip the check, so they cost almost nothing (a counter check, plus building the argument list). Values that only show up in later calls, e.g. after a config reload, are missed; `-maxcalls 0` checks every call.
- **`-stats`** (env `ARGMATCH_STATS`, a run-time setting) prints a line per check to stderr with its time, reads, bytes read, maps walked, and maps and type pointers skipped.
- **Function names** are as in stacktraces: `pkg/path.Func`, `pkg.T.M` or `pkg.(*T).M`. Functions written in assembly can't be instrumented.
- **Names in stack logs** are mapped to declarations by `stacklog.EnclosingFns`, which `parse` uses too. Closures and `go`/`defer` wrappers (`pkg.F.func1.2`, `pkg.F.gowrap1`) mean the function that contains them, and method values (`pkg.T.M-fm`) mean the method. A closure in an inlined function is named after both functions (`pkg.(*T).Run.helper.func5`), and both are checked (`pkg.helper` and `pkg.(*T).Run`). That's guessed from the name, so it can't recover an inlined function from another package, and a value-receiver method inlined into a function is misread. The comment on `EnclosingFns` describes an exact alternative using the frames' file:line.
- **What is matched:** string values and `[]byte`/byte array contents. By default a value matches only if it equals a match string exactly. With `-exact=false` (env `ARGMATCH_EXACT=false`) it also matches if it contains the full match string anywhere, so `-match needle` reports `"xneedlex"` too. The output shows both the match string (`needle`) and the whole value that contained it (`haystack`). A value that contains several match strings gives one row per string. Strings and byte slices are read up to 1 MiB, and arrays, slices and maps up to `-maxelems` elements.
- **First build is slow:** every package is rebuilt once, because the build cache is keyed on argmatch's settings. The cache key for compiling covers the instrumented functions (`-func`, `-stacklog`), `-recvonly`, the wrapper binary and the helper's sources. Changing any of those recompiles every package. Changing only run-time settings just relinks (see "How it works").
- **Limitations:**
  - **It can still crash the program, though it hasn't been seen.** An interface's type pointer, read from memory, is handed to `reflect` if it points into the executable's read-only memory. If the walker followed a stale pointer into freed and reused memory, those bytes may happen to pass that check without being a type descriptor. `reflect` then reads nonsense from them, which can crash the program with a fault or a runtime throw (e.g. "name offset out of range"). Everything else is read through `/proc/self/mem`, where a bad read is just an error.
  - **Torn reads give wrong or missed rows:** a value read while another goroutine writes it can be inconsistent. It's decoded like any other bytes, so the result may be a wrong row or a missed match, never a crash.
  - **Maps need Go 1.24's layout** or later. A map whose type descriptor doesn't have the expected layout is skipped and counted (`badMaps` in `-stats`).
  - **Closures** aren't followed (see above). Neither are channels.
  - **Re-entry:** calls to a standard library target function made by the helper itself while a check is running are skipped, to avoid infinite recursion.

## Prometheus

`../../prometheus/prombench-argmatch.Dockerfile` is `prombench.Dockerfile` with Prometheus built under argmatch. The target is set with build args (defaults shown):

```sh
cd ~/projects/config_tracing
docker build -f prometheus/prombench-argmatch.Dockerfile --build-arg VERSION=v3.2.1 \
    --build-arg ARGMATCH_FUNC=main.reloadConfig \
    --build-arg ARGMATCH_MATCH=/etc/prometheus/prometheus.yml \
    -t prometheus-argmatch:v3.2.1 .
docker run --rm prometheus-argmatch:v3.2.1 2>&1 | grep argmatch
```

To write matches to a file instead of stderr, add `--build-arg ARGMATCH_OUT=/argmatch-out/matches.txt` to the build and mount a host directory there:

```sh
docker run --rm -v /tmp/argmatch-out:/argmatch-out prometheus-argmatch:v3.2.1
```

The base image runs Prometheus as `nobody`, so the host directory must be writable by that user (e.g. `chmod 777 /tmp/argmatch-out`).

The output path is linked in, so every container started from the image writes to the same path, unless you override it with `docker run -e ARGMATCH_OUT=…`. If several of them run at once (e.g. prombench's `prometheus-meta` and `prometheus-test` pods), mount a different host directory into each, or they'll overwrite each other's output.

The Dockerfile copies `stacklog` too, at the same path relative to argmatch as here. To use a stack log, put it in the Prometheus source directory and pass `--build-arg ARGMATCH_STACKLOG=/src/<file>`. Like a match file (below), it's read during `docker build`, not when the container runs. Add `--build-arg ARGMATCH_FUNC=` to check only the log's functions, and `--build-arg ARGMATCH_RECVONLY=1` to check only their receivers. Matches are exact unless you pass `--build-arg ARGMATCH_EXACT=false`. `ARGMATCH_DEPTH`, `ARGMATCH_MAXELEMS`, `ARGMATCH_MAXFIELDS`, `ARGMATCH_MAXTIME`, `ARGMATCH_MAXMEM`, `ARGMATCH_MAXCALLS`, `ARGMATCH_IGNORELIBS` and `ARGMATCH_IGNORETYPES` set the flags of the same names. For example, `--build-arg ARGMATCH_IGNORETYPES=config.Config` stops `config.Config`'s fields from being walked.

Rebuilds are fast unless they change which code is instrumented. The Dockerfile keeps Go's build cache between builds (BuildKit cache mounts), and builds the web UI in its own stage, which reruns only when `web/ui` or the Makefiles change. Here, with a stack log and `-recvonly`:
- the first build took 65–95 s;
- a rebuild with only `ARGMATCH_DEPTH` changed took 21 s, against about 2.5 minutes before these changes.

Changing `ARGMATCH_FUNC`, `ARGMATCH_STACKLOG` (or the log file itself) or `ARGMATCH_RECVONLY` still recompiles every package.

The run-time settings can also be changed without rebuilding at all, with `docker run -e`. For example, `docker run -e ARGMATCH_DEPTH=20 -e ARGMATCH_MAXMEM=2GB …`. To use another match file this way, mount it into the container and set `-e ARGMATCH_MATCHFILE=` to its path there.

To use a match file, put it in the Prometheus source directory, which is copied to `/src` in the build stage, and pass its path there, e.g. `/src/argmatch-matches.txt`. The file is read during `docker build` and its strings are linked into Prometheus, so the image you run doesn't contain the file. After editing it, rebuild (which only relinks), or mount it and override with `docker run -e ARGMATCH_MATCHFILE=…`. `ARGMATCH_MATCH` defaults to the config path, so set it to empty to use only the file:

```sh
cp matches.txt ~/projects/config_tracing/prometheus/argmatch-matches.txt
docker build -f prometheus/prombench-argmatch.Dockerfile --build-arg VERSION=v3.2.1 \
    --build-arg ARGMATCH_MATCH= \
    --build-arg ARGMATCH_MATCHFILE=/src/argmatch-matches.txt \
    -t prometheus-argmatch:v3.2.1 .
```
