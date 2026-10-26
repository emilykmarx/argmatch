// Package rt is linked into the program being built by argmatch. The
// toolexec wrapper injects a call to Check at the top of each target
// function; Check walks the function's receiver and arguments with reflect.
package rt

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"unsafe"
)

var (
	outOnce sync.Once
	outMu   sync.Mutex
	out     io.Writer = os.Stderr

	active sync.Map // IDs of goroutines running Check, to detect re-entry

	// useReflect selects the original walker, which reads values directly
	// with reflect and can crash the program, instead of the raw one
	// (PROTOTYPE: set ARGMATCH_WALKER=reflect when the program runs).
	useReflect = os.Getenv("ARGMATCH_WALKER") == "reflect"
)

// Check reports every string in args that equals one of needles (or, if
// exact is false, contains one), along with the named types and interfaces
// on the path down to it, as CSV rows (see options.header). names[i] is the
// source name of the parameter whose address is boxed in args[i]. Output
// goes to the file outPath, which the first call overwrites, or to stderr if
// outPath is "".
//
// Values are walked depth levels deep, and only the first maxElems elements
// of each array, slice and map. The variables captured by func values
// (closures) aren't walked; ignoreClosures is accepted for compatibility
// with the wrapper, which refuses -ignoreclosures=false.
//
// calls counts the calls of fn so far (each instrumented function has its
// own counter). Once it reaches maxCalls, Check returns at once, so later
// calls cost almost nothing. maxCalls 0 means no limit.
//
// If guard is set, the target function may be called by Check itself, so
// calls made while checking are ignored. That's only possible for functions
// in the standard library, where rt's dependencies are; finding the
// goroutine ID for the guard is relatively slow.
//
// Checks don't share a lock: each runs on its caller's goroutine, so a slow
// check only delays the call it's checking.
//
// The push linkname lets the injected bodyless declaration in the target
// package (which can't import rt) refer to this function.
//
//go:linkname Check
func Check(calls *uint32, maxCalls int, guard bool, needles []string, exact, ignoreClosures bool, depth, maxElems int, outPath string, fn string, names []string, args []any) {
	if maxCalls > 0 {
		// Load first, so the counter stops growing (and can't wrap) once
		// the limit is reached. Concurrent first calls may overshoot it.
		if atomic.LoadUint32(calls) >= uint32(maxCalls) {
			return
		}
		atomic.AddUint32(calls, 1)
	}
	if guard {
		gid := goid()
		if _, busy := active.LoadOrStore(gid, true); busy {
			return
		}
		defer active.Delete(gid)
	}

	o := options{needles: needles, exact: exact, depth: depth, maxElems: maxElems}
	outOnce.Do(func() {
		if outPath != "" {
			// Overwrite any output from an earlier run.
			f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				fmt.Fprintf(os.Stderr, "argmatch: %v; writing to stderr instead\n", err)
			} else {
				out = f
			}
		}
		outMu.Lock()
		writeCSV(out, [][]string{o.header()})
		outMu.Unlock()
	})

	w := walker{options: o, fn: fn}
	if useReflect {
		start := time.Now()
		for i, arg := range args {
			// arg holds *T, where T is the parameter's static type, so that an
			// interface parameter stays an interface.
			p := reflect.ValueOf(arg)
			if p.Kind() != reflect.Pointer || p.IsNil() {
				continue
			}
			w.expr = append(w.expr[:0], names[i]...)
			w.walk(p.Elem(), 0)
		}
		fmt.Fprintf(os.Stderr, "argmatch-stats: %s reflect %v rows=%d\n", fn, time.Since(start).Round(time.Microsecond), len(w.rows))
	} else {
		walkRaw(&w, names, args) // PROTOTYPE
	}
	if len(w.rows) == 0 {
		return
	}
	// One write per call, so that calls' rows aren't interleaved.
	outMu.Lock()
	writeCSV(out, w.rows)
	outMu.Unlock()
}

func writeCSV(out io.Writer, rows [][]string) {
	var buf bytes.Buffer
	c := csv.NewWriter(&buf)
	c.WriteAll(rows)
	out.Write(buf.Bytes())
}

