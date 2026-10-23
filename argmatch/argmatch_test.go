package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
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
	if want := "main.ID.F: stopped loading at -maxtime"; !strings.Contains(stderr, want) {
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
	if want := "main.ID.F: stopped loading at -maxmem"; !strings.Contains(stderr, want) {
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

// TestByteSize checks how -maxmem sizes are parsed.
func TestByteSize(t *testing.T) {
	for s, want := range map[string]int64{
		"0": 0, "123": 123, "2KB": 2000, "512MB": 512e6, "1GB": 1e9,
		"1KiB": 1024, "3MiB": 3 << 20, "2GiB": 2 << 30, "7B": 7, "1 GiB": 1 << 30,
	} {
		var b byteSize
		if err := b.Set(s); err != nil || int64(b) != want {
			t.Errorf("Set(%q) = %d, %v; want %d", s, b, err, want)
		}
	}
	for _, s := range []string{"", "x", "-1", "1TB", "1.5GB", "99999999999GiB"} {
		var b byteSize
		if err := b.Set(s); err == nil {
			t.Errorf("Set(%q) = %d, want an error", s, b)
		}
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
// checked (3 by default, 0 for all): Repeat is called 5 times.
func TestMaxCalls(t *testing.T) {
	const row = "needle,main.Repeat,in.Name,main.Named,main.ID3\n"
	for _, test := range []struct {
		flags []string
		rows  int
	}{
		{nil, 3},
		{[]string{"-maxcalls", "1"}, 1},
		{[]string{"-maxcalls", "0"}, 5},
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

// TestClosure checks that with -ignoreclosures=false, matches reached
// through a func value's captured variables are reported, marked with the
// closure and the captured variable. With -exact=false as well, all the
// columns are written.
func TestClosure(t *testing.T) {
	got := runExample(t, "-func", "main.G", "-match", "needle", "-ignoreclosures=false", "-exact=false")
	want := "needle,haystack,function,expression,closure,types\n" +
		`needle,needle,main.G,r.Fn[captured receiver].Path,true,main.Reloader,closure main.(*Handler).Apply-fm,main.Handler,main.ID
needle,needle,main.G,named[captured named].Name,true,closure main.main.func1,main.Named,main.ID3
needle,needle,main.G,basic[captured needle],true,closure main.main.func2
`
	if got != want {
		t.Errorf("matches:\n%s\nwant:\n%s", got, want)
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
	wrapper := filepath.Join(tmp, "argmatch")
	run(t, rtDir, "go", "build", "-o", wrapper, ".")

	out := filepath.Join(tmp, "out.txt")
	toolexec := strings.Join(append([]string{`"` + wrapper + `"`,
		"-out", `"` + out + `"`, "-rtdir", `"` + rtDir + `"`}, flags...), " ")
	example := filepath.Join(tmp, "example")
	run(t, filepath.Join(rtDir, "testdata", "example"), "go", "build", "-toolexec", toolexec, "-o", example, ".")
	got, stderr := runStderr(t, tmp, example)
	if got != "done\n" {
		t.Errorf("example printed %q, want %q", got, "done\n")
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(data), stderr
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
