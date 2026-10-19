// Package rt is linked into the program being built by argmatch. The
// toolexec wrapper injects a call to Check at the top of the target
// function; Check then uses delve, in-process, to load the function's
// receiver and arguments from the program's own memory and DWARF info.
package rt

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"go/constant"
	"io"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/go-delve/delve/pkg/dwarf/godwarf"
	"github.com/go-delve/delve/pkg/proc"
	"github.com/go-delve/delve/pkg/proc/linutil"
)

var (
	initOnce sync.Once
	initErr  error
	bi       *proc.BinaryInfo
	selfMem  *memory

	outOnce sync.Once
	out     io.Writer = os.Stderr
	csvOut  *csv.Writer

	mu    sync.Mutex
	owner int64 // goroutine ID holding mu, to detect re-entry
)

// Check reports every string in args that contains one of needles, along
// with the named types, interfaces and closures on the path down to it, as
// CSV rows (see csvHeader). names[i] is the source name of the parameter
// whose address is boxed in args[i]. Output is appended to the file
// outPath, or written to stderr if outPath is "". Errors always go to
// stderr.
//
// The push linkname lets the injected bodyless declaration in the target
// package (which can't import rt) refer to this function.
//
//go:linkname Check
func Check(needles []string, depth int, outPath string, fn string, names []string, args []any) {
	// The target function may be called by delve itself (e.g. if it's in
	// the standard library), so ignore calls made while checking.
	gid := goid()
	if mu.TryLock() {
		owner = gid
	} else if owner == gid {
		return
	} else {
		mu.Lock()
		owner = gid
	}
	defer func() { owner = 0; mu.Unlock() }()

	outOnce.Do(func() {
		header := true
		if outPath != "" {
			f, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
			if err != nil {
				fmt.Fprintf(os.Stderr, "argmatch: %v; writing to stderr instead\n", err)
			} else {
				out = f
				// Appending to earlier output: it has a header already.
				if fi, err := f.Stat(); err == nil && fi.Size() > 0 {
					header = false
				}
			}
		}
		csvOut = csv.NewWriter(out)
		if header {
			csvOut.Write(csvHeader)
			csvOut.Flush()
		}
	})
	defer csvOut.Flush()
	initOnce.Do(load)
	if initErr != nil {
		fmt.Fprintf(os.Stderr, "argmatch: %v\n", initErr)
		return
	}
	scope := &proc.EvalScope{BinInfo: bi, Mem: selfMem}
	cfg := proc.LoadConfig{
		FollowPointers:     true,
		MaxVariableRecurse: depth,
		MaxStringLen:       1 << 16,
		MaxArrayValues:     1 << 12,
		MaxStructFields:    -1,
	}
	for i := range args {
		// args[i] is an interface{} holding *T, where T is the parameter's
		// static type; delve resolves *T from the runtime type.
		expr := fmt.Sprintf("*(*interface {})(%#x)", uintptr(unsafe.Pointer(&args[i])))
		shallow := cfg
		shallow.MaxVariableRecurse = -1
		box, err := scope.EvalExpression(expr, shallow)
		if err != nil || len(box.Children) != 1 {
			fmt.Fprintf(os.Stderr, "argmatch: %s: cannot load %s: %v\n", fn, names[i], err)
			continue
		}
		// Delve doesn't count dereferencing *T as a level unless T is a
		// pointer or interface; compensate so depth is relative to T.
		argCfg := cfg
		if ptr, ok := box.Children[0].RealType.(*godwarf.PtrType); ok {
			switch ptr.Type.Common().ReflectKind {
			case reflect.Ptr, reflect.Interface:
				argCfg.MaxVariableRecurse++
			}
		}
		box, err = scope.EvalExpression(expr, argCfg)
		if err != nil || len(box.Children) != 1 || len(box.Children[0].Children) != 1 {
			fmt.Fprintf(os.Stderr, "argmatch: %s: cannot load %s: %v\n", fn, names[i], err)
			continue
		}
		arg := &box.Children[0].Children[0]
		walk(arg, names[i], nil, func(path []string, expr, needle, val string) {
			closure := false
			for _, step := range path {
				closure = closure || strings.HasPrefix(step, closurePrefix)
			}
			csvOut.Write(append([]string{needle, val, fn, expr, strconv.FormatBool(closure)}, path...))
		}, needles)
	}
}

// csvHeader names the columns of the output. Each row has one more column
// per step of the path from the argument down to the match: a named type, an
// interface ("concrete (implements interface)"), or a closure ("closure fn").
// Rows have different lengths, so the last header names the first of them.
var csvHeader = []string{"needle", "haystack", "function", "expression", "closure", "types"}

// closurePrefix starts the path step for a closure; no type name starts
// with it.
const closurePrefix = "closure "

