// Package rt is linked into the program being built by argmatch. The
// toolexec wrapper injects a call to Check at the top of each target
// function. Check walks the function's receiver and arguments without
// touching the program's data directly: it reads every value's bytes
// through /proc/self/mem, so a bad pointer or a value that's being written
// gives a read error or wrong bytes, not a crash, and decodes them using
// reflect only for type layout (sizes, field offsets, element types), which
// comes from the binary's read-only type descriptors. Maps are decoded from
// the runtime's map layout (see walkMap).
package rt

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/tools/gopls/conftamer/stacklog"

	"argmatch/settings"
)

// encoded is the program's run-time settings (settings.Settings.Encode),
// set by the wrapper with the linker's -X flag, so that changing them
// doesn't change the compiled packages.
var encoded string

var (
	// current is the settings in use, loaded by the first check (see
	// loadSettings); nil until then.
	current    atomic.Pointer[settings.Settings]
	settingsMu sync.Mutex

	outOnce sync.Once
	outMu   sync.Mutex
	out     io.Writer = os.Stderr

	active sync.Map // IDs of goroutines running Check, to detect re-entry

	memOnce sync.Once
	memErr  error
	selfMem *os.File
	// typesLo and typesHi bound the executable's read-only mappings, where
	// the type descriptors are. A type pointer read from the program's
	// memory is only used if it's in this range.
	typesLo, typesHi uintptr

	ignored sync.Map // reflect.Type -> bool: whether its fields are skipped (see skipFields)

	registerMu sync.Mutex
	registered []string // the instrumented functions (see Register)
)

// Register records fns as instrumented functions. The wrapper makes each
// instrumented file call it from an init function, so that by the time the
// program's main runs, rt knows every instrumented function, including
// those never called. With Stats, the first check prints them. It may run
// before rt's own initialization, so it uses only zero-valued variables.
//
//go:linkname Register
func Register(fns []string) {
	registerMu.Lock()
	registered = append(registered, fns...)
	registerMu.Unlock()
}

