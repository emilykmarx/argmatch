package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// header is the output's header with the default -exact and
// -ignoreclosures, which leave out the haystack and closure columns.
const header = "needle,function,expression,types\n"

// wantF is the output for F with -match needle.
const wantF = header + `needle,main.ID.F,id_recvr,main.ID
needle,main.ID.F,id_arg,main.ID2
needle,main.ID.F,"h<key ""needle"">",main.Hier
needle,main.ID.F,in.Name,main.Named,main.ID3
needle,main.ID.F,o.P.Name,main.Outer,main.Inner,main.ID4
needle,main.ID.F,i,main.ID5 (implements main.Iface)
`

// TestExample builds testdata/example under argmatch with -match, runs it,
// and checks the matches reported for F.
func TestExample(t *testing.T) {
	got := runExample(t, "-func", "main.ID.F", "-match", "needle")
	if got != wantF {
		t.Errorf("matches:\n%s\nwant:\n%s", got, wantF)
	}
}

// TestRecvOnly checks that -recvonly checks the receiver but not the
// parameters.
func TestRecvOnly(t *testing.T) {
	got := runExample(t, "-func", "main.ID.F", "-match", "needle", "-recvonly")
	want := header + "needle,main.ID.F,id_recvr,main.ID\n"
	if got != want {
		t.Errorf("matches:\n%s\nwant:\n%s", got, want)
	}
}

// TestMaxElems checks that -maxelems limits how many map entries are
// loaded: with 0, the match in h's key isn't found.
func TestMaxElems(t *testing.T) {
	got := runExample(t, "-func", "main.ID.F", "-match", "needle", "-maxelems", "0")
	want := strings.Replace(wantF, "needle,main.ID.F,\"h<key \"\"needle\"\">\",main.Hier\n", "", 1)
	if want == wantF {
		t.Fatal("test bug: h row not in wantF")
	}
	if got != want {
		t.Errorf("matches:\n%s\nwant:\n%s", got, want)
	}
}

// TestMaxFields checks that -maxfields limits how many struct fields are
// loaded: with 0, the matches in in.Name and o.P.Name aren't found.
func TestMaxFields(t *testing.T) {
	got := runExample(t, "-func", "main.ID.F", "-match", "needle", "-maxfields", "0")
	want := wantF
	for _, row := range []string{
		"needle,main.ID.F,in.Name,main.Named,main.ID3\n",
		"needle,main.ID.F,o.P.Name,main.Outer,main.Inner,main.ID4\n",
	} {
		if !strings.Contains(want, row) {
			t.Fatalf("test bug: %q not in wantF", row)
		}
		want = strings.Replace(want, row, "", 1)
	}
	if got != want {
		t.Errorf("matches:\n%s\nwant:\n%s", got, want)
	}
}

// TestMaxTime checks that loading stops at -maxtime and the stop is
// reported: with 1ns, nothing is loaded. A generous limit changes nothing.
func TestMaxTime(t *testing.T) {
	got, stderr := runExampleStderr(t, "-func", "main.ID.F", "-match", "needle", "-maxtime", "1ns")
	if got != header {
		t.Errorf("matches:\n%s\nwant only the header", got)
	}
	if want := "main.ID.F: stopped at -maxtime"; !strings.Contains(stderr, want) {
		t.Errorf("stderr:\n%s\nwant %q", stderr, want)
	}
	if got, stderr := runExampleStderr(t, "-func", "main.ID.F", "-match", "needle", "-maxtime", "1m"); got != wantF || stderr != "" {
		t.Errorf("-maxtime 1m: matches:\n%s\nstderr:\n%s\nwant:\n%s", got, stderr, wantF)
	}
}