// options control what the walk reports.
type options struct {
	needles  []string
	exact    bool // a string matches only if it equals the needle, not if it just contains it
	depth    int
	maxElems int
}

// matches reports whether s matches needle.
func (o options) matches(s, needle string) bool {
	if o.exact {
		return s == needle
	}
	return strings.Contains(s, needle)
}

// header returns the names of the output's columns. Each row has one more
// column per step of the path from the argument down to the match: a named
// type, or an interface ("concrete (implements interface)"). Rows have
// different lengths, so the last header names the first of them. With exact
// matching the haystack column is left out, since it would always equal the
// needle.
func (o options) header() []string {
	h := []string{"needle"}
	if !o.exact {
		h = append(h, "haystack")
	}
	return append(h, "function", "expression", "types")
}

// A walker walks one call's arguments. expr and path are stacks: each level
// of the walk appends to them, and truncates them back when done.
type walker struct {
	options
	fn string

	expr []byte // source expression of the current value, e.g. o.P.Name
	path []step // named types and interfaces on the path to it
	rows [][]string
}

// A step is a named type on the path to a match, or an interface (typ) with
// its concrete type.
type step struct {
	typ, concrete reflect.Type
}

func (s step) String() string {
	if s.concrete != nil {
		return typeName(s.concrete) + " (implements " + typeName(s.typ) + ")"
	}
	return typeName(s.typ)
}

// walk checks v and what it contains, adding v's type to the path if it's
// a named type or an interface. level counts nesting the way delve does
// when loading a variable with MaxVariableRecurse = depth, so -depth means
// what it did when argmatch used delve:
//   - a struct's fields and a map's keys and values are walked only while
//     level <= depth, and are one level deeper;
//   - an array's or slice's elements are always walked, one level deeper;
//   - a pointer's target is at the same level, unless it's a pointer or an
//     interface, which are one level deeper (and a pointer target is only
//     walked while level <= depth);
//   - an interface's dynamic value is at the same level, walked only while
//     level <= depth;
//   - strings are always checked.
func (w *walker) walk(v reflect.Value, level int) {
	n := len(w.path)
	defer func() { w.path = w.path[:n] }()
	t := v.Type()
	switch {
	case v.Kind() == reflect.Interface:
		if v.IsNil() || level > w.depth {
			return
		}
		// Report the interface together with its dynamic type, then
		// continue below the dynamic value so it isn't reported twice.
		data := v.Elem()
		w.path = append(w.path, step{t, data.Type()})
		w.walkValue(data, level)
		return
	case isNamed(t):
		w.path = append(w.path, step{typ: t})
	}
	w.walkValue(v, level)
}

// walkValue checks v itself, whose type is already accounted for in the
// path, then walks its contents.
func (w *walker) walkValue(v reflect.Value, level int) {
	if s, ok := stringValue(v); ok {
		if len(w.path) > 0 {
			for _, needle := range w.needles {
				if w.matches(s, needle) {
					w.report(s, needle)
				}
			}
		}
		return
	}
	n := len(w.expr)
	defer func() { w.expr = w.expr[:n] }()
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return
		}
		switch e := v.Elem(); e.Kind() {
		case reflect.Interface:
			w.walk(e, level+1)
		case reflect.Pointer:
			if level <= w.depth {
				w.walk(e, level+1)
			}
		default:
			w.walk(e, level)
		}
	case reflect.Struct:
		if level > w.depth {
			return
		}
		t := v.Type()
		for i := range v.NumField() {
			w.expr = append(append(w.expr[:n], '.'), t.Field(i).Name...)
			w.walk(v.Field(i), level+1)
		}
	case reflect.Map:
		if level > w.depth {
			return
		}
		it := v.MapRange()
		for i := 0; i < w.maxElems && it.Next(); i++ {
			// Name an entry by its key if it's a basic value; otherwise by
			// its position, which reflect randomizes on every iteration.
			keyExpr, valExpr := "<key "+strconv.Itoa(i)+">", ""
			if lit, ok := keyLiteral(it.Key()); ok {
				keyExpr, valExpr = "<key "+lit+">", "["+lit+"]"
			} else {
				valExpr = "[" + keyExpr + "]"
			}
			w.expr = append(w.expr[:n], keyExpr...)
			w.walk(it.Key(), level+1)
			w.expr = append(w.expr[:n], valExpr...)
			w.walk(it.Value(), level+1)
		}
	case reflect.Slice, reflect.Array:
		for i := range min(v.Len(), w.maxElems) {
			w.expr = append(strconv.AppendInt(append(w.expr[:n], '['), int64(i), 10), ']')
			w.walk(v.Index(i), level+1)
		}
	}
}

