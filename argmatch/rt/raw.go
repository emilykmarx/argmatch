package rt

// PROTOTYPE: a walker that never touches the program's data directly. It
// reads every value's bytes through /proc/self/mem, so a bad pointer or a
// value that's being written gives a read error or wrong bytes, never a
// crash, and decodes them using reflect only for type layout (sizes, field
// offsets, element types), which comes from the binary's read-only type
// descriptors. Maps are decoded from the runtime's map layout (see
// walkMap), as delve does.

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
	badMaps                                int // maps not walked: unknown layout or bad header
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
			w.walkMap(t, word(b), level)
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

// walkMap walks the first maxElems entries of the map of type t whose
// runtime header (internal/runtime/maps.Map) is at addr, in memory order,
// as delve does: a small map (dirLen 0) is one group of 8 slots at dirPtr;
// otherwise dirPtr is an array of dirLen table pointers (several entries
// may share a table), and each table has lengthMask+1 groups. A slot is
// full if its control byte's top bit is clear.
func (w *rawWalker) walkMap(t reflect.Type, addr uintptr, level int) {
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
	group := func(g uintptr) bool { // walks group g's full slots; false once maxElems are done
		b := w.read(g, mt.groupSize)
		if b == nil {
			return true
		}
		for i := range uintptr(groupSlots) {
			if b[i]&ctrlEmpty != 0 {
				continue
			}
			if entries >= w.maxElems {
				return false
			}
			slot := b[slotsOffset+i*mt.slotSize:][:mt.slotSize]
			w.entry(t, mt, slot, entries, level)
			entries++
		}
		return entries < w.maxElems
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
func (w *rawWalker) entry(t reflect.Type, mt *mapType, slot []byte, i, level int) {
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

// keyLiteral is keyLiteral for a key of type t whose bytes are b.
func (w *rawWalker) keyLiteral(t reflect.Type, b []byte) (string, bool) {
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
	fmt.Fprintf(os.Stderr, "argmatch-stats: %s raw %v reads=%d bytes=%d readErrs=%d maps=%d badMaps=%d badTypes=%d rows=%d\n",
		w.fn, time.Since(start).Round(time.Microsecond), r.reads, r.bytes, r.readErrs, r.maps, r.badMaps, r.badTypes, len(w.rows))
}
