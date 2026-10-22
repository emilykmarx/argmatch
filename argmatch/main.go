// argmatch is a -toolexec wrapper that makes a Go program report, each time
// a given function is called, which strings and byte slices in the function's receiver and
// arguments contain any of the given match strings, and which named types
// and interfaces contain each match.
//
//	go build -o argmatch .   # in this directory
//	go build -toolexec "/path/to/argmatch -func main.ID.F -match needle -depth 5" .
//	ARGMATCH_FUNC=main.ID.F ARGMATCH_MATCH=needle GOFLAGS=-toolexec=/path/to/argmatch go build .
//	go build -toolexec "/path/to/argmatch -func main.ID.F -matchfile /abs/path/matches.txt" .
//	go build -toolexec "/path/to/argmatch -stacklog /abs/path/stacks.log -match needle" .
//
// The function may be in any package of the build, including the standard
// library, and is named as delve names it: pkg/path.Func, pkg/path.T.M or
// pkg/path.(*T).M. With -stacklog, every function in the stacktraces of a
// connection log (in the format conftamer parses) is checked as well.
//
// Matches are printed to the program's stderr, or written to the file
// given by -out, overwriting it.
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
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/tools/gopls/conftamer/stacklog"
)

const (
	rtPkg     = "argmatch/rt"
	nestedEnv = "ARGMATCH_NESTED" // set while building rt's dependencies
	checkFn   = "__argmatch_check"
)

// Each flag defaults to an environment variable, for use with
// GOFLAGS=-toolexec=/path/to/argmatch (GOFLAGS values can't contain spaces).
var (
	funcName       = flag.String("func", os.Getenv("ARGMATCH_FUNC"), "function to check, as named by delve (env ARGMATCH_FUNC)")
	stackLog       = flag.String("stacklog", os.Getenv("ARGMATCH_STACKLOG"), "absolute path of a log of connection stacktraces; every function in the stacks is checked, except those stacklog.IgnoreFn ignores (env ARGMATCH_STACKLOG)")
	needle         = flag.String("match", os.Getenv("ARGMATCH_MATCH"), "string to look for (env ARGMATCH_MATCH)")
	matchFile      = flag.String("matchfile", os.Getenv("ARGMATCH_MATCHFILE"), "absolute path of a file of strings to look for, one per line, in addition to -match; blank lines and lines starting with # are ignored (env ARGMATCH_MATCHFILE)")
	exact          = flag.Bool("exact", envBool("ARGMATCH_EXACT", true), "report only strings equal to a match string; -exact=false also reports strings that contain one (env ARGMATCH_EXACT)")
	ignoreClosures = flag.Bool("ignoreclosures", envBool("ARGMATCH_IGNORECLOSURES", true), "don't check the variables captured by closures and method values; -ignoreclosures=false checks them too (env ARGMATCH_IGNORECLOSURES)")
	recvOnly       = flag.Bool("recvonly", envBool("ARGMATCH_RECVONLY", false), "check only method receivers, not parameters; functions without a receiver are skipped (env ARGMATCH_RECVONLY)")
	depth          = flag.Int("depth", envInt("ARGMATCH_DEPTH", 5), "how many levels of nested structs, arrays, maps and pointers-to-pointers to load (delve's MaxVariableRecurse; env ARGMATCH_DEPTH)")
	maxElems       = flag.Int("maxelems", envInt("ARGMATCH_MAXELEMS", 64), "how many elements of each array, slice and map to load (delve's MaxArrayValues); the loaded tree grows roughly as maxelems^depth, and the default was tested with Prometheus up to -depth 10 (env ARGMATCH_MAXELEMS)")
	outPath        = flag.String("out", os.Getenv("ARGMATCH_OUT"), "file the program writes its matches to, overwriting it; resolved against the program's working directory (default: stderr; env ARGMATCH_OUT)")
	rtDir          = flag.String("rtdir", os.Getenv("ARGMATCH_RTDIR"), "directory of the argmatch module (default: the directory containing this executable; env ARGMATCH_RTDIR)")
)

// needles holds -match and the lines of -matchfile.
var needles []string

// funcs holds the functions to check, without any "[...]": -func, and the
// functions in the stacks of -stacklog.
var funcs = map[string]bool{}