// Check reports every string in args that equals one of the needles (or,
// if Exact is false, contains one), along with the named types and
// interfaces on the path down to it, as CSV rows (see header). The
// settings (settings.Settings) are loaded by the first call; see
// loadSettings. names[i] is the source name of the parameter whose address
// is boxed in args[i]. Output goes to the file Out, which the first call
// overwrites, or to stderr if Out is "". Errors always go to stderr.
//
// Values are walked Depth levels deep (see walker.walk), and only the first
// MaxElems elements of each array, slice and map and the first MaxFields
// fields of each struct (-1 for all). The fields of the struct types that
// IgnoreLibs and IgnoreTypes select aren't walked (see skipFields). The
// variables captured by closures aren't walked. Walking stops once a check
// has taken MaxTime or the Go heap has grown by MaxMem bytes (0 for no
// limit), and the matches found by then are reported.
//
// calls counts the calls of fn so far (each instrumented function has its
// own counter). Once it reaches MaxCalls, Check returns at once, so later
// calls cost almost nothing. MaxCalls 0 means no limit.
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
func Check(calls *uint32, guard bool, fn string, names []string, args []any) {
	s := current.Load()
	if s != nil && !admit(calls, s) {
		return
	}
	if guard || s == nil {
		// Loading the settings calls the standard library too, so mark the
		// goroutine for that as well.
		gid := goid()
		if _, busy := active.LoadOrStore(gid, true); busy {
			return
		}
		defer active.Delete(gid)
	}
	if s == nil {
		settingsMu.Lock()
		if s = current.Load(); s == nil {
			s = loadSettings()
			if s.Stats {
				printRegistered()
			}
			current.Store(s)
		}
		settingsMu.Unlock()
		if !admit(calls, s) {
			return
		}
	}

	outOnce.Do(func() {
		if s.Out != "" {
			// Overwrite any output from an earlier run.
			f, err := os.OpenFile(s.Out, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				fmt.Fprintf(os.Stderr, "argmatch: %v; writing to stderr instead\n", err)
			} else {
				out = f
			}
		}
		outMu.Lock()
		writeCSV(out, [][]string{header(s)})
		outMu.Unlock()
	})
	memOnce.Do(memInit)
	if memErr != nil {
		fmt.Fprintf(os.Stderr, "argmatch: %v\n", memErr)
		return
	}

	w := newWalker(s, fn)
	reported := false // whether running out of budget was reported
	for i, arg := range args {
		if w.exceeded != "" {
			fmt.Fprintf(os.Stderr, "argmatch: %s: %s; reporting the matches found, which don't include %s\n",
				fn, w.exceeded, strings.Join(names[i:], ", "))
			reported = true
			break
		}
		// arg holds *T, where T is the parameter's static type, so that an
		// interface parameter stays an interface. arg is our own value, so
		// reading its pointer directly is safe; T's bytes are read raw.
		p := reflect.ValueOf(arg)
		if p.Kind() != reflect.Pointer || p.IsNil() {
			continue
		}
		t := p.Type().Elem()
		w.expr = append(w.expr[:0], names[i]...)
		w.walk(t, w.read(p.Pointer(), t.Size()), 0)
	}
	if len(w.rows) > 0 {
		// One write per call, so that calls' rows aren't interleaved.
		outMu.Lock()
		writeCSV(out, w.rows)
		outMu.Unlock()
	}
	if w.exceeded != "" && !reported {
		fmt.Fprintf(os.Stderr, "argmatch: %s: %s; reporting the matches found\n", fn, w.exceeded)
	}
	if s.Stats {
		fmt.Fprintf(os.Stderr, "argmatch-stats: %s %v reads=%d bytes=%d readErrs=%d maps=%d badMaps=%d badTypes=%d rows=%d\n",
			fn, time.Since(w.started).Round(time.Microsecond), w.reads, w.bytes, w.readErrs, w.maps, w.badMaps, w.badTypes, len(w.rows))
	}
}

// printRegistered prints the instrumented functions registered so far to
// stderr, one "argmatch-instrumented:" line each, preceded by their number.
func printRegistered() {
	registerMu.Lock()
	defer registerMu.Unlock()
	fmt.Fprintf(os.Stderr, "argmatch-instrumented: %d functions\n", len(registered))
	for _, fn := range registered {
		fmt.Fprintf(os.Stderr, "argmatch-instrumented: %s\n", fn)
	}
}

// admit counts a call in calls and reports whether it should be checked,
// i.e. whether fewer than s.MaxCalls calls have been (0 for no limit).
func admit(calls *uint32, s *settings.Settings) bool {
	if s.MaxCalls <= 0 {
		return true
	}
	// Load first, so the counter stops growing (and can't wrap) once the
	// limit is reached. Concurrent first calls may overshoot the limit
	// slightly. Calls of the function made by Check itself (if it's in the
	// standard library) count too.
	if atomic.LoadUint32(calls) >= uint32(s.MaxCalls) {
		return false
	}
	atomic.AddUint32(calls, 1)
	return true
}

// loadSettings returns the settings the wrapper linked in (encoded),
// overridden by any ARGMATCH_* environment variables (see
// settings.Settings.FromEnv), and says on stderr which variables it used
// and any it couldn't.
func loadSettings() *settings.Settings {
	s := settings.Default()
	if encoded != "" {
		var err error
		if s, err = settings.Decode(encoded); err != nil {
			fmt.Fprintf(os.Stderr, "argmatch: decoding the linked-in settings: %v\n", err)
			s = settings.Default()
		}
	}
	used, err := s.FromEnv()
	if len(used) > 0 {
		fmt.Fprintf(os.Stderr, "argmatch: settings from the environment: %s\n", strings.Join(used, ", "))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "argmatch: ignoring %v\n", err)
	}
	if !s.IgnoreClosures {
		fmt.Fprintln(os.Stderr, "argmatch: ignoring ARGMATCH_IGNORECLOSURES=false: closures' captured variables can't be checked")
		s.IgnoreClosures = true
	}
	return &s
}

