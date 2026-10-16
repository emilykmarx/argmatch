// argmatch is a -toolexec wrapper that makes a Go program report, each time
// a given function is called, which strings and byte slices in the function's receiver and
// arguments contain a given needle, and which named types and interfaces
// contain each match.
//
//	go build -o argmatch .   # in this directory
//	go build -toolexec "/path/to/argmatch -func main.ID.F -match needle -depth 5" .
//	ARGMATCH_FUNC=main.ID.F ARGMATCH_MATCH=needle GOFLAGS=-toolexec=/path/to/argmatch go build .
//
// The function may be in any package of the build, including the standard
// library, and is named as delve names it: pkg/path.Func, pkg/path.T.M or
// pkg/path.(*T).M.
//
// Matches are printed to the program's stderr, or appended to the file
// given by -out.
//
// The wrapper never stops the program. Instead it rewrites code at compile
// time: it compiles everything with -N -l so values are readable, injects a
// call to argmatch/rt.Check at the top of the function, and links in
// argmatch/rt, which evaluates the arguments with delve from inside the
// program.
package main

import (
	"bufio"
	"crypto/sha256"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	rtPkg     = "argmatch/rt"
	nestedEnv = "ARGMATCH_NESTED" // set while building rt's dependencies
	checkFn   = "__argmatch_check"
)

// Each flag defaults to an environment variable, for use with
// GOFLAGS=-toolexec=/path/to/argmatch (GOFLAGS values can't contain spaces).
var (
	funcName = flag.String("func", os.Getenv("ARGMATCH_FUNC"), "function to check, as named by delve (required; env ARGMATCH_FUNC)")
	needle   = flag.String("match", os.Getenv("ARGMATCH_MATCH"), "string to look for (required; env ARGMATCH_MATCH)")
	depth    = flag.Int("depth", envInt("ARGMATCH_DEPTH", 5), "how many levels of nested structs, arrays, maps and pointers-to-pointers to load (delve's MaxVariableRecurse; env ARGMATCH_DEPTH)")
	outPath  = flag.String("out", os.Getenv("ARGMATCH_OUT"), "file the program appends its matches to, resolved against the program's working directory (default: stderr; env ARGMATCH_OUT)")
	rtDir    = flag.String("rtdir", os.Getenv("ARGMATCH_RTDIR"), "directory of the argmatch module (default: the directory containing this executable; env ARGMATCH_RTDIR)")
)

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return n
	}
	return def
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: go build -toolexec \"%s -func pkg.Func -match string [-depth n]\" ...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *funcName == "" || *needle == "" || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if *rtDir == "" {
		exe, err := os.Executable()
		check(err)
		*rtDir = filepath.Dir(exe)
	}
	tool, args := flag.Arg(0), flag.Args()[1:]

	if len(args) == 1 && args[0] == "-V=full" {
		printToolID(tool)
		return
	}
	tmp, err := os.MkdirTemp("", "argmatch")
	check(err)
	switch strings.TrimSuffix(filepath.Base(tool), ".exe") {
	case "compile":
		args = compileArgs(tool, args, tmp)
	case "link":
		if os.Getenv(nestedEnv) == "" {
			args = withRT(tool, args, tmp)
		}
	}
	cmd := exec.Command(tool, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	os.RemoveAll(tmp)
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		check(err)
	}
}

// printToolID prints the tool's version line with the argmatch settings
// appended, so that the go command caches our instrumented, unoptimized
// builds separately from normal ones and from other settings. The go
// command doesn't know about the injected dependency on rt, so rt's
// sources and module files are included too.
func printToolID(tool string) {
	out, err := exec.Command(tool, "-V=full").Output()
	check(err)
	line := strings.TrimSpace(string(out))
	// Development toolchains identify tools by the trailing buildID=
	// field instead, so leave their output alone.
	if !strings.Contains(line, "devel") {
		self, err := os.Executable()
		check(err)
		rtFiles, err := filepath.Glob(filepath.Join(*rtDir, "rt", "*.go"))
		check(err)
		h := sha256.New()
		fmt.Fprintf(h, "%q %q %d %q ", *funcName, *needle, *depth, *outPath)
		for _, f := range append([]string{self, filepath.Join(*rtDir, "go.mod"), filepath.Join(*rtDir, "go.sum")}, rtFiles...) {
			b, err := os.ReadFile(f)
			check(err)
			h.Write(b)
		}
		line += fmt.Sprintf(" argmatch=%x", h.Sum(nil)[:8])
	}
	fmt.Println(line)
}