// TestMaxMem checks that loading stops at -maxmem and the stop is
// reported, keeping the matches found so far. A generous limit changes
// nothing.
func TestMaxMem(t *testing.T) {
	got, stderr := runExampleStderr(t, "-func", "main.ID.F", "-match", "needle", "-maxmem", "1")
	if want := "main.ID.F: stopped at -maxmem"; !strings.Contains(stderr, want) {
		t.Errorf("stderr:\n%s\nwant %q", stderr, want)
	}
	// What's found depends on when the limit was hit, but it's a prefix of
	// the full output.
	if !strings.HasPrefix(wantF, got) || !strings.HasPrefix(got, header) {
		t.Errorf("matches:\n%s\nwant a prefix of:\n%s", got, wantF)
	}
	if got, stderr := runExampleStderr(t, "-func", "main.ID.F", "-match", "needle", "-maxmem", "1GiB"); got != wantF || stderr != "" {
		t.Errorf("-maxmem 1GiB: matches:\n%s\nstderr:\n%s\nwant:\n%s", got, stderr, wantF)
	}
}

// TestEnvOverride checks that ARGMATCH_* environment variables override
// the run-time settings when the program runs, without a rebuild, and that
// the program says which ones it used.
func TestEnvOverride(t *testing.T) {
	e := buildExample(t, "-func", "main.ID.F", "-match", "needle")
	if got, stderr := e.run(t); got != wantF || stderr != "" {
		t.Fatalf("matches:\n%s\nstderr:\n%s\nwant:\n%s", got, stderr, wantF)
	}
	got, stderr := e.run(t, "ARGMATCH_MATCH=eedl", "ARGMATCH_EXACT=false", "ARGMATCH_MAXELEMS=0")
	want := "needle,haystack,function,expression,types\n" +
		"eedl,needle,main.ID.F,id_recvr,main.ID\n" +
		"eedl,needle,main.ID.F,id_arg,main.ID2\n" +
		"eedl,needle,main.ID.F,in.Name,main.Named,main.ID3\n" +
		"eedl,needle,main.ID.F,o.P.Name,main.Outer,main.Inner,main.ID4\n" +
		"eedl,needle,main.ID.F,i,main.ID5 (implements main.Iface)\n"
	if got != want {
		t.Errorf("with environment: matches:\n%s\nwant:\n%s", got, want)
	}
	if w := "argmatch: settings from the environment: ARGMATCH_MATCH, ARGMATCH_EXACT, ARGMATCH_MAXELEMS\n"; stderr != w {
		t.Errorf("with environment: stderr %q, want %q", stderr, w)
	}
}

// TestRunTimeFlagsDontRecompile checks that changing only run-time flags
// relinks the program without recompiling any package, and that the new
// values take effect.
func TestRunTimeFlagsDontRecompile(t *testing.T) {
	e := buildExample(t, "-func", "main.ID.F", "-match", "needle")
	stderr := e.goBuild(t, []string{"-x"}, "-func", "main.ID.F", "-match", "eedl", "-exact=false", "-maxelems", "0", "-depth", "7")
	var compiles, links int
	for _, line := range strings.Split(stderr, "\n") {
		switch {
		case strings.Contains(line, "/compile ") || strings.Contains(line, "/compile\""):
			compiles++
		case strings.Contains(line, "/link ") || strings.Contains(line, "/link\""):
			links++
		}
	}
	if compiles != 0 || links != 1 {
		t.Errorf("rebuild ran %d compiles and %d links, want 0 and 1", compiles, links)
	}
	if got, _ := e.run(t); !strings.HasPrefix(got, "needle,haystack,function,expression,types\needl,needle,") {
		t.Errorf("after rebuild: matches:\n%s\nwant eedl rows", got)
	}
	// Changing which functions are instrumented does recompile. The
	// function needn't exist; a name not used before keeps the build cache
	// from having the result already.
	fn := fmt.Sprintf("main.NotThere%d", time.Now().UnixNano())
	stderr = e.goBuild(t, []string{"-x"}, "-func", fn, "-match", "needle")
	if !strings.Contains(stderr, "/compile") {
		t.Error("changing -func didn't recompile")
	}
}

