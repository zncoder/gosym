// +build go1.5

package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/loader"
)

var (
	verbose   = flag.Bool("debug", false, "verbose")
	logToFile = flag.Bool("log", false, "log to tmp file")
	filename  = flag.String("f", "", "file")
	offset    = flag.Int("o", 0, "offset in file, 1 based")
	stdin     = flag.Bool("i", false, "read file from stdin")
	// 2-pass is the fastest and loader is slowest
	pass = flag.Int("p", 3, "parse algorithm. can be 1, 2 or 3. 2 is the fastest. 3 is slowest but most reliable")
)

var logf *os.File

func lg(format string, arg ...interface{}) {
	if logf != nil {
		fmt.Fprintf(logf, format+"\n", arg...)
	} else if *verbose {
		log.Printf(format, arg...)
	}
}

func parseFile(fn string, src interface{}) *ast.File {
	f, err := parser.ParseFile(fset, fn, src, parser.AllErrors)
	if err != nil {
		// error is expected
		lg("parse file=%s err=%v", fn, err)
	}
	return f
}

func pkgPath(fn string) string {
	dir := filepath.Dir(fn)
	i := strings.LastIndex(dir, "/src/")
	if i < 0 {
		return "main"
	}
	return dir[i+len("/src/"):]
}

func tokenFile(f *ast.File) *token.File {
	return fset.File(f.Package)
}

func printPos(pos token.Pos) string {
	return fset.Position(pos).String()
}

func pkgFiles(p string) []string {
	pkg, err := build.Import(p, "", 0)
	if err != nil {
		lg("import pkg=%s err=%v", p, err)
		return nil
	}

	// TODO: use Imports in one-pass to parse fewer files.
	lg("pkg=%s imports %v", p, pkg.Imports)

	isTest := isTestFile(*filename)
	n := len(pkg.GoFiles)
	m := n
	if isTest {
		n += len(pkg.TestGoFiles)
	}

	out := make([]string, n)
	for i, f := range pkg.GoFiles {
		out[i] = filepath.Join(pkg.Dir, f)
	}
	if isTest {
		for i, f := range pkg.TestGoFiles {
			out[m+i] = filepath.Join(pkg.Dir, f)
		}
	}
	return out
}

func isTestFile(fn string) bool {
	fn = strings.TrimSuffix(fn, filepath.Ext(fn))
	return strings.HasSuffix(fn, "_test")
}

func parseMyPkg() (myPkg string, fs []*ast.File, chain []ast.Node) {
	myPkg = pkgPath(*filename)
	fns := pkgFiles(myPkg)
	if fns == nil {
		fns = []string{*filename}
	}
	lg("files in pkg=%s: %v", myPkg, fns)

	for _, fn := range fns {
		var f *ast.File
		if fn == *filename && *stdin {
			b, err := ioutil.ReadAll(os.Stdin)
			if err != nil {
				log.Fatalf("read stdin err=%v", err)
			}
			f = parseFile(fn, b)
		} else {
			f = parseFile(fn, nil)
		}

		fs = append(fs, f)

		if fn == *filename {
			tf := tokenFile(f)
			pos := tf.Pos(*offset)
			if pos == token.NoPos {
				lg("no pos found")
				fail()
			}
			lg("pos=%v", printPos(pos))
			chain, _ = astutil.PathEnclosingInterval(f, pos, pos+1)
		}
	}
	return myPkg, fs, chain
}

func findIdent(chain []ast.Node) *ast.Ident {
	for _, nd := range chain {
		if id, ok := nd.(*ast.Ident); ok {
			return id
		}
	}
	return nil
}

type srcImporter struct {
	cfg types.Config
}

func newSrcImporter() srcImporter {
	return srcImporter{
		cfg: types.Config{
			Importer: importer.Default(),
			Error:    func(err error) {},
			DisableUnusedImportCheck: true,
		},
	}
}

func importSrcPkg(cfg *types.Config, path string) (*types.Package, error) {
	fns := pkgFiles(path)
	var fs []*ast.File
	for _, fn := range fns {
		f := parseFile(fn, nil)
		fs = append(fs, f)
	}

	return cfg.Check(path, fset, fs, nil)
}

func (si srcImporter) Import(path string) (*types.Package, error) {
	return importSrcPkg(&si.cfg, path)
}

type hybridImporter struct {
	cfg      types.Config
	pkgInUse string
}

func newHybridImporter(pkg string) *hybridImporter {
	return &hybridImporter{
		pkgInUse: pkg,
		cfg: types.Config{
			Importer: importer.Default(),
			Error:    func(err error) {},
			DisableUnusedImportCheck: true,
		},
	}
}

