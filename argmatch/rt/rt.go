// Package rt is linked into the program being built by argmatch. The
// toolexec wrapper injects a call to Check at the top of each target
// function; Check walks the function's receiver and arguments with reflect.
package rt

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

// maxElems is how many elements of each array, slice or map are checked.
const maxElems = 1 << 12

var (
	outOnce sync.Once
	outMu   sync.Mutex
	out     io.Writer = os.Stderr

	active sync.Map // IDs of goroutines running Check, to detect re-entry
)

// Check reports every string in args that contains one of needles, along
// with the named types and interfaces on the path down to it. names[i] is the
// source name of the parameter whose address is boxed in args[i]. Output
// is appended to the file outPath, or written to stderr if outPath is "".
//
// If guard is set, the target function may be called by Check itself, so
// calls made while checking are ignored. That's only possible for functions
// in the standard library, where rt's dependencies are; finding the
// goroutine ID for the guard takes most of the time of a Check.
//
// The push linkname lets the injected bodyless declaration in the target
// package (which can't import rt) refer to this function.
//
//go:linkname Check
func Check(guard bool, needles []string, depth int, outPath string, fn string, names []string, args []any) {
	if guard {
		gid := goid()
		if _, busy := active.LoadOrStore(gid, true); busy {
			return
		}
		defer active.Delete(gid)
	}

	w := walker{needles: needles, depth: depth, fn: fn}
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
	if w.out.Len() == 0 {
		return
	}
	outOnce.Do(func() {
		if outPath == "" {
			return
		}
		f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "argmatch: %v; writing to stderr instead\n", err)
			return
		}
		out = f
	})
	// One write per call, so that calls' lines aren't interleaved.
	outMu.Lock()
	out.Write(w.out.Bytes())
	outMu.Unlock()
}

// A walker walks one argument at a time. expr and path are stacks: each
// level of the walk appends to them, and truncates them back when done.
type walker struct {
	needles []string
	depth   int
	fn      string

	expr []byte // source expression of the current value, e.g. o.P.Name
	path []step // named types and interfaces on the path to it
	out  bytes.Buffer
}

// A step is a named type on the path to a match, or an interface (iface)
// with its concrete type.
type step struct {
	typ, concrete reflect.Type
}

func (s step) String() string {
	if s.concrete != nil {
		return typeName(s.typ) + " (concrete " + typeName(s.concrete) + ")"
	}
	return typeName(s.typ)
}

// walk checks v and what it contains, adding v's type to the path if it's
// a named type or an interface. level counts nesting the way delve does
// when loading a variable with MaxVariableRecurse = depth, so -depth means
// what it did when argmatch used delve:
//   - a struct's fields and a map's keys and values are loaded only while
//     level <= depth, and are one level deeper;
//   - an array's or slice's elements are always loaded, one level deeper;
//   - a pointer's target is at the same level, unless it's a pointer or an
//     interface, which are one level deeper (and a pointer target is only
//     loaded while level <= depth);
//   - an interface's dynamic value is at the same level, loaded only while
//     level <= depth;
//   - strings are always loaded.
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
				if strings.Contains(s, needle) {
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
		for i := 0; i < maxElems && it.Next(); i++ {
			w.expr = append(strconv.AppendInt(append(w.expr[:n], "<key "...), int64(i), 10), '>')
			w.walk(it.Key(), level+1)
			w.expr = append(strconv.AppendInt(append(w.expr[:n], "[<key "...), int64(i), 10), ">]"...)
			w.walk(it.Value(), level+1)
		}
	case reflect.Slice, reflect.Array:
		for i := range min(v.Len(), maxElems) {
			w.expr = append(strconv.AppendInt(append(w.expr[:n], '['), int64(i), 10), ']')
			w.walk(v.Index(i), level+1)
		}
	}
}

func (w *walker) report(val, needle string) {
	path := make([]string, len(w.path))
	for i, s := range w.path {
		path[i] = s.String()
	}
	fmt.Fprintf(&w.out, "argmatch: value %q contains %q in %s: %s (%s)\n", val, needle, w.fn, strings.Join(path, " -> "), w.expr)
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
