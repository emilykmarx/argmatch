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

To check against several strings, put them in a file, one per line, and pass it with `-matchfile` (env `ARGMATCH_MATCHFILE`). Every value is checked against every string, and each output line names the string that matched. `-matchfile` can be combined with `-match`. The file's path must be absolute, because the go command runs the wrapper in each package's directory. Lines are used exactly as written, including leading and trailing spaces; blank lines are ignored. The strings are read at build time, so rebuild after editing the file.

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

Output for `F` in `testdata/example/main.go`:

```
argmatch: value "needle" contains "needle" in main.ID.F: main.ID (id_recvr)
argmatch: value "needle" contains "needle" in main.ID.F: main.ID2 (id_arg)
argmatch: value "needle" contains "needle" in main.ID.F: main.Hier (h<key 0>)
argmatch: value "needle" contains "needle" in main.ID.F: main.Named -> main.ID3 (in.Name)
argmatch: value "needle" contains "needle" in main.ID.F: main.Outer -> main.Inner -> main.ID4 (o.P.Name)
argmatch: value "needle" contains "needle" in main.ID.F: main.Iface (concrete main.ID5) (i)
done
```

`arr []string` correctly gives no output. Each line shows the match string, the whole value that contained it, the function, the named types from the argument down to the match, and the path to the value in brackets.

## How it works

The program is never stopped or attached to. All instrumentation happens at compile time:

- **Compile time ([main.go](main.go)):** in the package that contains the target function, a call is inserted at the top of the function. It passes the address of the receiver and each argument, boxed so the static type survives (an interface argument stays an interface). The edit adds no lines, so line numbers in panics and debug info stay correct. Packages are otherwise compiled normally, with optimizations.
- **Run time ([rt/rt.go](rt/rt.go)):** a small helper package linked into the program walks each argument with `reflect`. It follows pointers, interfaces, structs (including unexported fields), maps, arrays and slices down to `-depth`, checks every string and byte slice it reaches, and collects the named types and interfaces on the way. A check takes about 1.3 µs for `F` in the example when nothing matches.

## Tested

`go test .` builds the example under argmatch and checks the matches for `F` three ways: with `-match needle` against the output above, with a match file, and with `-stacklog testdata/stacks.log` and no `-func`. It takes a few seconds (longer with a cold build cache) and is skipped by `go test -short`.

Also tested by hand:

- **Standard library:** `-func 'net.(*sysDialer).dialSerial' -match 127.0.0.1` reports `net.sysDialer (sd.address)`.
- **Stack log from prombench:** Prometheus built with `-stacklog` set to a prometheus-meta pod log (92k connections, 252 distinct functions) builds in 30 s. Run with `-match prometheus.yml` and its config file at a path containing that string, it reports the `--config.file` flag value in `web.(*Handler).testReady`. Most paths to that value, including all of them from 8 other functions such as `main.reloadConfig`, go through closures and are missed (see the closure limitation below). [closure-gap/](closure-gap/) compares this with an earlier version that followed closures.
- **Edge cases:** unnamed and `_` parameters, a generic receiver (`main.G[main.Name]`), `any` holding a plain string, wrapped errors, unexported map keys, byte arrays stored as map values, pointers to pointers, and two goroutines calling the function at the same time all work. The depth limit cuts off deeper struct nesting as expected.
- **Byte slices:** `[]byte` and byte arrays are matched like strings. A plain `[]byte` argument prints nothing, but a named `type B []byte`, a `[]byte` field in a named struct, and a `[]byte` in an `any` are reported.

## Notes

- **Matching the program's build:** the helper depends only on the standard library. It's built with the outer `go build`'s `-tags`, `-race`, `-msan`, `-asan`, `-trimpath`, `-buildmode`, `-pgo` and `-installsuffix` flags, which keeps the standard library packages it shares with the program identical, as the linker requires.
- **What `-depth` counts:** nesting of structs, maps, arrays and slices. Pointers don't count as a level, except a pointer to a pointer or to an interface, so `-depth 2` reaches `l.X.X.N` in `L1{L2{L3{N}}}` and `-depth 1` doesn't. Strings are always checked once reached.
- **Function names** are as in stacktraces: `pkg/path.Func`, `pkg.T.M` or `pkg.(*T).M`. Functions written in assembly can't be instrumented.
- **Names in stack logs** are mapped to declarations by `stacklog.EnclosingFns`, which `parse` uses too. Closures and `go`/`defer` wrappers (`pkg.F.func1.2`, `pkg.F.gowrap1`) mean the function that contains them, and method values (`pkg.T.M-fm`) mean the method. A closure in an inlined function is named after both functions (`pkg.(*T).Run.helper.func5`), and both are checked (`pkg.helper` and `pkg.(*T).Run`). That's guessed from the name, so it can't recover an inlined function from another package, and a value-receiver method inlined into a function is misread. The comment on `EnclosingFns` describes an exact alternative using the frames' file:line.
- **What is matched:** string values and `[]byte`/byte array contents. A value matches if it contains the *full* match string anywhere, so `-match needle` reports `"xneedlex"`. The output shows both the match string and the whole value that contained it. A value that contains several match strings gives one line per string. Strings and byte slices are checked whole; only the first 4096 elements of other arrays and slices, and entries of maps, are walked.
- **Map keys** in paths (`h<key 0>`) are numbered in `reflect`'s iteration order, which is random, so the numbers can differ between runs.
- **First build is slow:** every package is rebuilt once, because the build cache is keyed on the settings, the wrapper binary and the helper's sources. Later builds with the same settings are fast.
- **Limitations:**
  - **Closures aren't followed.** A func value (a closure, method value or `http.HandlerFunc`) is a pointer to its code plus the variables it captured, and only the binary's DWARF debug info records the captured variables' types, so `reflect` can't see them. Anything reachable only through a closure's captured variables is missed. In Prometheus that's a lot: config reloaders, HTTP handlers and run-group actors are closures that capture `*web.Handler`. See [closure-gap/](closure-gap/) and the commit that introduced the `reflect` version for the measurements and ways to close the gap.
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

The Dockerfile copies `stacklog` too, at the same path relative to argmatch as here. To use a stack log, put it in the Prometheus source directory and pass `--build-arg ARGMATCH_STACKLOG=/src/<file>`, the same way as a match file (below). Add `--build-arg ARGMATCH_FUNC=` to check only the log's functions.

To use a match file, put it in the Prometheus source directory, which is copied to `/src` in the build stage, and pass its path in the container. `ARGMATCH_MATCH` defaults to the config path, so set it to empty to use only the file:

```sh
cp matches.txt ~/projects/config_tracing/prometheus/argmatch-matches.txt
docker build -f prometheus/prombench-argmatch.Dockerfile --build-arg VERSION=v3.2.1 \
    --build-arg ARGMATCH_MATCH= \
    --build-arg ARGMATCH_MATCHFILE=/src/argmatch-matches.txt \
    -t prometheus-argmatch:v3.2.1 .
```