// loadFuncs fills in funcs. Parsing a large log takes a while and the go
// command runs the wrapper once per package, so the functions found in a
// log are cached in the user cache directory, keyed on the log's path,
// size and modification time, and on this executable (whose parser may
// have changed).
func loadFuncs() {
	if *funcName != "" {
		funcs[strings.ReplaceAll(*funcName, "[...]", "")] = true
	}
	if *stackLog == "" {
		return
	}
	if !filepath.IsAbs(*stackLog) {
		fmt.Fprintf(os.Stderr, "argmatch: -stacklog %s must be an absolute path\n", *stackLog)
		os.Exit(2)
	}
	fi, err := os.Stat(*stackLog)
	check(err)
	self, err := os.Executable()
	check(err)
	selfInfo, err := os.Stat(self)
	check(err)
	var cache string
	if dir, err := os.UserCacheDir(); err == nil {
		key := sha256.Sum256(fmt.Appendf(nil, "%q %d %d %q %d %d", *stackLog, fi.Size(), fi.ModTime().UnixNano(),
			self, selfInfo.Size(), selfInfo.ModTime().UnixNano()))
		cache = filepath.Join(dir, "argmatch", fmt.Sprintf("%x.funcs", key[:8]))
		if data, err := os.ReadFile(cache); err == nil {
			for _, fn := range strings.Fields(string(data)) {
				funcs[fn] = true
			}
			return
		}
	}
	f, err := os.Open(*stackLog)
	check(err)
	defer f.Close()
	found := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for {
		conn, err := stacklog.ParseOneConn(scanner)
		check(err)
		if conn.LastConn {
			break
		}
		for _, line := range conn.Stacks {
			if frame, ok := stacklog.FrameFunc(line); ok {
				// E.g. check closures' enclosing functions instead.
				for _, fn := range stacklog.EnclosingFns(frame) {
					found[strings.ReplaceAll(fn, "[...]", "")] = true
				}
			}
		}
	}
	check(scanner.Err())
	list := slices.Sorted(maps.Keys(found))
	for _, fn := range list {
		funcs[fn] = true
	}
	if cache != "" {
		// Write and rename, so concurrent compiles never see a partial file.
		if os.MkdirAll(filepath.Dir(cache), 0o755) == nil {
			tmp := fmt.Sprintf("%s.%d", cache, os.Getpid())
			if os.WriteFile(tmp, []byte(strings.Join(list, "\n")+"\n"), 0o644) == nil {
				os.Rename(tmp, cache)
			}
		}
	}
}

// loadNeedles returns -match and the lines of -matchfile that are neither
// blank nor comments (starting with #), without duplicates. Each line is
// used as is, apart from a trailing \r.
func loadNeedles() []string {
	var ns []string
	if *needle != "" {
		ns = append(ns, *needle)
	}
	if *matchFile != "" {
		// The go command runs tools in each package's directory, so a
		// relative path would mean something different for each package.
		if !filepath.IsAbs(*matchFile) {
			fmt.Fprintf(os.Stderr, "argmatch: -matchfile %s must be an absolute path\n", *matchFile)
			os.Exit(2)
		}
		data, err := os.ReadFile(*matchFile)
		check(err)
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSuffix(line, "\r")
			if line != "" && !strings.HasPrefix(line, "#") && !slices.Contains(ns, line) {
				ns = append(ns, line)
			}
		}
	}
	if len(ns) == 0 {
		fmt.Fprintln(os.Stderr, "argmatch: no strings to match")
		os.Exit(2)
	}
	return ns
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return n
	}
	return def
}

