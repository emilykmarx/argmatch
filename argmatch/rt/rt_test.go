package rt

import (
	"strings"
	"testing"
)

func TestEscape(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"/etc/prometheus/prometheus.yml", "/etc/prometheus/prometheus.yml"},
		{`say "hi"`, `say "hi"`},
		{"héllo, 世界", "héllo, 世界"},
		{"a\x00b", `a\x00b`},
		{"tab\tnewline\n", `tab\tnewline\n`},
		{`back\slash`, `back\\slash`},
		{`literal \x00`, `literal \\x00`},
		{"bad \xff utf8", `bad \xff utf8`},
		{"\x01job\x08__name__", `\x01job\b__name__`},
		{string(rune(0x200b)), `\` + "u200b"}, // zero-width space
	} {
		if got := escape(test.in); got != test.want {
			t.Errorf("escape(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

// stackWithAncestors is runtime.Stack's output for goroutine 8, created by
// goroutine 7, with GODEBUG=tracebackancestors=100.
const stackWithAncestors = `goroutine 8 [running]:
argmatch/rt.Check(0x1, ...)
	/rt/rt.go:11 +0x47
main.leafA(...)
	/tmp/main.go:15
main.worker(0x0?)
	/tmp/main.go:20 +0x4c
created by main.parent in goroutine 7
	/tmp/main.go:25 +0x4f
[originating from goroutine 7]:
main.parent(...)
	/tmp/main.go:26 +0x4f
created by main.main
	/tmp/main.go:31 +0x65
[originating from goroutine 1]:
main.main(...)
	/tmp/main.go:32 +0x65
`

func TestSplitStack(t *testing.T) {
	for _, test := range []struct {
		name, in, ancestry, stack string
	}{
		{"ancestors", stackWithAncestors, `goroutine 8
created by main.parent in goroutine 7
	/tmp/main.go:25 +0x4f
[originating from goroutine 7]:
main.parent(...)
	/tmp/main.go:26 +0x4f
created by main.main
	/tmp/main.go:31 +0x65
[originating from goroutine 1]:
main.main(...)
	/tmp/main.go:32 +0x65`, `goroutine 8 [running]:
main.leafA(...)
	/tmp/main.go:15
main.worker(0x0?)
	/tmp/main.go:20 +0x4c`},
		{"main goroutine", "goroutine 1 [running, locked to thread]:\nargmatch/rt.Check()\n\t/rt/rt.go:1 +0x1\nmain.main()\n\t/tmp/main.go:3 +0x2\n",
			"goroutine 1", "goroutine 1 [running, locked to thread]:\nmain.main()\n\t/tmp/main.go:3 +0x2"},
	} {
		ancestry, stack := splitStack(test.in, isRT)
		if ancestry != test.ancestry {
			t.Errorf("%s: ancestry:\n%s\nwant:\n%s", test.name, ancestry, test.ancestry)
		}
		if stack != test.stack {
			t.Errorf("%s: stack:\n%s\nwant:\n%s", test.name, stack, test.stack)
		}
	}
}

// TestAncestrySameInGoroutine checks that the ancestry is the same at two
// different points in one goroutine, and differs between goroutines.
func TestAncestrySameInGoroutine(t *testing.T) {
	all := func(string) bool { return false }
	ancestries := make(chan [2]string)
	for range 2 {
		go func() {
			a1, _ := splitStack(goroutineStack(), all)
			a2, _ := func() (string, string) { return splitStack(goroutineStack(), all) }()
			ancestries <- [2]string{a1, a2}
		}()
	}
	a, b := <-ancestries, <-ancestries
	for _, pair := range [][2]string{a, b} {
		if pair[0] != pair[1] {
			t.Errorf("ancestry differs within a goroutine:\n%s\n---\n%s", pair[0], pair[1])
		}
		if !strings.Contains(pair[0], "\ncreated by argmatch/rt.TestAncestrySameInGoroutine") {
			t.Errorf("ancestry has no created by line:\n%s", pair[0])
		}
	}
	if a[0] == b[0] {
		t.Errorf("two goroutines have the same ancestry:\n%s", a[0])
	}
}