func (hi *hybridImporter) Import(path string) (*types.Package, error) {
	if path != hi.pkgInUse {
		// TODO: try cfg when the default importer fails to find the binary package.
		lg("import pkg=%s using default importer", path)
		return hi.cfg.Importer.Import(path)
	}
	return importSrcPkg(&hi.cfg, hi.pkgInUse)
}

func onePass(myPkg string, fs []*ast.File, target *ast.Ident) {
	cfg := types.Config{
		Importer: newSrcImporter(),
		Error:    func(err error) {},
		DisableUnusedImportCheck: true,
	}
	info := types.Info{
		Uses: make(map[*ast.Ident]types.Object),
	}
	cfg.Check(myPkg, fset, fs, &info)
	obj := info.Uses[target]
	lg("target=%v uses obj=%v", target, obj)
	printTargetObj(obj)
}

// compatible with godef
func fail() {
	fmt.Fprintln(os.Stderr, "godef: no identifier found")
	os.Exit(2)
}

func printTargetObj(obj types.Object) {
	if obj != nil && obj.Pos() != token.NoPos {
		fmt.Println(printPos(obj.Pos()))
	} else {
		fail()
	}
}

func twoPass(myPkg string, fs []*ast.File, target *ast.Ident) {
	// first pass to find out the package of target
	cfg := types.Config{
		Importer: newHybridImporter(""),
		Error:    func(err error) {},
		DisableUnusedImportCheck: true,
	}
	info := types.Info{
		Uses: make(map[*ast.Ident]types.Object),
	}
	cfg.Check(myPkg, fset, fs, &info)

	obj := info.Uses[target]
	if obj == nil {
		lg("object of target=%v not found", target)
		fail()
	}
	// BUG:https://github.com/golang/go/issues/13898
	otherPkg := obj.Pkg().Path()
	lg("obj of target=%v is %v in pkg=%s", target, obj, otherPkg)

	if otherPkg == myPkg {
		lg("found in mypkg")
		printTargetObj(obj)
		return
	}

	// second pass to find out the object of target
	cfg.Importer = newHybridImporter(otherPkg)
	info.Uses = make(map[*ast.Ident]types.Object)
	cfg.Check(myPkg, fset, fs, &info)
	obj = info.Uses[target]
	lg("target=%v in otherpkg obj=%v", target, obj)
	printTargetObj(obj)
}

func parseProgram(myPkg string, fs []*ast.File, target *ast.Ident) {
	cfg := loader.Config{
		Fset:       fset,
		ParserMode: parser.AllErrors,
		TypeChecker: types.Config{
			Error: func(error) {},
			DisableUnusedImportCheck: true,
		},
		TypeCheckFuncBodies: func(path string) bool {
			return path == myPkg
		},
		AllowErrors: true,
	}
	os.Setenv("CGO_ENABLED", "0")

	cfg.CreateFromFiles(myPkg, fs...)
	prog, err := cfg.Load()
	if err != nil {
		lg("load program err=%v", err)
	}

	obj := findIdentObj(prog, target)
	lg("target=%v uses obj=%v", target, obj)
	printTargetObj(obj)
}

func findIdentObj(prog *loader.Program, target *ast.Ident) types.Object {
	pkg := prog.Created[0]
	for id, obj := range pkg.Defs {
		if target == id {
			return obj
		}
	}
	for id, obj := range pkg.Uses {
		if target == id {
			return obj
		}
	}
	return nil
}

var fset = token.NewFileSet()

func main() {
	// flags used by godef
	flag.Bool("A", false, "")
	flag.Bool("a", false, "")
	flag.Bool("acme", false, "")
	flag.Bool("t", false, "")
	flag.Parse()

	if *logToFile {
		logf, _ = ioutil.TempFile(os.TempDir(), "gosym-log.")
	}

	*filename, _ = filepath.Abs(*filename)

	// offset is 1-based, but token.File.Offset is 0-based.
	*offset--
	if *offset < 0 {
		fail()
	}

	lg("args=%v", os.Args)

	myPkg, fs, chain := parseMyPkg()
	target := findIdent(chain)
	if target == nil {
		fail()
	}
	lg("target is %v@%v", target, printPos(target.Pos()))

	switch *pass {
	case 1:
		onePass(myPkg, fs, target)
	case 2:
		twoPass(myPkg, fs, target)
	case 3:
		parseProgram(myPkg, fs, target)
	}
}
