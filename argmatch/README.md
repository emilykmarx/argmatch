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
(cd testdata/example && go build -toolexec "$A -func main.ID.F -matchfile /tmp/matches.txt" -o /tmp/example .) && /tmp/example
```

To check every function that appears in a log of connection stacktraces, pass the log with `-stacklog` (env `ARGMATCH_STACKLOG`), instead of or as well as `-func`. Like `-matchfile`, the path must be absolute and the log is read at build time. The log is parsed with `stacklog`, the parser shared with conftamer's `parse` package, from `../../golang.org-x-tools/gopls/conftamer/stacklog`. Functions are chosen the same way `parse` chooses them:

- A closure (`pkg.F.func1`) means the function that defines it (`pkg.F`). See the notes below for the details.
- Functions that `stacklog.IgnoreFn` ignores are skipped. That's the standard library, gRPC, `golang.org/x/net`, `klauspost/compress`, `main.main` and some k8s client packages. A function given with `-func` is always checked.
- Functions without parameters or a receiver are skipped, since there's nothing to check.

```sh
(cd testdata/example && go build -toolexec "$A -stacklog $PWD/../stacks.log -match needle" -o /tmp/example .) && /tmp/example
```

Parsing a large log takes about a second, and the go command runs the wrapper once per package. So the list of functions is cached in `~/.cache/argmatch/`, keyed on the log's path, size and modification time.

The wrapper path must be absolute, which is why it's set from `$PWD`. Matches are printed while the program runs, not during the build. By default they go to stderr. With `-out file` (env `ARGMATCH_OUT`) the program appends them to that file instead, so they can't be interleaved with the program's own stderr output. A relative path is resolved against the program's working directory when it runs, and if the file can't be opened, output falls back to stderr. `-depth` defaults to 5, so you can leave it off.

Matches are written as CSV, one row per match, with these columns:

| Column | Contents |
|---|---|
| `needle` | the match string |
| `haystack` | the whole value that contained it, with non-printable characters, invalid UTF-8 and backslashes escaped as in a Go string literal (`\x00`, `\n`, `\\`), since it can be a binary buffer |
| `function` | the instrumented function |
| `expression` | the path from the parameter to the value, e.g. `o.P.Name` |
| `closure` | `true` if a closure is on the path, else `false` |
| `types` and every column after it | one per step of the path from the parameter down to the match, in order |

A step of the path is one of:
- a named type, e.g. `main.Outer`;
- an interface, written with its concrete type first, e.g. `main.ID5 (implements main.Iface)`;
- a closure or method value, written `closure <function>`, e.g. `closure main.(*Handler).Apply-fm`.

In expressions, a map entry is named by its key when the key is a string, integer or bool: `m["config.file"]` for the value and `m<key "config.file">` for the key itself. Other keys (e.g. structs) are named by their position, `m[<key 0>]`. Positions come from the map's memory layout, which depends on a hash seed Go randomizes for each map, so they can differ between runs. Expressions containing quotes are quoted in the CSV, with the inner quotes doubled, e.g. `"h<key ""needle"">"`.

Each call of an instrumented function writes its own rows, so a function called often repeats the same rows. [dedup.py](dedup.py) removes the duplicates from one run's CSV: `./dedup.py matches.csv > deduped.csv` (or read from stdin). It also merges rows that differ only in a map position, writing it as `<key N>`, and sorts the rows by needle, then haystack.

Rows have as many step columns as their path has steps, so rows differ in length. The header names only the first step column (`types`). The header is written when the output starts: once at the top of stderr, or when the `-out` file is new or empty, so appending to earlier output doesn't repeat it. Errors, such as an argument that can't be loaded, always go to stderr as plain text, never into the CSV.

Output for `F` in `testdata/example/main.go`:

```
needle,haystack,function,expression,closure,types
needle,needle,main.ID.F,id_recvr,false,main.ID
needle,needle,main.ID.F,id_arg,false,main.ID2
needle,needle,main.ID.F,"h<key ""needle"">",false,main.Hier
needle,needle,main.ID.F,in.Name,false,main.Named,main.ID3
needle,needle,main.ID.F,o.P.Name,false,main.Outer,main.Inner,main.ID4
needle,needle,main.ID.F,i,false,main.ID5 (implements main.Iface)
done
```

`arr []string` correctly gives no output.

A match reached through a func value's captured variables has `closure` set to `true`, and a `closure <function>` step at its place in the path. The expression shows the captured variable as `[captured <variable>]`, with `receiver` for a method value's receiver. For `G` in the example:

```
needle,haystack,function,expression,closure,types
needle,needle,main.G,r.Fn[captured receiver].Path,true,main.Reloader,closure main.(*Handler).Apply-fm,main.Handler,main.ID
needle,needle,main.G,named[captured named].Name,true,closure main.main.func1,main.Named,main.ID3
needle,needle,main.G,basic[captured needle],true,closure main.main.func2
```

A closure counts as a non-basic type on the path, so a plain string it captured is reported, as in the last row.

## How it works

The program is never stopped or attached to. All instrumentation happens at compile time:

- **Compile time ([main.go](main.go)):** every package is compiled with `-N -l`. In the package that contains the target function, a call is inserted at the top of the function. It passes the address of the receiver and each argument, boxed so the static type survives (an interface argument stays an interface). The edit adds no lines, so line numbers in panics and debug info stay correct.
- **Run time ([rt/rt.go](rt/rt.go)):** a small helper package linked into the program loads the binary's own debug info with delve. It reads the program's own memory through `/proc/self/mem`, so a bad pointer gives an error, not a crash. It then calls `EvalExpression("*(*interface {})(0x…)")` with `FollowPointers` and `MaxVariableRecurse = -depth`. Delve does all the type resolution, pointer following and loading; argmatch only walks the loaded tree and collects named types and interfaces.

## Tested

`go test .` builds the example under argmatch and checks the matches for `F` three ways: with `-match needle` against the output above, with a match file, and with `-stacklog testdata/stacks.log` and no `-func`. It also checks the closure output for `G` above. It takes a few seconds (longer with a cold build cache) and is skipped by `go test -short`.

Also tested by hand:

- **Standard library:** `-func 'net.(*sysDialer).dialSerial' -match 127.0.0.1` reports `127.0.0.1:<port>` at `sd.address` with type `net.sysDialer`. That's a package delve itself depends on.
- **Stack log from prombench:** Prometheus built with `-stacklog` set to a prometheus-meta pod log (92k connections, 252 distinct functions) builds in 25 s. Running it with `-match prometheus.yml` reports matches in `main.reloadConfig`, `web.(*Handler).Run`, `route.(*Router).handle` and the promhttp handlers. Nearly all of them (377 of 384 distinct rows) are through closures, e.g. expression `rls[2].reloader[captured receiver].flagsMap["config.file"]` with steps `main.reloader`, `closure github.com/prometheus/prometheus/web.(*Handler).ApplyConfig-fm`, `github.com/prometheus/prometheus/web.Handler`.
- **Edge cases:** unnamed and `_` parameters, a generic receiver (`main.G[main.Name]`), `any` holding a plain string, and two goroutines calling the function at the same time all work. The depth limit cuts off deeper struct nesting as expected.
- **Byte slices:** `[]byte` and byte arrays are matched like strings. A plain `[]byte` argument prints nothing, but a named `type B []byte`, a `[]byte` field in a named struct, and a `[]byte` in an `any` are reported.

## Notes

- **Delve version:** it uses upstream delve v1.26.3, not the local fork in `../../delve`, because the fork can't read Go 1.26 maps. `EvalExpression` is the same in both. v1.26.3 is used instead of v1.27.2 because its dependencies are old enough for Prometheus's (e.g. `golang.org/x/sys` v0.29.0).
- **Matching the program's build:** the helper and delve are built against the program's versions of any modules both use, and with the outer `go build`'s `-tags`, `-race`, `-msan`, `-asan`, `-trimpath`, `-buildmode`, `-pgo` and `-installsuffix` flags. That keeps packages shared with the program identical, which the linker requires.
- **What `-depth` counts:** it's delve's `MaxVariableRecurse`. Pointers don't count as a level, so `-depth 2` reaches `l.X.X.N` in `L1{L2{L3{N}}}` and `-depth 1` doesn't.
- **Function names** use delve's naming: `pkg/path.Func`, `pkg.T.M` or `pkg.(*T).M`, which is also how they appear in stacktraces. Functions written in assembly can't be instrumented.
- **Names in stack logs** are mapped to declarations by `stacklog.EnclosingFns`, which `parse` uses too. Closures and `go`/`defer` wrappers (`pkg.F.func1.2`, `pkg.F.gowrap1`) mean the function that contains them, and method values (`pkg.T.M-fm`) mean the method. A closure in an inlined function is named after both functions (`pkg.(*T).Run.helper.func5`), and both are checked (`pkg.helper` and `pkg.(*T).Run`). That's guessed from the name, so it can't recover an inlined function from another package, and a value-receiver method inlined into a function is misread. The comment on `EnclosingFns` describes an exact alternative using the frames' file:line.
- **What is matched:** string values and `[]byte`/byte array contents. A value matches if it contains the *full* match string anywhere, so `-match needle` reports `"xneedlex"`. The output shows both the match string (`needle`) and the whole value that contained it (`haystack`). A value that contains several match strings gives one row per string. Strings are loaded up to 64 KiB and arrays up to 4096 elements.
- **First build is slow:** it recompiles the standard library unoptimized, about 6 s here. The cache is keyed on the settings, the wrapper binary and the helper's sources, so later builds are fast.
- **Limitations:**
  - If the program's version of a shared module is too old for delve to compile against, the build fails.
  - Calls to the target function made by delve's own code while a check is running are skipped, to avoid infinite recursion.

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

The Dockerfile copies `stacklog` too, at the same path relative to argmatch as here. To use a stack log, put it in the Prometheus source directory and pass `--build-arg ARGMATCH_STACKLOG=/src/<file>`. Like a match file (below), it's read during `docker build`, not when the container runs. Add `--build-arg ARGMATCH_FUNC=` to check only the log's functions.

To use a match file, put it in the Prometheus source directory, which is copied to `/src` in the build stage, and pass its path there, e.g. `/src/argmatch-matches.txt`. The file is read while Prometheus is compiled, during `docker build`, so the image you run doesn't contain it; rebuild after editing it. `ARGMATCH_MATCH` defaults to the config path, so set it to empty to use only the file:

```sh
cp matches.txt ~/projects/config_tracing/prometheus/argmatch-matches.txt
docker build -f prometheus/prombench-argmatch.Dockerfile --build-arg VERSION=v3.2.1 \
    --build-arg ARGMATCH_MATCH= \
    --build-arg ARGMATCH_MATCHFILE=/src/argmatch-matches.txt \
    -t prometheus-argmatch:v3.2.1 .
```
