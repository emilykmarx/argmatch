package rt

// PROTOTYPE: a walker that never touches the program's data directly. It
// reads every value's bytes through /proc/self/mem, so a bad pointer or a
// value that's being written gives a read error or wrong bytes, never a
// crash, and decodes them using reflect only for type layout (sizes, field
// offsets, element types), which comes from the binary's read-only type
// descriptors. Maps aren't walked yet: they'd need a decoder for the
// runtime's map layout (e.g. adapted from delve's pkg/proc/mapiter.go).

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	ptrSize = 8
	// maxStringLen bounds how much of a string or byte slice is read, so a
	// garbage length from a torn read can't make us read gigabytes.
	maxStringLen = 1 << 20
	// maxSliceBytes bounds one read of a slice's elements, for the same
	// reason.
	maxSliceBytes = 64 << 20
)

var (
	rawOnce sync.Once
	rawErr  error
	selfMem *os.File
	// typesLo and typesHi bound the executable's read-only mapping, where
	// the type descriptors are. A type pointer read from the program's
	// memory is only trusted if it's in this range.
	typesLo, typesHi uintptr
)

func rawInit() {
	selfMem, rawErr = os.Open("/proc/self/mem")
	if rawErr != nil {
		return
	}
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		rawErr = err
		return
	}
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		rawErr = err
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// e.g. "00400000-01a3e000 r--p 00000000 08:02 1234 /bin/prometheus"
		fields := strings.Fields(sc.Text())
		if len(fields) < 6 || fields[5] != exe || !strings.HasPrefix(fields[1], "r") || fields[1][1] == 'w' {
			continue
		}
		lo, hi, _ := strings.Cut(fields[0], "-")
		l, err1 := strconv.ParseUint(lo, 16, 64)
		h, err2 := strconv.ParseUint(hi, 16, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		if typesLo == 0 || uintptr(l) < typesLo {
			typesLo = uintptr(l)
		}
		if uintptr(h) > typesHi {
			typesHi = uintptr(h)
		}
	}
	if typesHi == 0 {
		rawErr = fmt.Errorf("no read-only mapping of %s in /proc/self/maps", exe)
	}
}

// rawWalker is a walker whose values are bytes read from /proc/self/mem.
type rawWalker struct {
	*walker
	reads, bytes, readErrs, maps, badTypes int
}

// read returns size bytes at addr, or nil if they can't be read.
func (w *rawWalker) read(addr uintptr, size uintptr) []byte {
	if size == 0 {
		return []byte{}
	}
	b := make([]byte, size)
	w.reads++
	n, err := selfMem.ReadAt(b, int64(addr))
	w.bytes += n
	if err != nil || uintptr(n) != size {
		w.readErrs++
		return nil
	}
	return b
}

func word(b []byte) uintptr { return uintptr(binary.LittleEndian.Uint64(b)) }

// typeAt returns the type whose descriptor is at p, if p is in the
// executable's read-only mapping.
func (w *rawWalker) typeAt(p uintptr) (reflect.Type, bool) {
	if p < typesLo || p >= typesHi || p%ptrSize != 0 {
		w.badTypes++
		return nil, false
	}
	var e any
	(*[2]uintptr)(unsafe.Pointer(&e))[0] = p
	return reflect.TypeOf(e), true
}

// isDirectIface reports whether an interface holding a t holds the value
// itself in its data word rather than a pointer to it: pointer-shaped types,
// as the compiler decides (cmd/compile/internal/types.IsDirectIface).
func isDirectIface(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.UnsafePointer, reflect.Map, reflect.Chan, reflect.Func:
		return true
	case reflect.Array:
		return t.Len() == 1 && isDirectIface(t.Elem())
	case reflect.Struct:
		return t.NumField() == 1 && isDirectIface(t.Field(0).Type)
	}
	return false
}

// walk is walker.walk for a value of type t whose bytes are b.
func (w *rawWalker) walk(t reflect.Type, b []byte, level int) {
	if b == nil {
		return
	}
	n := len(w.path)
	defer func() { w.path = w.path[:n] }()
	switch {
	case t.Kind() == reflect.Interface:
		tab, data := word(b), word(b[ptrSize:])
		if tab == 0 || level > w.depth {
			return
		}
		typ := tab
		if t.NumMethod() > 0 {
			// tab is an *abi.ITab: Inter, then Type.
			itab := w.read(tab+ptrSize, ptrSize)
			if itab == nil {
				return
			}
			typ = word(itab)
		}
		dt, ok := w.typeAt(typ)
		if !ok {
			return
		}
		var val []byte
		if isDirectIface(dt) {
			val = b[ptrSize : 2*ptrSize]
		} else {
			val = w.read(data, dt.Size())
		}
		w.path = append(w.path, step{t, dt})
		w.walkValue(dt, val, level)
		return
	case isNamed(t):
		w.path = append(w.path, step{typ: t})
	}
	w.walkValue(t, b, level)
}