// envBool returns the environment variable's value as strconv.ParseBool
// sees it (1, t, true, 0, f, false, ...), or def if it's unset or not a
// boolean.
func envBool(name string, def bool) bool {
	if b, err := strconv.ParseBool(os.Getenv(name)); err == nil {
		return b
	}
	return def
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: go build -toolexec \"%s {-func pkg.Func | -stacklog /path} {-match string | -matchfile /path} [-depth n]\" ...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if *funcName == "" && *stackLog == "" || *needle == "" && *matchFile == "" || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	needles = loadNeedles()
	loadFuncs()
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
		fmt.Fprintf(h, "%q %q %t %t %d %d %q %t ", slices.Sorted(maps.Keys(funcs)), needles, *exact, *ignoreClosures, *depth, *maxElems, *outPath, *recvOnly)
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
	isStd := slices.Contains(args, "-std")
	args = append([]string{"-N", "-l"}, args...)
	isMain := pkg == "main" && os.Getenv(nestedEnv) == ""
	injectedMain, declared := false, false
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
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name, explicit, ok := matchesFunc(pkg, isStd, fd)
			if !ok || fd.Recv.NumFields() == 0 && (*recvOnly || fd.Type.Params.NumFields() == 0) {
				continue // nothing to check
			}
			if fd.Body == nil {
				if explicit {
					fmt.Fprintf(os.Stderr, "argmatch: %s has no Go body; cannot instrument it\n", name)
					os.Exit(1)
				}
				continue
			}
			edits = append(edits, instrument(fset, fd, name)...)
		}
		if len(edits) > 0 && !declared {
			// The target package can't import rt (it may be one of rt's
			// own dependencies), so refer to rt.Check by its linker name
			// instead, declaring it once per package.
			edits = append(edits,
				edit{fset.Position(f.Name.End()).Offset, `; import _ "unsafe"`},
				edit{len(src), fmt.Sprintf("\n//go:linkname %s %s.Check\nfunc %s([]string, bool, bool, int, int, string, string, []string, []interface{})\n", checkFn, rtPkg, checkFn)},
			)
			declared = true
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

// matchesFunc reports whether fd, in package pkg, is one of the functions
// to check, and returns its name as delve names it. explicit reports
// whether it was given by -func. Functions found only in -stacklog are
// skipped if stacklog.IgnoreFn ignores them; isStd reports whether pkg is
// in the standard library.
func matchesFunc(pkg string, isStd bool, fd *ast.FuncDecl) (name string, explicit, ok bool) {
	name = funcDeclName(pkg, fd)
	if name == "" {
		return "", false, false
	}
	short := strings.ReplaceAll(name, "[...]", "")
	if *funcName != "" && short == strings.ReplaceAll(*funcName, "[...]", "") {
		return name, true, true
	}
	if !funcs[short] || stacklog.IgnoreFn(name, func(string) bool { return isStd }) {
		return "", false, false
	}
	return name, false, true
}

// funcDeclName returns the name delve gives fd, in package pkg, or "" if
// it can't tell.
func funcDeclName(pkg string, fd *ast.FuncDecl) string {
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
			return ""
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
	return name
}

// instrument returns the edits that make fd, named fn, call rt.Check with
// the addresses of its receiver and parameters (only the receiver with
// -recvonly), naming unnamed ones.
func instrument(fset *token.FileSet, fd *ast.FuncDecl, fn string) []edit {
	off := func(p token.Pos) int { return fset.Position(p).Offset }
	var edits []edit
	var names, addrs []string
	fields := fd.Type.Params.List
	if *recvOnly {
		fields = nil
	}
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
	quotedNeedles := make([]string, len(needles))
	for i, n := range needles {
		quotedNeedles[i] = strconv.Quote(n)
	}
	call := fmt.Sprintf("%s([]string{%s}, %t, %t, %d, %d, %q, %q, []string{%s}, []interface{}{%s});",
		checkFn, strings.Join(quotedNeedles, ", "), *exact, *ignoreClosures, *depth, *maxElems, *outPath, fn, strings.Join(quoted, ", "), strings.Join(addrs, ", "))
	return append(edits, edit{off(fd.Body.Lbrace) + 1, call})
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
	toolexec := strings.Join([]string{quote(self), "-func", quote(*funcName), "-stacklog", quote(*stackLog), "-match", quote(*needle),
		"-matchfile", quote(*matchFile), "-depth", strconv.Itoa(*depth), "-maxelems", strconv.Itoa(*maxElems), "-recvonly=" + strconv.FormatBool(*recvOnly), "-exact=" + strconv.FormatBool(*exact), "-ignoreclosures=" + strconv.FormatBool(*ignoreClosures),
		"-out", quote(*outPath), "-rtdir", quote(*rtDir)}, " ")
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
	// The copy is in another directory, so make rt's own local replacements
	// absolute.
	mod = regexp.MustCompile(`(?m)^(replace \S+ => )(\.\.?/\S*)$`).ReplaceAllFunc(mod, func(line []byte) []byte {
		prefix, path, _ := strings.Cut(string(line), "=> ")
		return []byte(prefix + "=> " + filepath.Join(*rtDir, path))
	})
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
