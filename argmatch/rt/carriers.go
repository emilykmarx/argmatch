package rt

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unsafe"
)

// A carrierSpec is one of the Carriers settings, <type>:<field path>: a
// value of the named type <type> (as typeName names it, e.g.
// github.com/prometheus/alertmanager/types.Alert) is a carrier, identified
// by the value at <field path> in it: field names separated by dots, each
// found as reflect's FieldByName finds it (so through embedded structs, e.g.
// Labels in types.Alert's embedded model.Alert), and pointers followed. Its
// key is "<type>:<field path>=" and the value rendered (see render), so two
// carriers have the same key if the values at the path are equal, as render
// compares them: e.g. two alerts with the same labels.
type carrierSpec struct {
	spec string   // as given
	path []string // field names
}

// parseCarriers returns specs parsed by type name, and the specs that
// aren't <type>:<field path>.
func parseCarriers(specs []string) (map[string][]carrierSpec, []string) {
	var bad []string
	m := map[string][]carrierSpec{}
	for _, c := range specs {
		typ, path, ok := strings.Cut(c, ":")
		if !ok || typ == "" || path == "" || slices.Contains(strings.Split(path, "."), "") {
			bad = append(bad, c)
			continue
		}
		m[typ] = append(m[typ], carrierSpec{spec: c, path: strings.Split(path, ".")})
	}
	if len(m) == 0 {
		return nil, bad
	}
	return m, bad
}

// specsFor returns t's carrier specs, nil if it isn't a carrier type.
func specsFor(t reflect.Type) []carrierSpec {
	if v, ok := carrierTypes.Load(t); ok {
		return v.([]carrierSpec)
	}
	var specs []carrierSpec
	if isNamed(t) {
		specs = carrierSpecs[typeName(t)]
	}
	carrierTypes.Store(t, specs)
	return specs
}

// findCarriers adds the keys of the value of type t whose bytes are b to
// w.carriers, if t is a carrier type.
func (w *walker) findCarriers(t reflect.Type, b []byte) {
	for _, c := range specsFor(t) {
		v, vt, ok := w.field(t, b, c.path)
		if !ok {
			continue
		}
		var r strings.Builder
		w.render(&r, vt, v, 0)
		key := c.spec + "=" + r.String()
		if !slices.Contains(w.carriers, key) {
			w.carriers = append(w.carriers, key)
		}
	}
}

// field returns the bytes and type of the field at path in the struct (or
// pointer to one) of type t whose bytes are b.
func (w *walker) field(t reflect.Type, b []byte, path []string) ([]byte, reflect.Type, bool) {
	deref := func() bool {
		for t.Kind() == reflect.Pointer {
			p := word(b)
			if p == 0 {
				return false
			}
			t = t.Elem()
			if b = w.read(p, t.Size()); b == nil {
				return false
			}
		}
		return true
	}
	for _, name := range path {
		if !deref() || t.Kind() != reflect.Struct {
			return nil, nil, false
		}
		sf, ok := t.FieldByName(name)
		if !ok {
			return nil, nil, false
		}
		// sf.Index goes through the embedded structs the field is promoted
		// from, any of which may be a pointer
		for _, i := range sf.Index {
			if !deref() {
				return nil, nil, false
			}
			f := t.Field(i)
			b, t = b[f.Offset:f.Offset+f.Type.Size()], f.Type
		}
	}
	return b, t, true
}

// maxRender bounds the elements and nesting render writes.
const (
	maxRenderElems = 256
	maxRenderDepth = 4
)