// TestIgnoreLibs checks that by default the fields of standard library
// structs aren't loaded: Lib's only match is in a url.URL's field.
func TestIgnoreLibs(t *testing.T) {
	if got := runExample(t, "-func", "main.Lib", "-match", "needle"); got != header {
		t.Errorf("matches:\n%s\nwant only the header", got)
	}
	got := runExample(t, "-func", "main.Lib", "-match", "needle", "-ignorelibs=false")
	if want := header + "needle,main.Lib,u.Host,net/url.URL\n"; got != want {
		t.Errorf("-ignorelibs=false: matches:\n%s\nwant:\n%s", got, want)
	}
}

// TestIgnoreTypes checks that the fields of -ignoretypes structs aren't
// loaded, whether given by full name or by the end after a slash.
func TestIgnoreTypes(t *testing.T) {
	got := runExample(t, "-func", "main.ID.F", "-match", "needle", "-ignoretypes", "main.Outer")
	want := strings.Replace(wantF, "needle,main.ID.F,o.P.Name,main.Outer,main.Inner,main.ID4\n", "", 1)
	if want == wantF {
		t.Fatal("test bug: o.P.Name row not in wantF")
	}
	if got != want {
		t.Errorf("-ignoretypes main.Outer: matches:\n%s\nwant:\n%s", got, want)
	}
	if got := runExample(t, "-func", "main.Lib", "-match", "needle", "-ignorelibs=false", "-ignoretypes", "main.Outer,url.URL"); got != header {
		t.Errorf("-ignoretypes url.URL: matches:\n%s\nwant only the header", got)
	}
}

// TestMaxCalls checks that only the first -maxcalls calls of a function are
// checked (3 by default, 0 for all), and that -callinterval skips calls
// soon after a check: Repeat is called 5 times in quick succession.
func TestMaxCalls(t *testing.T) {
	const row = "needle,main.Repeat,in.Name,main.Named,main.ID3\n"
	for _, test := range []struct {
		flags []string
		rows  int
	}{
		{nil, 3},
		{[]string{"-maxcalls", "1"}, 1},
		{[]string{"-maxcalls", "0"}, 5},
		{[]string{"-maxcalls", "0", "-callinterval", "1h"}, 1},
		{[]string{"-maxcalls", "0", "-callinterval", "1ns"}, 5},
		{[]string{"-callinterval", "1h"}, 1},
	} {
		got := runExample(t, append([]string{"-func", "main.Repeat", "-match", "needle"}, test.flags...)...)
		if want := header + strings.Repeat(row, test.rows); got != want {
			t.Errorf("%q: matches:\n%s\nwant:\n%s", test.flags, got, want)
		}
	}
}

// TestIgnoreClosuresDefault checks that by default the variables captured
// by func values aren't checked: G's only matches are through closures.
func TestIgnoreClosuresDefault(t *testing.T) {
	if got := runExample(t, "-func", "main.G", "-match", "needle"); got != header {
		t.Errorf("matches:\n%s\nwant only the header", got)
	}
}

// TestClosuresUnsupported checks that -ignoreclosures=false is refused,
// since the type information from reflect doesn't describe the variables
// closures capture.
func TestClosuresUnsupported(t *testing.T) {
	tmp := t.TempDir()
	wrapper := filepath.Join(tmp, "argmatch")
	run(t, ".", "go", "build", "-o", wrapper, ".")
	cmd := exec.Command(wrapper, "-func", "main.G", "-match", "needle", "-ignoreclosures=false", "compile")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "-ignoreclosures=false isn't supported") {
		t.Errorf("argmatch -ignoreclosures=false: err %v, output %q; want an error saying it isn't supported", err, out)
	}
}

// TestStackLog checks that the functions in the stacks of -stacklog are
// checked. testdata/stacks.log has F, a closure in F (which means F),
// main.main and standard library functions, which are all ignored, so the
// output is the same as for -func main.ID.F.
func TestStackLog(t *testing.T) {
	stacks, err := filepath.Abs(filepath.Join("testdata", "stacks.log"))
	if err != nil {
		t.Fatal(err)
	}
	got := runExample(t, "-stacklog", `"`+stacks+`"`, "-match", "needle")
	if got != wantF {
		t.Errorf("matches:\n%s\nwant:\n%s", got, wantF)
	}
}

