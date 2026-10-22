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

To check against several strings, put them in a file, one per line, and pass it with `-matchfile` (env `ARGMATCH_MATCHFILE`). Every value is checked against every string, and each output line names the string that matched. `-matchfile` can be combined with `-match`. The file's path must be absolute, because the go command runs the wrapper in each package's directory. Lines are used exactly as written, including leading and trailing spaces; blank lines and lines starting with `#` are ignored, so a match string can't start with `#`. The strings are read at build time, so rebuild after editing the file.

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

In expressions, a map entry is named by its key when the key is a string, integer or bool: `m["config.file"]` for the value and `m<key "config.file">` for the key itself. Other keys (e.g. structs) are named by their position, `m[<key 0>]`. Go randomizes the order maps are iterated in, so positions can differ between calls and runs. Expressions containing quotes are quoted in the CSV, with the inner quotes doubled, e.g. `"h<key ""needle"">"`.

Each call of an instrumented function writes its own rows, so a function called often repeats the same rows. [dedup.py](dedup.py) removes the duplicates from one run's CSV: `./dedup.py matches.csv > deduped.csv` (or read from stdin). It also merges rows that differ only in a map position, writing it as `<key N>`, and groups the rows by needle, ordering the groups by how many distinct haystacks the needle matched (fewest first) and sorting each group by haystack. Without a `haystack` column (exact matching), that's simply sorting by needle.

Rows have as many step columns as their path has steps, so rows differ in length. The header names only the first step column (`types`). The header is written once, at the top of the output. Errors, such as an argument that can't be loaded, always go to stderr as plain text, never into the CSV.

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

The variables captured by func values (closures, method values, `http.HandlerFunc`s) aren't checked, so `G` in the example, whose matches are all in captured variables, reports nothing. `reflect` can't see them: a func value is a pointer to its code and its captured variables, and only the binary's DWARF debug info records their types. `-ignoreclosures=false` is refused for that reason. In Prometheus that misses a lot, since config reloaders, HTTP handlers and run-group actors are closures that capture `*web.Handler`; the delve-based version on `main` follows closures.

## How it works

The program is never stopped or attached to. All instrumentation happens at compile time:

- **Compile time ([main.go](main.go)):** in the package that contains the target function, a call is inserted at the top of the function. It passes the address of the receiver and each argument, boxed so the static type survives (an interface argument stays an interface). The edit adds no lines, so line numbers in panics and debug info stay correct. Packages are otherwise compiled as usual, with optimizations.
- **Run time ([rt/rt.go](rt/rt.go)):** a small helper package linked into the program walks each argument with `reflect`, on the calling goroutine. It follows pointers, interfaces, structs (including unexported fields), maps, arrays and slices down to `-depth`, checks every string and byte slice it reaches, and collects the named types and interfaces on the way. It walks values one at a time, so memory doesn't grow with the size of what it walks, and checks don't share a lock, so a slow check only delays the call it's checking.

## Tested

`go test .` builds the example under argmatch and checks the matches for `F` three ways: with `-match needle` against the output above, with a match file, and with `-stacklog testdata/stacks.log` and no `-func`. It also checks that exact matching and ignoring closures are the defaults, that `-ignoreclosures=false` is refused, `-recvonly`, `-maxelems`, `-maxcalls`, and the columns with `-exact=false`. It takes a few seconds (longer with a cold build cache) and is skipped by `go test -short`.

Also tested by hand:

- **Standard library:** `-func 'net.(*sysDialer).dialSerial' -match 127.0.0.1` reports `127.0.0.1:<port>` at `sd.address` with type `net.sysDialer`.
- **Edge cases:** unnamed and `_` parameters, a generic receiver (`main.G[main.Name]`), `any` holding a plain string, wrapped errors, unexported map keys, integer map keys, byte arrays stored as map values, pointers to pointers, and two goroutines calling the function at the same time all work. The depth limit cuts off deeper struct nesting as expected. At depths 1, 2 and 5 the output matched the delve-based version on `main`, except that delve counted an interface argument (e.g. an `error`) one level deeper than an interface field.
- **Byte slices:** `[]byte` and byte arrays are matched like strings. A plain `[]byte` argument prints nothing, but a named `type B []byte`, a `[]byte` field in a named struct, and a `[]byte` in an `any` are reported.

## Notes