// render writes the value of type t whose bytes are b to r, canonically:
// strings quoted, numbers and booleans as Go literals, pointers followed,
// slices and arrays as [e1 e2 ...], maps as {k1=v1 k2=v2 ...} sorted by their
// rendered entries (so two maps with the same entries render the same,
// whatever their memory order), structs as {F1:v1 F2:v2 ...}, and anything
// else (or a value that can't be read) as ?.
func (w *walker) render(r *strings.Builder, t reflect.Type, b []byte, depth int) {
	if b == nil || depth > maxRenderDepth {
		r.WriteString("?")
		return
	}
	if s, ok := w.stringValue(t, b); ok {
		r.WriteString(strconv.Quote(s))
		return
	}
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		lit, _ := w.keyLiteral(t, b)
		r.WriteString(lit)
	case reflect.Pointer:
		p := word(b)
		if p == 0 {
			r.WriteString("nil")
			return
		}
		w.render(r, t.Elem(), w.read(p, t.Elem().Size()), depth+1)
	case reflect.Slice, reflect.Array:
		e := t.Elem()
		n, elems := 0, b
		if t.Kind() == reflect.Array {
			n = t.Len()
		} else if p := word(b); p != 0 && e.Size() > 0 {
			n = min(int(word(b[ptrSize:])), maxRenderElems)
			elems = nil
			if n > 0 {
				elems = w.read(p, uintptr(n)*e.Size())
			}
		}
		r.WriteString("[")
		for i := range min(n, maxRenderElems) {
			if elems == nil {
				r.WriteString("?")
				break
			}
			if i > 0 {
				r.WriteString(" ")
			}
			off := uintptr(i) * e.Size()
			w.render(r, e, elems[off:off+e.Size()], depth+1)
		}
		r.WriteString("]")
	case reflect.Map:
		var entries []string
		if p := word(b); p != 0 {
			w.eachSlot(t, p, maxRenderElems, func(mt *mapType, slot []byte, _ int) {
				key, elem, ok := w.slotEntry(t, mt, slot)
				if !ok {
					return
				}
				var e strings.Builder
				w.render(&e, t.Key(), key, depth+1)
				e.WriteString("=")
				w.render(&e, t.Elem(), elem, depth+1)
				entries = append(entries, e.String())
			})
		}
		slices.Sort(entries)
		r.WriteString("{" + strings.Join(entries, " ") + "}")
	case reflect.Struct:
		r.WriteString("{")
		for i := range t.NumField() {
			f := t.Field(i)
			if i > 0 {
				r.WriteString(" ")
			}
			r.WriteString(f.Name + ":")
			w.render(r, f.Type, b[f.Offset:f.Offset+f.Type.Size()], depth+1)
		}
		r.WriteString("}")
	default:
		r.WriteString("?")
	}
}

// identify gives the call its ID and records its goroutine and request, once
// (see walker.call).
func (w *walker) identify() {
	if w.call != "" {
		return
	}
	w.call = strconv.FormatUint(nextCall.Add(1), 10)
	w.ancestry, w.stack = splitStack(goroutineStack(), isRT)
	w.request, w.requestGoroutine = requestLabels()
}

// The goroutine labels the ConfTamer patch to net/http sets while a handler
// serves a request (conftamerLabelRequest): the request's context ID, as its
// events log it, and the goroutine that started serving it.
const (
	requestLabel          = "conftamer.request"
	requestGoroutineLabel = "conftamer.request_goroutine"
)

// getProfLabel returns the calling goroutine's pprof labels: a pointer to
// runtime/pprof's labelMap, whose layout profLabels mirrors (Go 1.24 and
// later), or nil. The runtime allows this linkname (go.dev/issue/67401).
//
//go:linkname getProfLabel runtime/pprof.runtime_getProfLabel
func getProfLabel() unsafe.Pointer

// profLabels mirrors runtime/pprof's labelMap (internal/runtime/pprof/label's
// Set: a list of key-value pairs).
type profLabels struct {
	list []struct{ key, value string }
}

// requestLabels returns the calling goroutine's request labels (see
// requestLabel), "" for any it doesn't have. A goroutine's labels are only
// replaced, never changed, so reading them is safe.
func requestLabels() (request, goroutine string) {
	p := getProfLabel()
	if p == nil {
		return "", ""
	}
	for _, l := range (*profLabels)(p).list {
		switch l.key {
		case requestLabel:
			request = l.value
		case requestGoroutineLabel:
			goroutine = l.value
		}
	}
	return request, goroutine
}