// TestExactDefault checks that by default a value must equal the match
// string: "eedl" is in "needle" but isn't reported.
func TestExactDefault(t *testing.T) {
	if got := runExample(t, "-func", "main.ID.F", "-match", "eedl"); got != header {
		t.Errorf("matches:\n%s\nwant only the header", got)
	}
}

// TestMatchFile checks that every value is checked against every string in
// -matchfile, and that each match names the string it matched. It uses
// -exact=false, so that partial matches are reported too.
func TestMatchFile(t *testing.T) {
	matches := filepath.Join(t.TempDir(), "matches.txt")
	// A partial match, the whole value, a string that never matches, and a
	// blank line and a comment, which are ignored.
	if err := os.WriteFile(matches, []byte("# comment\needl\nneedle\n\nnot present\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runExample(t, "-func", "main.ID.F", "-matchfile", `"`+matches+`"`, "-exact=false")
	var want strings.Builder
	want.WriteString("needle,haystack,function,expression,types\n")
	for _, m := range []string{
		`id_recvr,main.ID`,
		`id_arg,main.ID2`,
		`"h<key ""needle"">",main.Hier`,
		`in.Name,main.Named,main.ID3`,
		`o.P.Name,main.Outer,main.Inner,main.ID4`,
		`i,main.ID5 (implements main.Iface)`,
	} {
		want.WriteString("eedl,needle,main.ID.F," + m + "\n")
		want.WriteString("needle,needle,main.ID.F," + m + "\n")
	}
	if got != want.String() {
		t.Errorf("matches:\n%s\nwant:\n%s", got, want.String())
	}
}

// TestLoadNeedles checks which lines of -matchfile are used.
func TestLoadNeedles(t *testing.T) {
	matches := filepath.Join(t.TempDir(), "matches.txt")
	data := "# a comment\n#needle\n a\n\nb\r\na\nb # not a comment\nb\n"
	if err := os.WriteFile(matches, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func(n, m string) { *needle, *matchFile = n, m }(*needle, *matchFile)
	*needle, *matchFile = "a", matches
	got := loadNeedles()
	want := []string{"a", " a", "b", "b # not a comment"}
	if !slices.Equal(got, want) {
		t.Errorf("loadNeedles() = %q, want %q", got, want)
	}
}

// runExample builds testdata/example under argmatch with the given wrapper
// flags, runs it, and returns the matches it reports, failing if the
// program printed anything to stderr.
func runExample(t *testing.T, flags ...string) string {
	t.Helper()
	got, stderr := runExampleStderr(t, flags...)
	if stderr != "" {
		t.Errorf("stderr:\n%s", stderr)
	}
	return got
}

// runExampleStderr is like runExample, but returns the program's stderr
// too.
func runExampleStderr(t *testing.T, flags ...string) (matches, stderr string) {
	t.Helper()
	e := buildExample(t, flags...)
	return e.run(t)
}

// example is testdata/example built under argmatch.
type example struct {
	tmp, wrapper, rtDir string
	exe, out            string // the program and the file it writes matches to
}

// buildExample builds the wrapper and then testdata/example under it with
// the given wrapper flags.
func buildExample(t *testing.T, flags ...string) *example {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("rt reads /proc/self")
	}
	if testing.Short() {
		t.Skip("builds the standard library without optimizations")
	}
	rtDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	e := &example{tmp: tmp, wrapper: filepath.Join(tmp, "argmatch"), rtDir: rtDir,
		exe: filepath.Join(tmp, "example"), out: filepath.Join(tmp, "out.txt")}
	run(t, rtDir, "go", "build", "-o", e.wrapper, ".")
	e.build(t, flags...)
	return e
}

// build (re)builds the example under the wrapper with the given flags, and
// returns the go command's stderr.
func (e *example) build(t *testing.T, flags ...string) string {
	t.Helper()
	return e.goBuild(t, nil, flags...)
}

// goBuild runs go build under the wrapper with the given go build flags
// (e.g. -x) and wrapper flags, and returns the go command's stderr.
func (e *example) goBuild(t *testing.T, goFlags []string, flags ...string) string {
	t.Helper()
	toolexec := strings.Join(append([]string{`"` + e.wrapper + `"`,
		"-out", `"` + e.out + `"`, "-rtdir", `"` + e.rtDir + `"`}, flags...), " ")
	args := append(append([]string{"build", "-toolexec", toolexec}, goFlags...), "-o", e.exe, ".")
	_, stderr := runStderr(t, filepath.Join(e.rtDir, "testdata", "example"), "go", args...)
	return stderr
}

// run runs the example, with env added to its environment, and returns
// the matches it wrote and its stderr.
func (e *example) run(t *testing.T, env ...string) (matches, stderr string) {
	t.Helper()
	cmd := exec.Command(e.exe)
	cmd.Dir = e.tmp
	cmd.Env = append(os.Environ(), env...)
	var errb strings.Builder
	cmd.Stderr = &errb
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v\n%s", e.exe, err, errb.String())
	}
	if string(got) != "done\n" {
		t.Errorf("example printed %q, want %q", got, "done\n")
	}
	data, err := os.ReadFile(e.out)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), errb.String()
}

