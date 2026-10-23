# We've tried 3 ways of implementing argmatch: delve, reflect, go-spew

argmatch doesn't stop or attach to the program it checks. It only changes the program's code at compile time. So every version injects a call at the top of each target function, and that call reads the function's receiver and arguments from inside the running program, while other goroutines keep running and changing them. The versions differ in how that call reads the values:

| | delve (`main`) | reflect (`reflect-port`) | go-spew (experiment only) |
|---|---|---|---|
| How it reads | delve's `EvalExpression`, reading through `/proc/self/mem` | `reflect`, plus `unsafe` for unexported fields | `spew.Sdump`, which also uses `reflect` plus `unsafe` |
| Can crash the program | no | **yes**, 5 of 10 Prometheus runs | **yes**, 4 of 10 Prometheus runs |
| Program is ready after | 20 s to over a minute (see below) | 1 s | (not measured) |
| Memory | 0.5–5 GB, growing to the caps | 26–34 MB | similar to reflect |
| Other goroutines' calls | wait (one lock shared by all checks) | not delayed | not delayed |
| Program compiled with | `-N -l` (no optimizations) | normal optimizations | normal optimizations |
| Closures' captured variables | followed (`-ignoreclosures=false`) | not followed | not followed |
| Map entries checked | first `-maxelems` in memory order | `-maxelems` in random iteration order | all of them |

## delve (`main`)

Delve loads each argument's whole tree from the program's memory, using the binary's DWARF debug info, then argmatch searches the loaded tree.

- **Safe:** delve reads through `/proc/self/mem`, so a bad pointer or a value that's being written gives a read error or a wrong value, never a crash. It never iterates a map with the runtime's iterator, so it can't hit the concurrent-map throw that crashes the other two versions.
- **Slow to start:** the first check reads the DWARF info, which takes about 20 s for Prometheus. All checks share one lock, so while one runs, every other instrumented call waits.
- **Memory:** delve loads the whole tree, down to `-depth`, before anything is searched, and loads it depth-first. In Prometheus:
  - at `-depth 10 -maxelems 4096`, it grew by about 0.5 GB/s until it was killed;
  - at `-maxelems 64`, it stayed at 0.5–3 GB;
  - at `-depth 20`, the first check (`web.(*Handler).testReady`) was still loading after 90 s, at 27 GB.
- **What makes it usable:** `-maxelems`, `-maxcalls`, `-maxtime` and `-maxmem`. With depth 20, `-maxelems 4`, `-maxcalls 1`, `-maxmem 1GB` and `-maxtime 10s`, every big check stopped at a cap after 1–10 s, Prometheus answered `/-/ready` after about 60 s, and memory peaked at about 4–5 GB.
- **What doesn't help much:**
  - `-ignorelibs` (skip standard library structs) made only checks on mostly standard-library values fast, such as the HTTP `Write` methods;
  - `-ignoretypes config.Config` didn't speed up the big checks either;
  - `-ignoreclosures` only filters the output, because delve loads closures' captured variables anyway.
- **Unoptimized program:** delve needs `-N -l` so that values are readable, so the whole program runs slower.
- **Closures:** the only version that can follow closures' captured variables, because DWARF describes them.

## reflect (`reflect-port`)

argmatch walks each value with `reflect` on the calling goroutine, searching as it goes. It uses `unsafe` to read unexported fields.

- **Fast:** in Prometheus at `-depth 10 -maxelems 64 -maxcalls 3`, `/-/ready` answered after 1 s instead of 15–20 s, and memory stayed at 26–34 MB instead of 0.5–2.2 GB. There's no shared lock, no debug-info loading, and the program is compiled with optimizations.
- **Can crash the program:** it reads live values with ordinary memory loads, without taking the program's locks. The crash seen in practice is `fatal error: concurrent map iteration and map write`. The runtime's map iterator detects a concurrent write and calls `throw`, which can't be recovered. It crashed 2 of 10 runs in the first test and 5 of 10 in a later one, always while checking `web.(*Handler).Run`, within seconds of starting. The other possible crashes haven't been seen:
  - a fault from following a stale pointer into unmapped memory;
  - a nil pointer, which panics; that's recoverable, but `Check` doesn't recover;
  - a runtime or garbage collector throw from a garbage pointer;
  - running out of memory after reading a garbage string or slice length.

  Torn reads can also just give wrong or missed rows.
- **No closures:** `reflect` can't see captured variables, so `-ignoreclosures=false` is refused. Following them would mean reading a little DWARF info without delve. That isn't started.
- **Output differences from delve:** mostly which map entries are walked. The runtime's iteration order is random, so different calls to the same function can see different entries. In one run each, 2,584 distinct rows were common to both versions, 2,742 were only in reflect and 1,562 only in delve.

## go-spew (experiment only)

[go-spew](https://github.com/davecgh/go-spew) was tried as a possibly safer replacement for the reflect walker, since it's widely used. For the test, `Check` called `spew.Sdump` on each argument, with methods disabled and `MaxDepth` set to `-depth`, and searched the dump for the match strings.

- **Not safer:** it reads values the same way as the reflect version (`reflect`, plus `unsafe` for unexported fields), so it has the same crashes. go-spew is widely used on values nothing else is changing, such as test fixtures and debug dumps, not on a running program's live state. With only `web.(*Handler).Run` checked, Prometheus crashed in 4 of 10 runs. Both crashes come from spew's `(*dumpState).dump`:
  - `concurrent map iteration and map write`, in `reflect.Value.MapKeys`;
  - `concurrent map read and map write`, in `reflect.Value.MapIndex`.
- **A misleading result:** with the whole stack log checked, it didn't crash in 10 runs. That's only because spew's earlier checks are slower, so it reached `Handler.Run` after the map had stopped changing.
- **Other drawbacks:**
  - it produces text, which argmatch would have to parse back into expressions and type paths;
  - by default it calls `String()` and `Error()` methods on live values, which can lock, panic or have side effects (`DisableMethods` turns that off);
  - it has no per-collection element limit, and `MapKeys` copies every key of every map;
  - its last release was in 2018.
- **Build detail:** `rt`'s go-spew version had to match the one Prometheus already links (`v1.1.2-0.20180830191138-d8f796af33cc`). Otherwise the link fails with a fingerprint mismatch.

## Ways to make the reflect version safe

These fixes were considered, from least to most work:

1. **Skip maps,** and add `debug.SetPanicOnFault(true)` plus `recover` in `Check`. Removes the map throw, faults and nil pointers, but loses everything inside maps.
2. **Iterate maps through the runtime's map internals** with `unsafe`, without the runtime's iterator. Avoids the map throw only, and depends on Go's map layout, which changes between versions.
3. **Read maps through `/proc/self/mem`.** Same result as 2, with more code.
4. **Read all values through `/proc/self/mem` or `process_vm_readv`** into byte buffers, and decode them with `reflect` type info, never making Go pointers from the program's data. This is the only fix that removes every crash cause above, because every read can fail with an error instead. It's a substantial rewrite of `rt/rt.go`, and it would stay much faster than delve, with no DWARF loading, no shared lock and an optimized program.