// walk calls report for each string or []byte in v's tree and each of
// needles it contains, if any named type or interface lies on the path to
// it. path holds those types.
func walk(v *proc.Variable, expr string, path []string, report func(path []string, expr, needle, val string), needles []string) {
	if v.Unreadable != nil || v.DwarfType == nil {
		return
	}
	typ := v.DwarfType.String()
	switch {
	case v.Kind == reflect.Interface && len(v.Children) == 1 && v.Children[0].DwarfType != nil:
		// Report the interface together with its dynamic type, then
		// continue below the dynamic value so it isn't reported twice.
		data := &v.Children[0]
		path = append(path, fmt.Sprintf("%s (implements %s)", data.DwarfType.String(), typ))
		walkValue(data, expr, path, report, needles)
		return
	case isNamed(typ):
		path = append(path, typ)
	}
	walkValue(v, expr, path, report, needles)
}

// walkValue checks v itself, whose type is already accounted for in path,
// then walks its children.
func walkValue(v *proc.Variable, expr string, path []string, report func(path []string, expr, needle, val string), needles []string) {
	if len(path) > 0 {
		if s, ok := stringValue(v); ok {
			for _, needle := range needles {
				if strings.Contains(s, needle) {
					report(path, expr, needle, s)
				}
			}
		}
	}
	if v.Kind == reflect.Func && len(v.Children) > 0 {
		// A closure (or method value), whose children are the variables
		// it captured. Name it on the path, so that matches reached
		// through it are clearly marked.
		fn := ""
		if v.Value != nil {
			fn = constant.StringVal(v.Value)
		}
		path = append(path, closurePrefix+fn)
	}
	for i := range v.Children {
		c := &v.Children[i]
		var cexpr string
		switch v.Kind {
		case reflect.Func:
			name := c.Name
			if name == ".this" {
				name = "receiver" // of a method value
			}
			cexpr = fmt.Sprintf("%s[captured %s]", expr, name)
		case reflect.Struct:
			cexpr = expr + "." + c.Name
		case reflect.Ptr, reflect.Interface:
			cexpr = expr
		case reflect.Map:
			if i%2 == 0 {
				cexpr = fmt.Sprintf("%s<key %d>", expr, i/2)
			} else {
				cexpr = fmt.Sprintf("%s[<key %d>]", expr, i/2)
			}
		default:
			cexpr = fmt.Sprintf("%s[%d]", expr, i)
		}
		walk(c, cexpr, path, report, needles)
	}
}

// stringValue returns the contents of a string, or of a byte slice or
// array (as far as delve loaded it).
func stringValue(v *proc.Variable) (string, bool) {
	switch v.Kind {
	case reflect.String:
		if v.Value != nil {
			return constant.StringVal(v.Value), true
		}
	case reflect.Slice, reflect.Array:
		// Delve gives byte elements Kind reflect.Uint, so check the size.
		if len(v.Children) == 0 || v.Children[0].RealType == nil || v.Children[0].RealType.Size() != 1 ||
			(v.Children[0].Kind != reflect.Uint && v.Children[0].Kind != reflect.Uint8) {
			return "", false
		}
		b := make([]byte, len(v.Children))
		for i, c := range v.Children {
			if c.Value == nil {
				return "", false
			}
			n, _ := constant.Uint64Val(c.Value)
			b[i] = byte(n)
		}
		return string(b), true
	}
	return "", false
}

// isNamed reports whether a DWARF type name is a defined type other than a
// predeclared basic type, e.g. main.ID or net/http.Header, as opposed to
// string or a type literal such as []string or *main.Outer.
func isNamed(t string) bool {
	switch t {
	case "", "bool", "string", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"float32", "float64", "complex64", "complex128", "unsafe.Pointer":
		return false
	}
	for _, p := range []string{"*", "[", "map[", "chan ", "<-chan ", "func(", "struct {", "interface {"} {
		if strings.HasPrefix(t, p) {
			return false
		}
	}
	return true
}

func load() {
	exe, err := os.Executable()
	if err != nil {
		initErr = err
		return
	}
	// The entry point is needed to find the load address of PIE binaries.
	auxv, err := os.ReadFile("/proc/self/auxv")
	if err != nil {
		initErr = err
		return
	}
	bi = proc.NewBinaryInfo(runtime.GOOS, runtime.GOARCH)
	if err := bi.LoadBinaryInfo(exe, linutil.EntryPointFromAuxv(auxv, bi.Arch.PtrSize()), nil); err != nil {
		initErr = err
		return
	}
	f, err := os.Open("/proc/self/mem")
	if err != nil {
		initErr = err
		return
	}
	selfMem = &memory{f}
}

// memory reads the program's own memory through /proc/self/mem, so that
// following a bad pointer yields an error rather than a segfault.
type memory struct{ f *os.File }

func (m *memory) ReadMemory(buf []byte, addr uint64) (int, error) {
	return m.f.ReadAt(buf, int64(addr))
}

func (m *memory) WriteMemory(addr uint64, data []byte) (int, error) {
	return 0, fmt.Errorf("argmatch: memory is read-only")
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