// memInit opens /proc/self/mem and finds the executable's read-only
// mappings in /proc/self/maps.
func memInit() {
	selfMem, memErr = os.Open("/proc/self/mem")
	if memErr != nil {
		return
	}
	exe, err := os.Readlink("/proc/self/exe")
	if err != nil {
		memErr = err
		return
	}
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		memErr = err
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
		memErr = fmt.Errorf("no read-only mapping of %s in /proc/self/maps", exe)
	}
}

func writeCSV(out io.Writer, rows [][]string) {
	var buf bytes.Buffer
	c := csv.NewWriter(&buf)
	c.WriteAll(rows)
	out.Write(buf.Bytes())
}

// header returns the names of the output's columns. Each row has one more
// column per step of the path from the argument down to the match: a named
// type, or an interface ("concrete (implements interface)"). Rows have
// different lengths, so the last header names the first of them. With exact
// matching the haystack column is left out, since it would always equal the
// needle.
func header(s *settings.Settings) []string {
	h := []string{"needle"}
	if !s.Exact {
		h = append(h, "haystack")
	}
	return append(h, "function", "expression", "types")
}

const (
	ptrSize = 8
	// maxStringLen bounds how much of a string or byte slice is read, so a
	// garbage length from a torn read can't make us read gigabytes.
	maxStringLen = 1 << 20
	// maxSliceBytes bounds one read of a slice's elements, for the same
	// reason.
	maxSliceBytes = 64 << 20
	// heapCheckReads is how often (in reads) the heap size is checked for
	// MaxMem. Reading it costs about as much as half a read of
	// /proc/self/mem; the clock is checked on every read.
	heapCheckReads = 16
)

// A walker walks one call's arguments, reading their bytes through
// /proc/self/mem. expr and path are stacks: each level of the walk appends
// to them, and truncates them back when done.
type walker struct {
	*settings.Settings
	fn string

	expr []byte // source expression of the current value, e.g. o.P.Name
	path []step // named types and interfaces on the path to it
	rows [][]string

	// The budget (MaxTime, MaxMem). Once exceeded is set, every read fails.
	started  time.Time
	deadline time.Time // zero for no limit
	baseHeap int64
	exceeded string // why the budget ran out, or "" if it hasn't

	reads, bytes, readErrs, maps, badMaps, badTypes int
}

func newWalker(s *settings.Settings, fn string) *walker {
	w := &walker{Settings: s, fn: fn, started: time.Now()}
	if s.MaxTime > 0 {
		w.deadline = w.started.Add(s.MaxTime)
	}
	if s.MaxMem > 0 {
		w.baseHeap = heapBytes()
	}
	return w
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

// heapSample is reused to read the heap size.
var heapSample = []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}

// heapBytes returns the bytes of heap objects: live ones, and dead ones the
// garbage collector hasn't freed yet. It covers the whole program, not just
// this check.
func heapBytes() int64 {
	metrics.Read(heapSample)
	if heapSample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return int64(heapSample[0].Value.Uint64())
}

