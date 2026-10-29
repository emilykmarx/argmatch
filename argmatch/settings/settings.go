// Package settings holds argmatch's run-time settings: those that change
// only what rt does while the program runs, not which code is
// instrumented. The wrapper passes them to the linker rather than compiling
// them into the instrumented packages, so changing them relinks the
// program without recompiling it. The program can also override them with
// ARGMATCH_* environment variables when it starts (see FromEnv).
package settings

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Settings are argmatch's run-time settings. See the wrapper's flags of the
// same names for what each one means.
type Settings struct {
	Needles        []string
	Exact          bool
	IgnoreClosures bool
	IgnoreLibs     bool
	IgnoreTypes    []string
	Depth          int
	MaxElems       int
	MaxFields      int
	MaxCalls       int
	CallInterval   time.Duration
	MaxTime        time.Duration
	MaxMem         int64
	Out            string // "" for stderr
	Stats          bool   // print each check's time and read counts to stderr
}

// Default returns the default settings, with no needles.
func Default() Settings {
	return Settings{
		Exact:          true,
		IgnoreClosures: true,
		IgnoreLibs:     true,
		Depth:          5,
		MaxElems:       64,
		MaxFields:      -1,
		MaxCalls:       3,
	}
}

// Encode returns s as a string of base64 characters, which can be passed
// in a linker -X flag without any quoting.
func (s Settings) Encode() string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // can't happen: s has only plain fields
	}
	return base64.StdEncoding.EncodeToString(b)
}

// Decode returns the settings Encode encoded as enc.
func Decode(enc string) (Settings, error) {
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return Settings{}, err
	}
	s := Default()
	err = json.Unmarshal(b, &s)
	return s, err
}

// FromEnv overrides s with the ARGMATCH_* environment variables that are
// set to a non-empty value, as the wrapper reads them: ARGMATCH_MATCH and
// ARGMATCH_MATCHFILE (which, if either is set, replace the needles),
// ARGMATCH_EXACT, ARGMATCH_IGNORECLOSURES, ARGMATCH_IGNORELIBS,
// ARGMATCH_IGNORETYPES, ARGMATCH_DEPTH, ARGMATCH_MAXELEMS,
// ARGMATCH_MAXFIELDS, ARGMATCH_MAXCALLS, ARGMATCH_CALLINTERVAL,
// ARGMATCH_MAXTIME, ARGMATCH_MAXMEM,
// ARGMATCH_OUT and ARGMATCH_STATS. It returns the names of the variables it used, and an
// error for the first one it couldn't use; the others are still applied.
func (s *Settings) FromEnv() (used []string, err error) {
	fail := func(name string, e error) {
		if err == nil {
			err = fmt.Errorf("%s: %v", name, e)
		}
	}
	get := func(name string) (string, bool) {
		v := os.Getenv(name)
		if v != "" {
			used = append(used, name)
		}
		return v, v != ""
	}
	setBool := func(name string, p *bool) {
		if v, ok := get(name); ok {
			b, e := strconv.ParseBool(v)
			if e != nil {
				fail(name, e)
				return
			}
			*p = b
		}
	}
	setInt := func(name string, p *int) {
		if v, ok := get(name); ok {
			n, e := strconv.Atoi(v)
			if e != nil {
				fail(name, e)
				return
			}
			*p = n
		}
	}
	match, hasMatch := get("ARGMATCH_MATCH")
	file, hasFile := get("ARGMATCH_MATCHFILE")
	if hasMatch || hasFile {
		needles, e := LoadNeedles(match, file)
		if e != nil {
			fail("ARGMATCH_MATCHFILE", e)
		} else {
			s.Needles = needles
		}
	}
	setBool("ARGMATCH_EXACT", &s.Exact)
	setBool("ARGMATCH_IGNORECLOSURES", &s.IgnoreClosures)
	setBool("ARGMATCH_IGNORELIBS", &s.IgnoreLibs)
	if v, ok := get("ARGMATCH_IGNORETYPES"); ok {
		s.IgnoreTypes = SplitList(v)
	}
	setInt("ARGMATCH_DEPTH", &s.Depth)
	setInt("ARGMATCH_MAXELEMS", &s.MaxElems)
	setInt("ARGMATCH_MAXFIELDS", &s.MaxFields)
	setInt("ARGMATCH_MAXCALLS", &s.MaxCalls)
	setDuration := func(name string, p *time.Duration) {
		if v, ok := get(name); ok {
			d, e := time.ParseDuration(v)
			if e != nil {
				fail(name, e)
				return
			}
			*p = d
		}
	}
	setDuration("ARGMATCH_CALLINTERVAL", &s.CallInterval)
	setDuration("ARGMATCH_MAXTIME", &s.MaxTime)
	if v, ok := get("ARGMATCH_MAXMEM"); ok {
		var b ByteSize
		if e := b.Set(v); e != nil {
			fail("ARGMATCH_MAXMEM", e)
		} else {
			s.MaxMem = int64(b)
		}
	}
	if v, ok := get("ARGMATCH_OUT"); ok {
		s.Out = v
	}
	setBool("ARGMATCH_STATS", &s.Stats)
	return used, err
}

// LoadNeedles returns match and the lines of the file at path (if path
// isn't "") that are neither blank nor comments (starting with #), without
// duplicates. Each line is used as is, apart from a trailing \r. path must
// be absolute.
func LoadNeedles(match, path string) ([]string, error) {
	var ns []string
	if match != "" {
		ns = append(ns, match)
	}
	if path != "" {
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("match file %s must be an absolute path", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for line := range strings.SplitSeq(string(data), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line != "" && !strings.HasPrefix(line, "#") && !slices.Contains(ns, line) {
				ns = append(ns, line)
			}
		}
	}
	if len(ns) == 0 {
		return nil, fmt.Errorf("no strings to match")
	}
	return ns, nil
}

// SplitList returns the non-empty, space-trimmed elements of a
// comma-separated list.
func SplitList(s string) []string {
	var list []string
	for e := range strings.SplitSeq(s, ",") {
		if e = strings.TrimSpace(e); e != "" {
			list = append(list, e)
		}
	}
	return list
}

// ByteSize is a number of bytes, written as a plain number or with a unit:
// KB, MB, GB (powers of 1000) or KiB, MiB, GiB (powers of 1024). It's a
// flag.Value.
type ByteSize int64

var byteUnits = []struct {
	suffix string
	size   int64
}{
	{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
	{"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"B", 1},
}

func (b *ByteSize) Set(s string) error {
	mult := int64(1)
	for _, u := range byteUnits {
		if t, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = strings.TrimSpace(t), u.size
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return fmt.Errorf("invalid size %q", s)
	}
	*b = ByteSize(n * mult)
	return nil
}

func (b *ByteSize) String() string { return strconv.FormatInt(int64(*b), 10) }
