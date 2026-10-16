package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestExample builds testdata/example under argmatch, runs it, and checks
// the matches reported for F.
func TestExample(t *testing.T) {
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
	toolexec := strings.Join([]string{`"` + wrapper + `"`, "-func", "main.ID.F", "-match", "needle",
		"-out", `"` + out + `"`, "-rtdir", `"` + rtDir + `"`}, " ")
	example := filepath.Join(tmp, "example")
	run(t, filepath.Join(rtDir, "testdata", "example"), "go", "build", "-toolexec", toolexec, "-o", example, ".")
	if got := run(t, tmp, example); got != "done\n" {
		t.Errorf("example printed %q, want %q", got, "done\n")
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := `argmatch: "needle" in main.ID.F: main.ID (id_recvr)
argmatch: "needle" in main.ID.F: main.ID2 (id_arg)
argmatch: "needle" in main.ID.F: main.Hier (h<key 0>)
argmatch: "needle" in main.ID.F: main.Named -> main.ID3 (in.Name)
argmatch: "needle" in main.ID.F: main.Outer -> main.Inner -> main.ID4 (o.P.Name)
argmatch: "needle" in main.ID.F: main.Iface (concrete main.ID5) (i)
`
	if string(got) != want {
		t.Errorf("matches:\n%s\nwant:\n%s", got, want)
	}
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