// walkValue is walker.walkValue for a value of type t whose bytes are b.
func (w *rawWalker) walkValue(t reflect.Type, b []byte, level int) {
	if b == nil {
		return
	}
	if s, ok := w.stringValue(t, b); ok {
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
	switch t.Kind() {
	case reflect.Pointer:
		p := word(b)
		if p == 0 {
			return
		}
		e := t.Elem()
		switch e.Kind() {
		case reflect.Interface:
			w.walk(e, w.read(p, e.Size()), level+1)
		case reflect.Pointer:
			if level <= w.depth {
				w.walk(e, w.read(p, e.Size()), level+1)
			}
		default:
			w.walk(e, w.read(p, e.Size()), level)
		}
	case reflect.Struct:
		if level > w.depth {
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			w.expr = append(append(w.expr[:n], '.'), f.Name...)
			w.walk(f.Type, b[f.Offset:f.Offset+f.Type.Size()], level+1)
		}
	case reflect.Map:
		if level <= w.depth && word(b) != 0 {
			w.maps++ // not walked yet
		}
	case reflect.Array:
		e := t.Elem()
		for i := range min(t.Len(), w.maxElems) {
			off := uintptr(i) * e.Size()
			w.expr = append(strconv.AppendInt(append(w.expr[:n], '['), int64(i), 10), ']')
			w.walk(e, b[off:off+e.Size()], level+1)
		}
	case reflect.Slice:
		e := t.Elem()
		p, length := word(b), int(word(b[ptrSize:]))
		if p == 0 || length <= 0 || e.Size() == 0 {
			return
		}
		count := min(length, w.maxElems, int(maxSliceBytes/e.Size()))
		elems := w.read(p, uintptr(count)*e.Size())
		if elems == nil {
			return
		}
		for i := range count {
			off := uintptr(i) * e.Size()
			w.expr = append(strconv.AppendInt(append(w.expr[:n], '['), int64(i), 10), ']')
			w.walk(e, elems[off:off+e.Size()], level+1)
		}
	}
}

// stringValue returns the contents of a string, byte slice or byte array
// of type t whose bytes are b, reading them if needed.
func (w *rawWalker) stringValue(t reflect.Type, b []byte) (string, bool) {
	switch t.Kind() {
	case reflect.String, reflect.Slice:
		if t.Kind() == reflect.Slice && t.Elem().Kind() != reflect.Uint8 {
			return "", false
		}
		p, length := word(b), int(word(b[ptrSize:]))
		if p == 0 || length <= 0 {
			return "", true
		}
		data := w.read(p, uintptr(min(length, maxStringLen)))
		if data == nil {
			return "", false
		}
		return string(data), true
	case reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return string(b), true
		}
	}
	return "", false
}

// walkRaw walks args like Check's reflect walk, but through /proc/self/mem,
// and reports what it read on stderr.
func walkRaw(w *walker, names []string, args []any) {
	rawOnce.Do(rawInit)
	if rawErr != nil {
		fmt.Fprintf(os.Stderr, "argmatch: %v\n", rawErr)
		return
	}
	start := time.Now()
	r := &rawWalker{walker: w}
	for i, arg := range args {
		// arg holds *T; arg itself is our own value, so reading its
		// pointer directly is safe. T's bytes are read raw.
		p := reflect.ValueOf(arg)
		if p.Kind() != reflect.Pointer || p.IsNil() {
			continue
		}
		t := p.Type().Elem()
		w.expr = append(w.expr[:0], names[i]...)
		r.walk(t, r.read(p.Pointer(), t.Size()), 0)
	}
	fmt.Fprintf(os.Stderr, "argmatch-stats: %s raw %v reads=%d bytes=%d readErrs=%d mapsSkipped=%d badTypes=%d rows=%d\n",
		w.fn, time.Since(start).Round(time.Microsecond), r.reads, r.bytes, r.readErrs, r.maps, r.badTypes, len(w.rows))
}