// compileArgs disables optimizations, injects the call to rt.Check if the
// package contains the target function, and imports rt into main packages.
func compileArgs(tool string, args []string, tmp string) []string {
	pkg := ""
	if i := slices.Index(args, "-p"); i >= 0 && i+1 < len(args) {
		pkg = args[i+1]
	}
	args = append([]string{"-N", "-l"}, args...)
	isMain := pkg == "main" && os.Getenv(nestedEnv) == ""
	injectedMain := false
	for i, path := range args {
		if !strings.HasSuffix(path, ".go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			continue // not a source file
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			continue // let the compiler report it
		}
		var edits []edit
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && matchesFunc(pkg, fd) {
				if fd.Body == nil {
					fmt.Fprintf(os.Stderr, "argmatch: %s has no Go body; cannot instrument it\n", *funcName)
					os.Exit(1)
				}
				edits = append(edits, instrument(fset, f, fd, len(src))...)
			}
		}
		if isMain && !injectedMain {
			edits = append(edits, edit{fset.Position(f.Name.End()).Offset, fmt.Sprintf("; import _ %q", rtPkg)})
			injectedMain = true
		}
		if len(edits) == 0 {
			continue
		}
		// Edits don't add lines, so positions in DWARF and panics stay correct.
		slices.SortStableFunc(edits, func(a, b edit) int { return b.off - a.off })
		for _, e := range edits {
			src = slices.Concat(src[:e.off], []byte(e.text), src[e.off:])
		}
		newPath := filepath.Join(tmp, fmt.Sprintf("%d_%s", i, filepath.Base(path)))
		check(os.WriteFile(newPath, src, 0o644))
		args[i] = newPath
		// Record the original path in DWARF and export data; a temp path
		// would also make the export data differ between builds.
		rewrite := newPath + "=>" + path
		if j := slices.Index(args, "-trimpath"); j >= 0 && j+1 < len(args) {
			args[j+1] = rewrite + ";" + args[j+1]
		} else {
			args = append([]string{"-trimpath", rewrite}, args...)
		}
		// The injected bodyless declaration is incompatible with -complete.
		args = slices.DeleteFunc(args, func(a string) bool { return a == "-complete" })
	}
	if injectedMain {
		args = withRT(tool, args, tmp)
	}
	return args
}

type edit struct {
	off  int
	text string
}

// matchesFunc reports whether fd, in package pkg, is the target function.
func matchesFunc(pkg string, fd *ast.FuncDecl) bool {
	name := pkg + "." + fd.Name.Name
	if fd.Recv != nil && len(fd.Recv.List) == 1 {
		typ, star := fd.Recv.List[0].Type, false
		if s, ok := typ.(*ast.StarExpr); ok {
			typ, star = s.X, true
		}
		generic := false
		switch t := typ.(type) {
		case *ast.IndexExpr:
			typ, generic = t.X, true
		case *ast.IndexListExpr:
			typ, generic = t.X, true
		}
		id, ok := typ.(*ast.Ident)
		if !ok {
			return false
		}
		recv := id.Name
		if generic {
			recv += "[...]"
		}
		if star {
			recv = "(*" + recv + ")"
		}
		name = pkg + "." + recv + "." + fd.Name.Name
	}
	return name == *funcName || strings.ReplaceAll(name, "[...]", "") == *funcName
}

// instrument returns the edits that make fd call rt.Check with the
// addresses of its receiver and parameters, naming unnamed ones.
func instrument(fset *token.FileSet, f *ast.File, fd *ast.FuncDecl, size int) []edit {
	off := func(p token.Pos) int { return fset.Position(p).Offset }
	var edits []edit
	var names, addrs []string
	fields := fd.Type.Params.List
	if fd.Recv != nil {
		fields = append(slices.Clone(fd.Recv.List), fields...)
	}
	for _, field := range fields {
		if len(field.Names) == 0 {
			name := fmt.Sprintf("__argmatch_%d", len(names))
			edits = append(edits, edit{off(field.Type.Pos()), name + " "})
			names, addrs = append(names, "_"), append(addrs, "&"+name)
		}
		for _, id := range field.Names {
			name := id.Name
			if name == "_" {
				name = fmt.Sprintf("__argmatch_%d", len(names))
				edits = append(edits, edit{off(id.Pos()) + 1, name[1:]}) // "_" + "_argmatch_N"
			}
			names, addrs = append(names, id.Name), append(addrs, "&"+name)
		}
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	call := fmt.Sprintf("%s(%q, %d, %q, %q, []string{%s}, []interface{}{%s});",
		checkFn, *needle, *depth, *outPath, *funcName, strings.Join(quoted, ", "), strings.Join(addrs, ", "))
	return append(edits,
		edit{off(fd.Body.Lbrace) + 1, call},
		// The target package can't import rt (it may be one of rt's own
		// dependencies), so refer to rt.Check by its linker name instead.
		edit{off(f.Name.End()), `; import _ "unsafe"`},
		edit{size, fmt.Sprintf("\n//go:linkname %s %s.Check\nfunc %s(string, int, string, string, []string, []interface{})\n", checkFn, rtPkg, checkFn)},
	)
}

// withRT adds rt and its dependencies to the -importcfg in args.
func withRT(tool string, args []string, tmp string) []string {
	i := slices.Index(args, "-importcfg")
	if i < 0 || i+1 >= len(args) {
		fmt.Fprintln(os.Stderr, "argmatch: no -importcfg in", args)
		os.Exit(1)
	}
	cfg, err := os.ReadFile(args[i+1])
	check(err)
	have := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(string(cfg)))
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "packagefile "); ok {
			path, _, _ := strings.Cut(rest, "=")
			have[path] = true
		}
	}
	extra := ""
	for _, line := range rtExports(tool) {
		path, _, _ := strings.Cut(line, "=")
		if !have[path] {
			extra += "packagefile " + line + "\n"
		}
	}
	newCfg := filepath.Join(tmp, "importcfg")
	check(os.WriteFile(newCfg, append(cfg, extra...), 0o644))
	args = slices.Clone(args)
	args[i+1] = newCfg
	return args
}