// run runs a command in dir and returns its stdout, failing the test if
// the command fails.
func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	out, _ := runStderr(t, dir, name, args...)
	return out
}

// runStderr is like run, but returns the command's stderr too.
func runStderr(t *testing.T, dir, name string, args ...string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var errb strings.Builder
	cmd.Stderr = &errb
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, errb.String())
	}
	return string(out), errb.String()
}

// TestMaps checks that maps are decoded from the runtime's map layout:
// small maps, maps big enough for tables, indirect (over 128-byte) keys and
// values, basic and struct keys, nested maps and a map in an interface. The
// expected rows are what reflect's own map iteration found (see the
// reflect-port branch); all entries fit in -maxelems, so the order in which
// entries are walked doesn't matter.
func TestMaps(t *testing.T) {
	if testing.Short() {
		t.Skip("rebuilds the standard library under argmatch")
	}
	rtDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	wrapper := filepath.Join(tmp, "argmatch")
	run(t, rtDir, "go", "build", "-o", wrapper, ".")
	out := filepath.Join(tmp, "out.txt")
	toolexec := strings.Join([]string{`"` + wrapper + `"`, "-out", `"` + out + `"`, "-rtdir", `"` + rtDir + `"`,
		"-func", "main.(*Holder).F", "-match", "needle", "-maxelems", "100", "-depth", "10"}, " ")
	prog := filepath.Join(tmp, "maps")
	run(t, filepath.Join(rtDir, "testdata", "maps"), "go", "build", "-toolexec", toolexec, "-o", prog, ".")
	run(t, tmp, prog)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(data)), "\n")
	slices.Sort(got)
	want := []string{
		`needle,function,expression,types`,
		`needle,main.(*Holder).F,h.Any[-3],main.Holder,map[int8]main.Name (implements interface {}),main.Name`,
		`needle,main.(*Holder).F,h.BigKey[<key 0>],main.Holder,main.Name`,
		`needle,main.(*Holder).F,"h.BigVal[""k""].Name",main.Holder,main.Big,main.Name`,
		`needle,main.(*Holder).F,h.Bools[true].Name,main.Holder,main.Big,main.Name`,
		`needle,main.(*Holder).F,h.Large[-5],main.Holder,main.Name`,
		`needle,main.(*Holder).F,"h.Nested[""outer""][65535]",main.Holder,main.Name`,
		`needle,main.(*Holder).F,h.SK[<key 0>],main.Holder,main.Name`,
		`needle,main.(*Holder).F,"h.Small[""a""]",main.Holder,main.Name`,
		`needle,main.(*Holder).F,"h.Strs<key ""needle"">",main.Holder,main.Name`,
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("matches:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
