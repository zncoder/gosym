// +build go1.6

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/loader"
)

var (
	verbose   = flag.Bool("debug", false, "verbose")
	filename  = flag.String("f", "", "file")
	offset    = flag.Int("o", 0, "offset in file, 1 based")
	stdin     = flag.Bool("i", false, "read file from stdin")
	godef     = flag.String("godef", "godef.orig", "path to godef")
	cacheFile = flag.String("cache", os.ExpandEnv("$HOME/.cache/gosym.recent"), "recent go symbols")
)

func lg(format string, arg ...interface{}) {
	if *verbose {
		_, fn, ln, _ := runtime.Caller(1)
		log.Printf(fmt.Sprintf("%s:%d %s", fn, ln, format), arg...)
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

func pkgFiles(p string) (files, imports []string) {
	pkg, err := build.Import(p, "", 0)
	if err != nil {
		lg("import pkg=%s err=%v pkg=%+v", p, err, pkg)
		return nil, nil
	}

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
	return out, pkg.Imports
}

func isTestFile(fn string) bool {
	fn = strings.TrimSuffix(fn, filepath.Ext(fn))
	return strings.HasSuffix(fn, "_test")
}

var fileBody []byte
var fileSHA1 string

func sha(b []byte) string {
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func parseMyPkg() (myPkg string, fs []*ast.File, imports []string, chain []ast.Node) {
	myPkg = pkgPath(*filename)
	fns, imports := pkgFiles(myPkg)
	if fns == nil {
		fns = []string{*filename}
	}
	lg("mypkg=%s: files=%v imports=%v", myPkg, fns, imports)

	for _, fn := range fns {
		var f *ast.File
		var err error

		if fn == *filename {
			if *stdin {
				fileBody, err = ioutil.ReadAll(os.Stdin)
			} else {
				fileBody, err = ioutil.ReadFile(fn)
			}
			if err != nil {
				log.Fatalf("read stdin or file=%s err=%v", fn, err)
			}
			fileSHA1 = sha(fileBody)
			f = parseFile(fn, fileBody)
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
	return myPkg, fs, imports, chain
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
	fns, _ := pkgFiles(path)
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

func newHybridImporter(pkgInUse string) *hybridImporter {
	hi := &hybridImporter{
		pkgInUse: pkgInUse,
		cfg: types.Config{
			Importer: importer.Default(),
			Error:    func(err error) {},
			DisableUnusedImportCheck: true,
		},
	}
	return hi
}

func (hi *hybridImporter) Import(path string) (pkg *types.Package, err error) {
	if hi.pkgInUse != path {
		lg("import pkg=%s using default importer", path)
		if pkg, err = hi.cfg.Importer.Import(path); err == nil {
			return pkg, nil
		}
		lg("import pkg=%s using default importer err=%v. fall back to source importer", path, err)
	}

	if pkg, err = importSrcPkg(&hi.cfg, path); err == nil {
		return pkg, nil
	}
	lg("import source pkg=%s err=%v pkg=%v", path, err, pkg)
	return pkg, err
}

// compatible with godef
func fail() {
	fmt.Fprintln(os.Stderr, "godef: no identifier found")
	os.Exit(2)
}

func printTargetObj(obj types.Object) {
	if obj != nil && obj.Pos() != token.NoPos {
		fmt.Println(printPos(obj.Pos()))
		return
	}
	if *godef != "" {
		cmd := exec.Command(*godef, os.Args[1:]...)
		cmd.Stdin = bytes.NewReader(fileBody)
		b, err := cmd.Output()
		if err != nil {
			fail()
		}
		os.Stdout.Write(b)
		return
	}
	fail()
}

func findInMyPkg(myPkg string, fs []*ast.File, target *ast.Ident) (obj types.Object, otherPkg string) {
	cfg := types.Config{
		Importer: importer.Default(),
		Error:    func(err error) {},
		DisableUnusedImportCheck: true,
	}
	info := types.Info{
		Uses: make(map[*ast.Ident]types.Object),
	}
	cfg.Check(myPkg, fset, fs, &info)

	if obj = info.Uses[target]; obj == nil {
		lg("object of target=%v not found", target)
		return nil, ""
	}
	// BUG:https://github.com/golang/go/issues/13898
	otherPkg = obj.Pkg().Path()
	lg("obj of target=%v is %v in pkg=%s", target, obj, otherPkg)

	if otherPkg == myPkg {
		return obj, myPkg
	}
	return nil, otherPkg
}

func twoPass(myPkg string, fs []*ast.File, target *ast.Ident) types.Object {
	// first pass to find out the package of target
	obj, otherPkg := findInMyPkg(myPkg, fs, target)
	if obj != nil {
		lg("find in mypkg")
		return obj
	}
	if otherPkg == "" {
		return nil
	}

	// second pass to find out the object of target in otherPkg
	cfg := types.Config{
		Importer: newHybridImporter(otherPkg),
		Error:    func(err error) {},
		DisableUnusedImportCheck: true,
	}
	cfg.Importer = newHybridImporter(otherPkg)
	info := types.Info{
		Uses: make(map[*ast.Ident]types.Object),
	}
	cfg.Check(myPkg, fset, fs, &info)
	return info.Uses[target]
}

func parseProgram(myPkg string, fs []*ast.File, target *ast.Ident) types.Object {
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

	return findIdentObj(prog, target)
}

func findIdentObj(prog *loader.Program, target *ast.Ident) types.Object {
	pkg := prog.Created[0]
	for id, obj := range pkg.Uses {
		if target == id {
			return obj
		}
	}
	for id, obj := range pkg.Defs {
		if target == id {
			return obj
		}
	}
	return nil
}

// ident => object: position, created, sha1
type objectEntry struct {
	FromSHA1 string
	ToPos    string
	ToSHA1   string
	Created  int64
	bad      bool
}

type recentObjects struct {
	entries map[string]*objectEntry
}

var recents *recentObjects

func findRecent(ident *ast.Ident) {
	if recents == nil {
		return
	}

	k := printPos(ident.Pos())

	ent, ok := recents.entries[k]
	if !ok {
		return
	}

	if !validEntry(ent) {
		ent.bad = true
		return
	}

	lg("find in recent")
	fmt.Println(ent.ToPos)
	os.Exit(0)
}

func validEntry(ent *objectEntry) bool {
	if ent.FromSHA1 != fileSHA1 {
		return false
	}

	i := strings.LastIndexByte(ent.ToPos, ':')
	if i < 0 {
		return false
	}
	i = strings.LastIndexByte(ent.ToPos[:i], ':')
	if i < 0 {
		return false
	}
	fn := ent.ToPos[:i]

	return fileSHA(fn) == ent.ToSHA1
}

func fileSHA(fn string) string {
	b, err := ioutil.ReadFile(fn)
	if err != nil {
		return ""
	}
	return sha(b)
}

func loadRecents() {
	if *cacheFile == "" {
		return
	}

	b, err := ioutil.ReadFile(*cacheFile)
	if err != nil {
		return
	}

	recents = &recentObjects{}
	if err = json.Unmarshal(b, &recents.entries); err != nil {
		lg("unmarshal recents err=%v", err)
	}
}

func saveRecent(ident *ast.Ident, obj types.Object) {
	if *cacheFile == "" {
		return
	}
	if obj == nil || obj.Pos() == token.NoPos {
		return
	}

	if fset.Position(ident.Pos()).Filename == fset.Position(obj.Pos()).Filename {
		lg("same file")
		return
	}

	k := printPos(ident.Pos())

	sha := fileSHA(fset.Position(obj.Pos()).Filename)
	if sha == "" {
		return
	}

	now := time.Now()
	ent := &objectEntry{
		FromSHA1: fileSHA1,
		ToPos:    printPos(obj.Pos()),
		ToSHA1:   sha,
		Created:  now.UnixNano(),
	}
	lg("new recent entry %s => %+v", k, ent)

	if recents == nil {
		recents = &recentObjects{entries: make(map[string]*objectEntry)}
	}
	recents.entries[k] = ent

	expire := now.Add(-24 * time.Hour).UnixNano()
	for k, ent := range recents.entries {
		if ent.bad || ent.Created < expire {
			delete(recents.entries, k)
		}
	}

	b, err := json.MarshalIndent(recents.entries, "", "  ")
	if err != nil {
		lg("marshal recents err=%v", err)
		return
	}
	if err = ioutil.WriteFile(*cacheFile, b, 0600); err != nil {
		lg("write recents err=%v", err)
	}
}

func parallelPass(myPkg string, fs []*ast.File, target *ast.Ident) types.Object {
	var wg sync.WaitGroup
	if recents != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			findRecent(target)
		}()
	}
	defer wg.Wait()

	out := make(chan types.Object, 2)
	go func() {
		obj := twoPass(myPkg, fs, target)
		if obj != nil {
			lg("find in two-pass")
		}
		out <- obj
	}()
	go func() {
		obj := parseProgram(myPkg, fs, target)
		if obj != nil {
			lg("find in parseprogram")
		}
		out <- obj
	}()
	for i := 0; i < 2; i++ {
		obj := <-out
		if obj != nil {
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

	*filename, _ = filepath.Abs(*filename)

	// offset is 1-based, but token.File.Offset is 0-based.
	*offset--
	if *offset < 0 {
		fail()
	}

	lg("args=%v", os.Args)

	loadRecents()

	myPkg, fs, _, chain := parseMyPkg()
	target := findIdent(chain)
	if target == nil {
		fail()
	}
	lg("target is %v@%v", target, printPos(target.Pos()))

	obj := parallelPass(myPkg, fs, target)
	lg("target=%v in otherpkg obj=%v", target, obj)

	saveRecent(target, obj)
	printTargetObj(obj)
}