// rtExports builds rt and its dependencies the same way as the outer build,
// so that packages shared with the program are identical, and returns
// their "importpath=archive" pairs. That means using this wrapper, the
// outer build's flags, and the program's versions of shared modules.
func rtExports(tool string) []string {
	goroot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(tool)))) // $GOROOT/pkg/tool/GOOS_GOARCH/compile
	goCmd := filepath.Join(goroot, "bin", "go")
	self, err := os.Executable()
	check(err)
	// The go command only recognizes quotes at the start of a word, and
	// doesn't unescape within them.
	quote := func(s string) string {
		if strings.Contains(s, `"`) {
			return "'" + s + "'"
		}
		return `"` + s + `"`
	}
	toolexec := strings.Join([]string{quote(self), "-func", quote(*funcName), "-match", quote(*needle),
		"-depth", strconv.Itoa(*depth), "-out", quote(*outPath), "-rtdir", quote(*rtDir)}, " ")
	tmp, err := os.MkdirTemp("", "argmatch-mod")
	check(err)
	defer os.RemoveAll(tmp)
	args := []string{"list", "-deps", "-export", "-toolexec", toolexec,
		"-modfile", alignedModfile(goCmd, tmp), "-mod=mod"}
	args = append(args, outerBuildFlags()...)
	cmd := exec.Command(goCmd, append(args, "-f", "{{if .Export}}{{.ImportPath}}={{.Export}}{{end}}", rtPkg)...)
	cmd.Dir = *rtDir
	cmd.Env = append(os.Environ(), nestedEnv+"=1", "GOWORK=off")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "argmatch: building %s in %s: %v\n", rtPkg, *rtDir, err)
		os.Exit(1)
	}
	return strings.Fields(string(out))
}

// alignedModfile writes a copy of rt's go.mod (and go.sum) to dir that
// pins every module the program also uses to the program's version, and
// returns its path. The program's module is found from the current
// directory, which the go command sets to the package being built.
func alignedModfile(goCmd, dir string) string {
	mod, err := os.ReadFile(filepath.Join(*rtDir, "go.mod"))
	check(err)
	sum, _ := os.ReadFile(filepath.Join(*rtDir, "go.sum"))
	list := exec.Command(goCmd, "list", "-m", "-f",
		"{{if not .Main}}{{.Path}} {{with .Replace}}{{if .Version}}{{.Path}} {{.Version}}{{else}}{{.Dir}}{{end}}{{else}}{{.Path}} {{.Version}}{{end}}{{end}}", "all")
	list.Env = append(os.Environ(), "GOFLAGS=-mod=readonly")
	list.Stderr = os.Stderr
	out, err := list.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "argmatch: listing the program's modules:", err)
		os.Exit(1)
	}
	mod = append(mod, '\n')
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if f := strings.Fields(line); len(f) >= 2 {
			mod = fmt.Appendf(mod, "replace %s => %s\n", f[0], strings.Join(f[1:], " "))
		}
	}
	if gomod, err := exec.Command(goCmd, "env", "GOMOD").Output(); err == nil {
		if s := strings.TrimSpace(string(gomod)); s != "" && s != os.DevNull {
			progSum, _ := os.ReadFile(filepath.Join(filepath.Dir(s), "go.sum"))
			sum = append(append(sum, '\n'), progSum...)
		}
	}
	check(os.WriteFile(filepath.Join(dir, "go.mod"), mod, 0o644))
	check(os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o644))
	return filepath.Join(dir, "go.mod")
}

// outerBuildFlags returns the flags of the go command that ran this wrapper
// that change how packages are compiled.
func outerBuildFlags() []string {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", os.Getppid()))
	if err != nil {
		return nil
	}
	withValue := map[string]bool{"tags": true, "buildmode": true, "pgo": true, "installsuffix": true}
	boolean := map[string]bool{"race": true, "msan": true, "asan": true, "trimpath": true}
	var flags []string
	args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
	for i := 1; i < len(args); i++ {
		name, _, hasValue := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		if !strings.HasPrefix(args[i], "-") {
			continue
		}
		switch {
		case boolean[name]:
			flags = append(flags, args[i])
		case withValue[name] && hasValue:
			flags = append(flags, args[i])
		case withValue[name] && i+1 < len(args):
			flags = append(flags, args[i], args[i+1])
			i++
		}
	}
	return flags
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "argmatch:", err)
		os.Exit(1)
	}
}