func (w *walker) report(val, needle string) {
	r := []string{needle}
	if !w.exact {
		r = append(r, escape(val))
	}
	r = append(r, w.fn, string(w.expr))
	for _, s := range w.path {
		r = append(r, s.String())
	}
	w.rows = append(w.rows, r)
}

// keyLiteral returns a map key as a Go literal, e.g. "config.file" or 42,
// if it's a string, integer or bool.
func keyLiteral(k reflect.Value) (string, bool) {
	switch k.Kind() {
	case reflect.String:
		return strconv.Quote(k.String()), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(k.Int(), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.FormatUint(k.Uint(), 10), true
	case reflect.Bool:
		return strconv.FormatBool(k.Bool()), true
	}
	return "", false
}

// escape returns s with non-printable characters, invalid UTF-8 and
// backslashes escaped as in a Go string literal (e.g. \x00, \n, \\), so
// that a haystack from a binary buffer stays readable text. Printable
// characters, including non-ASCII ones and quotes, are left as is.
func escape(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		switch {
		case r == '"':
			b.WriteByte('"') // CSV quoting takes care of it
		case r == utf8.RuneError && size == 1, r == '\\', !strconv.IsPrint(r):
			q := strconv.Quote(s[:size])
			b.WriteString(q[1 : len(q)-1])
		default:
			b.WriteString(s[:size])
		}
		s = s[size:]
	}
	return b.String()
}

// stringValue returns the contents of a string, or of a byte slice or
// array. For byte slices the result shares memory with the slice, so it's
// only valid while the value is being checked.
func stringValue(v reflect.Value) (string, bool) {
	switch v.Kind() {
	case reflect.String:
		return v.String(), true
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b := v.Bytes()
			return unsafe.String(unsafe.SliceData(b), len(b)), true
		}
	case reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			if v.CanAddr() {
				b := v.Bytes()
				return unsafe.String(unsafe.SliceData(b), len(b)), true
			}
			// e.g. a map value
			b := make([]byte, v.Len())
			for i := range b {
				b[i] = byte(v.Index(i).Uint())
			}
			return string(b), true
		}
	}
	return "", false
}

// isNamed reports whether t is a defined type other than a predeclared one,
// e.g. main.ID or net/http.Header, as opposed to string, error or a type
// literal such as []string or *main.Outer.
func isNamed(t reflect.Type) bool {
	return t.Name() != "" && t.PkgPath() != ""
}

// typeName returns t's name with full package paths, e.g. net/http.Header
// or *main.Outer, as DWARF (and so delve) names it.
func typeName(t reflect.Type) string {
	if t.Name() != "" {
		if t.PkgPath() == "" {
			return t.Name() // predeclared
		}
		// Type arguments of generic types are already fully qualified.
		return t.PkgPath() + "." + t.Name()
	}
	switch t.Kind() {
	case reflect.Pointer:
		return "*" + typeName(t.Elem())
	case reflect.Slice:
		return "[]" + typeName(t.Elem())
	case reflect.Array:
		return "[" + strconv.Itoa(t.Len()) + "]" + typeName(t.Elem())
	case reflect.Map:
		return "map[" + typeName(t.Key()) + "]" + typeName(t.Elem())
	case reflect.Chan:
		switch t.ChanDir() {
		case reflect.RecvDir:
			return "<-chan " + typeName(t.Elem())
		case reflect.SendDir:
			return "chan<- " + typeName(t.Elem())
		}
		return "chan " + typeName(t.Elem())
	}
	return t.String() // func, struct and interface literals
}

func goid() int64 {
	var buf [64]byte
	b := bytes.TrimPrefix(buf[:runtime.Stack(buf[:], false)], []byte("goroutine "))
	var id int64
	for _, c := range b {
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + int64(c-'0')
	}
	return id
}
