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
// flags, runs it, and returns the matches it reports.
func runExample(t *testing.T, flags ...string) string {
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
	if got := run(t, tmp, example); got != "done\n" {
		t.Errorf("example printed %q, want %q", got, "done\n")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// run runs a command in dir and returns its stdout, failing the test if
// the command fails.
func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, stderr.String())
	}
	return string(out)
}
