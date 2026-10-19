package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wantF is the output for F with -match needle.
const wantF = `argmatch: value "needle" contains "needle" in main.ID.F: main.ID (id_recvr)
argmatch: value "needle" contains "needle" in main.ID.F: main.ID2 (id_arg)
argmatch: value "needle" contains "needle" in main.ID.F: main.Hier (h<key 0>)
argmatch: value "needle" contains "needle" in main.ID.F: main.Named -> main.ID3 (in.Name)
argmatch: value "needle" contains "needle" in main.ID.F: main.Outer -> main.Inner -> main.ID4 (o.P.Name)
argmatch: value "needle" contains "needle" in main.ID.F: main.Iface (concrete main.ID5) (i)
`

// TestExample builds testdata/example under argmatch with -match, runs it,
// and checks the matches reported for F.
func TestExample(t *testing.T) {
	got := runExample(t, "-func", "main.ID.F", "-match", "needle")
	if got != wantF {
		t.Errorf("matches:\n%s\nwant:\n%s", got, wantF)
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
	// blank line, which is ignored.
	if err := os.WriteFile(matches, []byte("eedl\nneedle\n\nnot present\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := runExample(t, "-func", "main.ID.F", "-matchfile", `"`+matches+`"`)
	var want strings.Builder
	for _, m := range []string{
		`main.ID (id_recvr)`,
		`main.ID2 (id_arg)`,
		`main.Hier (h<key 0>)`,
		`main.Named -> main.ID3 (in.Name)`,
		`main.Outer -> main.Inner -> main.ID4 (o.P.Name)`,
		`main.Iface (concrete main.ID5) (i)`,
	} {
		want.WriteString(`argmatch: value "needle" contains "eedl" in main.ID.F: ` + m + "\n")
		want.WriteString(`argmatch: value "needle" contains "needle" in main.ID.F: ` + m + "\n")
	}
	if got != want.String() {
		t.Errorf("matches:\n%s\nwant:\n%s", got, want.String())
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