// read returns size bytes at addr, or nil if they can't be read or the
// budget has run out.
func (w *walker) read(addr uintptr, size uintptr) []byte {
	if w.exceeded != "" {
		return nil
	}
	if !w.deadline.IsZero() {
		if now := time.Now(); now.After(w.deadline) {
			w.exceeded = fmt.Sprintf("stopped at -maxtime after %v", now.Sub(w.started).Round(time.Millisecond))
			return nil
		}
	}
	if w.MaxMem > 0 && w.reads%heapCheckReads == 0 {
		// The heap's growth covers the whole program, and only changes a
		// span at a time; the bytes this check has read are what it has
		// allocated for values, so count whichever is larger.
		if grown := max(heapBytes()-w.baseHeap, int64(w.bytes)); grown > w.MaxMem {
			w.exceeded = fmt.Sprintf("stopped at -maxmem after using %d MB in %v",
				grown/1e6, time.Since(w.started).Round(time.Millisecond))
			return nil
		}
	}
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
// executable's read-only mappings and aligned. That rules out most garbage
// pointers (e.g. from a freed and reused object), but not all: a garbage
// pointer that happens to point into read-only data that isn't a type
// descriptor can make reflect read nonsense, and crash the program with a
// fault or a runtime throw. That's the one way left for this walker to
// crash the program; it hasn't been seen.
func (w *walker) typeAt(p uintptr) (reflect.Type, bool) {
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

// walk checks the value of type t whose bytes are b (nil if unreadable)
// and what it contains, adding t to the path if it's a named type or an
// interface. level counts nesting the way delve does when loading a
// variable with MaxVariableRecurse = Depth, which is what -depth meant when
// argmatch used delve:
//   - a struct's fields and a map's keys and values are walked only while
//     level <= Depth, and are one level deeper;
//   - an array's or slice's elements are always walked, one level deeper;
//   - a pointer's target is at the same level, unless it's a pointer or an
//     interface, which are one level deeper (and a pointer target is only
//     walked while level <= Depth);
//   - an interface's dynamic value is at the same level, walked only while
//     level <= Depth;
//   - strings are always checked.
func (w *walker) walk(t reflect.Type, b []byte, level int) {
	if b == nil {
		return
	}
	n := len(w.path)
	defer func() { w.path = w.path[:n] }()
	switch {
	case t.Kind() == reflect.Interface:
		tab, data := word(b), word(b[ptrSize:])
		if tab == 0 || level > w.Depth {
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
		// Report the interface together with its dynamic type, then
		// continue below the dynamic value so it isn't reported twice.
		w.path = append(w.path, step{t, dt})
		w.walkValue(dt, val, level)
		return
	case isNamed(t):
		w.path = append(w.path, step{typ: t})
	}
	w.walkValue(t, b, level)
}

// walkValue checks the value of type t whose bytes are b, whose type is
// already accounted for in the path, then walks its contents.
func (w *walker) walkValue(t reflect.Type, b []byte, level int) {
	if b == nil {
		return
	}
	if s, ok := w.stringValue(t, b); ok {
		if len(w.path) > 0 {
			for _, needle := range w.Needles {
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
			if level <= w.Depth {
				w.walk(e, w.read(p, e.Size()), level+1)
			}
		default:
			w.walk(e, w.read(p, e.Size()), level)
		}
	case reflect.Struct:
		if level > w.Depth || w.skipFields(t) {
			return
		}
		for i := range t.NumField() {
			if w.MaxFields >= 0 && i >= w.MaxFields {
				break
			}
			f := t.Field(i)
			w.expr = append(append(w.expr[:n], '.'), f.Name...)
			w.walk(f.Type, b[f.Offset:f.Offset+f.Type.Size()], level+1)
		}
	case reflect.Map:
		if level <= w.Depth && word(b) != 0 {
			w.walkMap(t, word(b), level)
		}
	case reflect.Array:
		e := t.Elem()
		for i := range min(t.Len(), w.MaxElems) {
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
		count := min(length, w.MaxElems, int(maxSliceBytes/e.Size()))
		if count <= 0 {
			return
		}
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

// matches reports whether s matches needle.
func (w *walker) matches(s, needle string) bool {
	if w.Exact {
		return s == needle
	}
	return strings.Contains(s, needle)
}

func (w *walker) report(val, needle string) {
	r := []string{needle}
	if !w.Exact {
		r = append(r, escape(val))
	}
	r = append(r, w.fn, string(w.expr))
	for _, s := range w.path {
		r = append(r, s.String())
	}
	w.rows = append(w.rows, r)
}

// skipFields reports whether the fields of struct type t aren't walked:
// with IgnoreLibs, if stacklog.IgnoreFn would ignore a function of the same
// name (it's in the standard library or one of stacklog.IGNORE_LIBS), and
// if it's in IgnoreTypes, each a full type name or its end after a slash
// (config.Config for github.com/prometheus/prometheus/config.Config).
func (w *walker) skipFields(t reflect.Type) bool {
	if !isNamed(t) || !w.IgnoreLibs && len(w.IgnoreTypes) == 0 {
		return false
	}
	if skip, ok := ignored.Load(t); ok {
		return skip.(bool)
	}
	name := typeName(t)
	// A generic type's arguments may contain slashes and dots too.
	base, _, _ := strings.Cut(name, "[")
	skip := w.IgnoreLibs && stacklog.IgnoreFn(base, isStdPkg)
	for _, it := range w.IgnoreTypes {
		skip = skip || name == it || base == it || strings.HasSuffix(name, "/"+it) || strings.HasSuffix(base, "/"+it)
	}
	ignored.Store(t, skip)
	return skip
}

// isStdPkg reports whether pkg is in the standard library, using the go
// command's rule: the first element of the path has no dot. main is the
// exception.
func isStdPkg(pkg string) bool {
	first, _, _ := strings.Cut(pkg, "/")
	return pkg != "main" && !strings.Contains(first, ".")
}

// mapType mirrors internal/abi.MapType, the type descriptor of a map type
// (Go 1.24 and later), which is in the binary's read-only data.
type mapType struct {
	typ                          [48]byte // abi.Type
	key, elem, group, hasher     uintptr
	groupSize, slotSize, elemOff uintptr
	flags                        uint32
}

// Map layout constants from internal/abi and internal/runtime/maps.
const (
	mapIndirectKey  = 1 << 2 // abi.MapIndirectKey: slots hold *key
	mapIndirectElem = 1 << 3 // abi.MapIndirectElem: slots hold *elem
	groupSlots      = 8      // abi.MapGroupSlots
	slotsOffset     = 8      // maps.groupSlotsOffset: slots follow the 8 control bytes
	ctrlEmpty       = 0x80   // a control byte with the top bit set is an empty or deleted slot
	mapHeaderSize   = 32     // maps.Map up to and including dirLen
	tableSize       = 32     // maps.table up to and including groups
	maxDirLen       = 1 << 16
	maxGroups       = 1 << 20
)

// typePtr returns the address of t's type descriptor.
func typePtr(t reflect.Type) uintptr { return (*[2]uintptr)(unsafe.Pointer(&t))[1] }

// mapTypeOf returns the descriptor of map type t, if its layout is the one
// mapType mirrors: its key and elem must be t's.
func mapTypeOf(t reflect.Type) (*mapType, bool) {
	mt := (*mapType)(unsafe.Pointer(typePtr(t)))
	ok := mt.key == typePtr(t.Key()) && mt.elem == typePtr(t.Elem()) &&
		mt.groupSize == slotsOffset+groupSlots*mt.slotSize && mt.elemOff < mt.slotSize
	return mt, ok
}

// walkMap walks the first MaxElems entries of the map of type t whose
// runtime header (internal/runtime/maps.Map) is at addr, in memory order,
// as delve does: a small map (dirLen 0) is one group of 8 slots at dirPtr;
// otherwise dirPtr is an array of dirLen table pointers (several entries
// may share a table), and each table has lengthMask+1 groups. A slot is
// full if its control byte's top bit is clear. A map whose type descriptor
// doesn't have the expected layout (another Go version) is skipped and
// counted in badMaps.
func (w *walker) walkMap(t reflect.Type, addr uintptr, level int) {
	mt, ok := mapTypeOf(t)
	if !ok {
		w.badMaps++
		return
	}
	hdr := w.read(addr, mapHeaderSize)
	if hdr == nil {
		return
	}
	w.maps++
	dirPtr, dirLen := word(hdr[16:]), int(word(hdr[24:]))
	entries := 0
	group := func(g uintptr) bool { // walks group g's full slots; false once MaxElems are done
		b := w.read(g, mt.groupSize)
		if b == nil {
			return w.exceeded == ""
		}
		for i := range uintptr(groupSlots) {
			if b[i]&ctrlEmpty != 0 {
				continue
			}
			if entries >= w.MaxElems {
				return false
			}
			slot := b[slotsOffset+i*mt.slotSize:][:mt.slotSize]
			w.entry(t, mt, slot, entries, level)
			entries++
		}
		return entries < w.MaxElems
	}
	if dirLen == 0 {
		if dirPtr != 0 {
			group(dirPtr)
		}
		return
	}
	if dirLen < 0 || dirLen > maxDirLen {
		w.badMaps++
		return
	}
	dir := w.read(dirPtr, uintptr(dirLen)*ptrSize)
	if dir == nil {
		return
	}
	seen := map[uintptr]bool{}
	for i := range dirLen {
		tab := word(dir[i*ptrSize:])
		if tab == 0 || seen[tab] {
			continue
		}
		seen[tab] = true
		tb := w.read(tab, tableSize)
		if tb == nil {
			continue
		}
		groups, n := word(tb[16:]), word(tb[24:])+1
		if n > maxGroups {
			w.badMaps++
			continue
		}
		for g := range n {
			if !group(groups + g*mt.groupSize) {
				return
			}
		}
	}
}

// entry walks the key and value in a map slot, the i'th entry walked.
func (w *walker) entry(t reflect.Type, mt *mapType, slot []byte, i, level int) {
	kt, et := t.Key(), t.Elem()
	key := slot[:min(kt.Size(), uintptr(len(slot)))]
	if mt.flags&mapIndirectKey != 0 {
		key = w.read(word(slot), kt.Size())
	}
	elem := slot[mt.elemOff:][:min(et.Size(), mt.slotSize-mt.elemOff)]
	if mt.flags&mapIndirectElem != 0 {
		elem = w.read(word(slot[mt.elemOff:]), et.Size())
	}
	if key == nil || uintptr(len(key)) != kt.Size() || elem == nil || uintptr(len(elem)) != et.Size() {
		return
	}
	// Name the entry by its key if it's a basic value, otherwise by its
	// position in memory order.
	n := len(w.expr)
	keyExpr, valExpr := "<key "+strconv.Itoa(i)+">", ""
	if lit, ok := w.keyLiteral(kt, key); ok {
		keyExpr, valExpr = "<key "+lit+">", "["+lit+"]"
	} else {
		valExpr = "[" + keyExpr + "]"
	}
	w.expr = append(w.expr[:n], keyExpr...)
	w.walk(kt, key, level+1)
	w.expr = append(w.expr[:n], valExpr...)
	w.walk(et, elem, level+1)
	w.expr = w.expr[:n]
}

// keyLiteral returns a map key of type t whose bytes are b as a Go literal,
// e.g. "config.file" or 42, if it's a string, integer or bool.
func (w *walker) keyLiteral(t reflect.Type, b []byte) (string, bool) {
	switch t.Kind() {
	case reflect.String:
		s, ok := w.stringValue(t, b)
		return strconv.Quote(s), ok
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(signExtend(b), 10), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		var u uint64
		for i := len(b) - 1; i >= 0; i-- {
			u = u<<8 | uint64(b[i])
		}
		return strconv.FormatUint(u, 10), true
	case reflect.Bool:
		return strconv.FormatBool(b[0] != 0), true
	}
	return "", false
}

// signExtend returns the little-endian signed integer in b.
func signExtend(b []byte) int64 {
	var u uint64
	for i := len(b) - 1; i >= 0; i-- {
		u = u<<8 | uint64(b[i])
	}
	shift := 64 - 8*uint(len(b))
	return int64(u<<shift) >> shift
}

// stringValue returns the contents of a string, byte slice or byte array
// of type t whose bytes are b, reading them if needed (up to maxStringLen).
func (w *walker) stringValue(t reflect.Type, b []byte) (string, bool) {
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
