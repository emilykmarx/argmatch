package settings

import (
	"testing"
	"time"
)

// TestByteSize checks how -maxmem sizes are parsed.
func TestByteSize(t *testing.T) {
	for s, want := range map[string]int64{
		"0": 0, "123": 123, "2KB": 2000, "512MB": 512e6, "1GB": 1e9,
		"1KiB": 1024, "3MiB": 3 << 20, "2GiB": 2 << 30, "7B": 7, "1 GiB": 1 << 30,
	} {
		var b ByteSize
		if err := b.Set(s); err != nil || int64(b) != want {
			t.Errorf("Set(%q) = %d, %v; want %d", s, b, err, want)
		}
	}
	for _, s := range []string{"", "x", "-1", "1TB", "1.5GB", "99999999999GiB"} {
		var b ByteSize
		if err := b.Set(s); err == nil {
			t.Errorf("Set(%q) = %d, want an error", s, b)
		}
	}
}

// TestEncode checks that settings survive Encode and Decode.
func TestEncode(t *testing.T) {
	s := Default()
	s.Needles = []string{"a", "b,c", "\"q\"\n"}
	s.IgnoreTypes = []string{"config.Config"}
	s.MaxTime = 10 * time.Second
	s.MaxMem = 1e9
	s.Out = "/out.csv"
	got, err := Decode(s.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Encode() != s.Encode() {
		t.Errorf("Decode(Encode(%+v)) = %+v", s, got)
	}
}

// TestFromEnv checks that environment variables override settings, and
// that a bad value is reported but doesn't stop the others.
func TestFromEnv(t *testing.T) {
	t.Setenv("ARGMATCH_MATCH", "x")
	t.Setenv("ARGMATCH_DEPTH", "20")
	t.Setenv("ARGMATCH_EXACT", "false")
	t.Setenv("ARGMATCH_MAXMEM", "1GB")
	t.Setenv("ARGMATCH_MAXTIME", "nope")
	t.Setenv("ARGMATCH_IGNORETYPES", "a.T, b.U")
	s := Default()
	s.Needles = []string{"old"}
	used, err := s.FromEnv()
	if err == nil {
		t.Error("no error for ARGMATCH_MAXTIME=nope")
	}
	if len(used) != 6 {
		t.Errorf("used = %q, want all 6 variables", used)
	}
	want := Default()
	want.Needles = []string{"x"}
	want.Depth = 20
	want.Exact = false
	want.MaxMem = 1e9
	want.IgnoreTypes = []string{"a.T", "b.U"}
	if s.Encode() != want.Encode() {
		t.Errorf("got %+v, want %+v", s, want)
	}
}
