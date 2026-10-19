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

const header = "needle,haystack,function,expression,closure,types\n"

// wantF is the output for F with -match needle.
const wantF = header + `needle,needle,main.ID.F,id_recvr,false,main.ID
needle,needle,main.ID.F,id_arg,false,main.ID2
needle,needle,main.ID.F,h<key 0>,false,main.Hier
needle,needle,main.ID.F,in.Name,false,main.Named,main.ID3
needle,needle,main.ID.F,o.P.Name,false,main.Outer,main.Inner,main.ID4
needle,needle,main.ID.F,i,false,main.ID5 (implements main.Iface)
`

// TestExample builds testdata/example under argmatch with -match, runs it,
// and checks the matches reported for F.
func TestExample(t *testing.T) {
	got := runExample(t, "-func", "main.ID.F", "-match", "needle")
	if got != wantF {
		t.Errorf("matches:\n%s\nwant:\n%s", got, wantF)
	}
}

// TestClosure checks that matches reached through a func value's captured
// variables are marked with the closure and the captured variable.
func TestClosure(t *testing.T) {
	got := runExample(t, "-func", "main.G", "-match", "needle")
	want := header + `needle,needle,main.G,r.Fn[captured receiver].Path,true,main.Reloader,closure main.(*Handler).Apply-fm,main.Handler,main.ID
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

// TestMatchFile checks that every value is checked against every string in
// -matchfile, and that each match names the string it matched.
func TestMatchFile(t *testing.T) {
	matches := filepath.Join(t.TempDir(), "matches.txt")
	// A partial match, the whole value, a string that never matches, and a
	// blank line and a comment, which are ignored.
	if err := os.WriteFile(matches, []byte("# comment\needl\nneedle\n\nnot present\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runExample(t, "-func", "main.ID.F", "-matchfile", `"`+matches+`"`)
	var want strings.Builder
	want.WriteString(header)
	for _, m := range []string{
		`id_recvr,false,main.ID`,
		`id_arg,false,main.ID2`,
		`h<key 0>,false,main.Hier`,
		`in.Name,false,main.Named,main.ID3`,
		`o.P.Name,false,main.Outer,main.Inner,main.ID4`,
		`i,false,main.ID5 (implements main.Iface)`,
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