- **Matching the program's build:** the helper depends only on the standard library. It's built with the outer `go build`'s `-tags`, `-race`, `-msan`, `-asan`, `-trimpath`, `-buildmode`, `-pgo` and `-installsuffix` flags, which keeps the standard library packages it shares with the program identical, as the linker requires.
- **What `-depth` counts:** nesting of structs, maps, arrays and slices, following delve's `MaxVariableRecurse` rules. Pointers don't count as a level, except a pointer to a pointer or to an interface, so `-depth 2` reaches `l.X.X.N` in `L1{L2{L3{N}}}` and `-depth 1` doesn't. Strings are always checked once reached.
- **Time, `-depth` and `-maxelems`:** a check walks up to `-maxelems` (env `ARGMATCH_MAXELEMS`, default 64) elements of each array, slice and map, so the work per check can grow roughly as maxelems^depth. Memory stays small, but a check on an argument that reaches much of the program (e.g. Prometheus's `*web.Handler`) at a large depth can take a long time. Matches beyond the first `-maxelems` elements of a collection are missed.
- **`-maxcalls`** (env `ARGMATCH_MAXCALLS`, default 3): only the first `-maxcalls` calls of each function are checked. Later calls skip the check, so they cost almost nothing (a counter check, plus building the argument list). Values that only show up in later calls, e.g. after a config reload, are missed; `-maxcalls 0` checks every call.
- **Function names** are as in stacktraces: `pkg/path.Func`, `pkg.T.M` or `pkg.(*T).M`. Functions written in assembly can't be instrumented.
- **Names in stack logs** are mapped to declarations by `stacklog.EnclosingFns`, which `parse` uses too. Closures and `go`/`defer` wrappers (`pkg.F.func1.2`, `pkg.F.gowrap1`) mean the function that contains them, and method values (`pkg.T.M-fm`) mean the method. A closure in an inlined function is named after both functions (`pkg.(*T).Run.helper.func5`), and both are checked (`pkg.helper` and `pkg.(*T).Run`). That's guessed from the name, so it can't recover an inlined function from another package, and a value-receiver method inlined into a function is misread. The comment on `EnclosingFns` describes an exact alternative using the frames' file:line.
- **What is matched:** string values and `[]byte`/byte array contents. By default a value matches only if it equals a match string exactly. With `-exact=false` (env `ARGMATCH_EXACT=false`) it also matches if it contains the full match string anywhere, so `-match needle` reports `"xneedlex"` too. The output shows both the match string (`needle`) and the whole value that contained it (`haystack`). A value that contains several match strings gives one row per string. Strings and byte slices are checked whole, and arrays, slices and maps up to `-maxelems` elements.
- **First build is slow:** every package is rebuilt once, because the build cache is keyed on the settings, the wrapper binary and the helper's sources. Later builds with the same settings are fast.
- **Limitations:**
  - Closures aren't followed (see above).
  - Calls to a standard library target function made by the helper itself while a check is running are skipped, to avoid infinite recursion.

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

The output path is compiled in, so every container started from the image writes to the same path. If several of them run at once (e.g. prombench's `prometheus-meta` and `prometheus-test` pods), mount a different host directory into each, or they'll overwrite each other's output.

The Dockerfile copies `stacklog` too, at the same path relative to argmatch as here. To use a stack log, put it in the Prometheus source directory and pass `--build-arg ARGMATCH_STACKLOG=/src/<file>`. Like a match file (below), it's read during `docker build`, not when the container runs. Add `--build-arg ARGMATCH_FUNC=` to check only the log's functions, and `--build-arg ARGMATCH_RECVONLY=1` to check only their receivers. Matches are exact unless you pass `--build-arg ARGMATCH_EXACT=false`. Closures are always ignored by this version, so leave `ARGMATCH_IGNORECLOSURES` at its default; `false` makes the build fail.

To use a match file, put it in the Prometheus source directory, which is copied to `/src` in the build stage, and pass its path there, e.g. `/src/argmatch-matches.txt`. The file is read while Prometheus is compiled, during `docker build`, so the image you run doesn't contain it; rebuild after editing it. `ARGMATCH_MATCH` defaults to the config path, so set it to empty to use only the file:

```sh
cp matches.txt ~/projects/config_tracing/prometheus/argmatch-matches.txt
docker build -f prometheus/prombench-argmatch.Dockerfile --build-arg VERSION=v3.2.1 \
    --build-arg ARGMATCH_MATCH= \
    --build-arg ARGMATCH_MATCHFILE=/src/argmatch-matches.txt \
    -t prometheus-argmatch:v3.2.1 .
```
