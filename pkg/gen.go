// Copyright 2018 The CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build ignore

// gen.go generates the pkg.go files inside the packages under the pkg directory.
//
// It takes the list of packages from the packages.txt.
//
// Be sure to also update an entry in pkg/pkg.go, if so desired.
package main

// TODO generate ../register.go too.

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"go/constant"
	"go/format"
	"go/token"
	"go/types"
	"log"
	"math/big"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"golang.org/x/tools/go/packages"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/build"
	"cuelang.org/go/cue/errors"
	cueformat "cuelang.org/go/cue/format"
	"cuelang.org/go/internal"
	"cuelang.org/go/internal/core/runtime"
)

const genFile = "pkg.go"

//go:embed packages.txt
var packagesStr string

type headerParams struct {
	GoPkg  string
	CUEPkg string

	PackageDoc  string
	PackageDefs string
}

var header = template.Must(template.New("").Parse(
	`// Code generated by cuelang.org/go/pkg/gen. DO NOT EDIT.

{{if .PackageDoc}}
{{.PackageDoc -}}
//     {{.PackageDefs}}
{{end -}}
package {{.GoPkg}}

{{if .CUEPkg -}}
import (
	"cuelang.org/go/internal/core/adt"
	"cuelang.org/go/internal/pkg"
)

func init() {
	pkg.Register({{printf "%q" .CUEPkg}}, p)
}

var _ = adt.TopKind // in case the adt package isn't used
{{end}}
`))

const pkgParent = "cuelang.org/go/pkg"

func main() {
	flag.Parse()
	log.SetFlags(log.Lshortfile)
	log.SetOutput(os.Stdout)

	var packagesList []string
	for _, pkg := range strings.Fields(packagesStr) {
		if pkg == "path" {
			// TODO remove this special case. Currently the path
			// pkg.go file cannot be generated automatically but that
			// will be possible when we can attach arbitrary signatures
			// to builtin functions.
			continue
		}
		packagesList = append(packagesList, path.Join(pkgParent, pkg))
	}

	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedFiles | packages.NeedTypes}
	pkgs, err := packages.Load(cfg, packagesList...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load: %v\n", err)
		os.Exit(1)
	}
	if packages.PrintErrors(pkgs) > 0 {
		os.Exit(1)
	}
	for _, pkg := range pkgs {
		if err := generate(pkg); err != nil {
			log.Fatalf("%s: %v", pkg, err)
		}
	}
}

type generator struct {
	dir        string
	w          *bytes.Buffer
	cuePkgPath string
	first      bool
}

func generate(pkg *packages.Package) error {
	// go/packages supports multiple build systems, including some which don't keep
	// a Go package entirely within a single directory.
	// However, we know for certain that CUE uses modules, so it is the case here.
	// We can figure out the directory from the first Go file.
	pkgDir := filepath.Dir(pkg.GoFiles[0])
	cuePkg := strings.TrimPrefix(pkg.PkgPath, pkgParent+"/")
	g := generator{
		dir:        pkgDir,
		cuePkgPath: cuePkg,
		w:          &bytes.Buffer{},
	}

	params := headerParams{
		GoPkg:  pkg.Name,
		CUEPkg: cuePkg,
	}
	// As a special case, the "tool" package cannot be imported from CUE.
	skipRegister := params.CUEPkg == "tool"
	if skipRegister {
		params.CUEPkg = ""
	}

	if doc, err := os.ReadFile(filepath.Join(pkgDir, "doc.txt")); err == nil {
		defs, err := os.ReadFile(filepath.Join(pkgDir, pkg.Name+".cue"))
		if err != nil {
			return err
		}
		i := bytes.Index(defs, []byte("package "+pkg.Name))
		defs = defs[i+len("package "+pkg.Name)+1:]
		defs = bytes.TrimRight(defs, "\n")
		defs = bytes.ReplaceAll(defs, []byte("\n"), []byte("\n//\t"))
		params.PackageDoc = string(doc)
		params.PackageDefs = string(defs)
	}

	if err := header.Execute(g.w, params); err != nil {
		return err
	}

	if !skipRegister {
		fmt.Fprintf(g.w, "var p = &pkg.Package{\nNative: []*pkg.Builtin{")
		g.first = true
		if err := g.processGo(pkg); err != nil {
			return err
		}
		fmt.Fprintf(g.w, "},\n")
		if err := g.processCUE(); err != nil {
			return err
		}
		fmt.Fprintf(g.w, "}\n")
	}

	b, err := format.Source(g.w.Bytes())
	if err != nil {
		fmt.Printf("go/format error on %s: %v\n", pkg.PkgPath, err)
		b = g.w.Bytes() // write the unformatted source
	}

	filename := filepath.Join(pkgDir, genFile)

	if err := os.WriteFile(filename, b, 0o666); err != nil {
		return err
	}
	return nil
}

func (g *generator) sep() {
	if g.first {
		g.first = false
		return
	}
	fmt.Fprint(g.w, ", ")
}

// processCUE mixes in CUE definitions defined in the package directory.
func (g *generator) processCUE() error {
	// Note: we avoid using the cue/load and the cuecontext packages
	// because they depend on the standard library which is what this
	// command is generating - cyclic dependencies are undesirable in general.
	ctx := newContext()
	val, err := loadCUEPackage(ctx, g.dir, g.cuePkgPath)
	if err != nil {
		if errors.Is(err, errNoCUEFiles) {
			return nil
		}
		errors.Print(os.Stderr, err, nil)
		return fmt.Errorf("error processing %s: %v", g.cuePkgPath, err)
	}

	v := val.Syntax(cue.Raw())
	// fmt.Printf("%T\n", v)
	// fmt.Println(astinternal.DebugStr(v))
	n := internal.ToExpr(v)
	b, err := cueformat.Node(n)
	if err != nil {
		return err
	}
	b = bytes.ReplaceAll(b, []byte("\n\n"), []byte("\n"))
	// body = strings.ReplaceAll(body, "\t", "")
	// TODO: escape backtick
	fmt.Fprintf(g.w, "CUE: `%s`,\n", string(b))
	return nil
}

func (g *generator) processGo(pkg *packages.Package) error {
	// We sort the objects by their original source code position.
	// Otherwise go/types defaults to sorting by name strings.
	// We could remove this code if we were fine with sorting by name.
	scope := pkg.Types.Scope()
	type objWithPos struct {
		obj types.Object
		pos token.Position
	}
	var objs []objWithPos
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		objs = append(objs, objWithPos{obj, pkg.Fset.Position(obj.Pos())})
	}
	sort.Slice(objs, func(i, j int) bool {
		obj1, obj2 := objs[i], objs[j]
		if obj1.pos.Filename == obj2.pos.Filename {
			return obj1.pos.Line < obj2.pos.Line
		}
		return obj1.pos.Filename < obj2.pos.Filename
	})

	for _, obj := range objs {
		obj := obj.obj // no longer need the token.Position
		if !obj.Exported() {
			continue
		}
		// TODO: support type declarations.
		switch obj := obj.(type) {
		case *types.Const:
			var value string
			switch v := obj.Val(); v.Kind() {
			case constant.Bool, constant.Int, constant.String:
				// TODO: convert octal numbers
				value = v.ExactString()
			case constant.Float:
				var rat big.Rat
				rat.SetString(v.ExactString())
				var float big.Float
				float.SetRat(&rat)
				value = float.Text('g', -1)
			default:
				fmt.Printf("Dropped entry %s.%s (%T: %v)\n", g.cuePkgPath, obj.Name(), v.Kind(), v.ExactString())
				continue
			}
			g.sep()
			fmt.Fprintf(g.w, "{\nName: %q,\n Const: %q,\n}", obj.Name(), value)
		case *types.Func:
			g.genFunc(obj)
		}
	}
	return nil
}

var errorType = types.Universe.Lookup("error").Type()

func (g *generator) genFunc(fn *types.Func) {
	sign := fn.Type().(*types.Signature)
	if sign.Recv() != nil {
		return
	}
	params := sign.Params()
	results := sign.Results()
	if results == nil || (results.Len() != 1 && results.At(1).Type() != errorType) {
		fmt.Printf("Dropped func %s.%s: must have one return value or a value and an error %v\n", g.cuePkgPath, fn.Name(), sign)
		return
	}

	g.sep()
	fmt.Fprintf(g.w, "{\n")
	defer fmt.Fprintf(g.w, "}")

	fmt.Fprintf(g.w, "Name: %q,\n", fn.Name())

	args := []string{}
	vals := []string{}
	kind := []string{}
	for i := 0; i < params.Len(); i++ {
		param := params.At(i)
		typ := strings.Title(g.goKind(param.Type()))
		argKind := g.goToCUE(param.Type())
		vals = append(vals, fmt.Sprintf("c.%s(%d)", typ, len(args)))
		args = append(args, param.Name())
		kind = append(kind, argKind)
	}

	fmt.Fprintf(g.w, "Params: []pkg.Param{\n")
	for _, k := range kind {
		fmt.Fprintf(g.w, "{Kind: %s},\n", k)
	}
	fmt.Fprintf(g.w, "\n},\n")

	fmt.Fprintf(g.w, "Result: %s,\n", g.goToCUE(results.At(0).Type()))

	argList := strings.Join(args, ", ")
	valList := strings.Join(vals, ", ")
	init := ""
	if len(args) > 0 {
		init = fmt.Sprintf("%s := %s", argList, valList)
	}

	fmt.Fprintf(g.w, "Func: func(c *pkg.CallCtxt) {")
	defer fmt.Fprintln(g.w, "},")
	fmt.Fprintln(g.w)
	if init != "" {
		fmt.Fprintln(g.w, init)
	}
	fmt.Fprintln(g.w, "if c.Do() {")
	defer fmt.Fprintln(g.w, "}")
	if results.Len() == 1 {
		fmt.Fprintf(g.w, "c.Ret = %s(%s)", fn.Name(), argList)
	} else {
		fmt.Fprintf(g.w, "c.Ret, c.Err = %s(%s)", fn.Name(), argList)
	}
}

// TODO(mvdan): goKind and goToCUE still use a lot of strings; simplify.

func (g *generator) goKind(typ types.Type) string {
	if ptr, ok := typ.(*types.Pointer); ok {
		typ = ptr.Elem()
	}
	switch str := types.TypeString(typ, nil); str {
	case "math/big.Int":
		return "bigInt"
	case "math/big.Float":
		return "bigFloat"
	case "math/big.Rat":
		return "bigRat"
	case "cuelang.org/go/internal/core/adt.Bottom":
		return "error"
	case "github.com/cockroachdb/apd/v3.Decimal":
		return "decimal"
	case "cuelang.org/go/internal/pkg.List":
		return "cueList"
	case "cuelang.org/go/internal/pkg.Struct":
		return "struct"
	case "[]*github.com/cockroachdb/apd/v3.Decimal":
		return "decimalList"
	case "cuelang.org/go/cue.Value":
		return "value"
	case "cuelang.org/go/cue.List":
		return "list"
	case "[]string":
		return "stringList"
	case "[]byte":
		return "bytes"
	case "[]cuelang.org/go/cue.Value":
		return "list"
	case "io.Reader":
		return "reader"
	case "time.Time":
		return "string"
	default:
		return str
	}
}

func (g *generator) goToCUE(typ types.Type) (cueKind string) {
	// TODO: detect list and structs types for return values.
	switch k := g.goKind(typ); k {
	case "error":
		cueKind += "adt.BottomKind"
	case "bool":
		cueKind += "adt.BoolKind"
	case "bytes", "reader":
		cueKind += "adt.BytesKind|adt.StringKind"
	case "string":
		cueKind += "adt.StringKind"
	case "int", "int8", "int16", "int32", "rune", "int64",
		"uint", "byte", "uint8", "uint16", "uint32", "uint64",
		"bigInt":
		cueKind += "adt.IntKind"
	case "float64", "bigRat", "bigFloat", "decimal":
		cueKind += "adt.NumKind"
	case "list", "decimalList", "stringList", "cueList":
		cueKind += "adt.ListKind"
	case "struct":
		cueKind += "adt.StructKind"
	case "value":
		// Must use callCtxt.value method for these types and resolve manually.
		cueKind += "adt.TopKind" // TODO: can be more precise
	default:
		switch {
		case strings.HasPrefix(k, "[]"):
			cueKind += "adt.ListKind"
		case strings.HasPrefix(k, "map["):
			cueKind += "adt.StructKind"
		default:
			// log.Println("Unknown type:", k)
			// Must use callCtxt.value method for these types and resolve manually.
			cueKind += "adt.TopKind" // TODO: can be more precise
		}
	}
	return cueKind
}

var errNoCUEFiles = errors.New("no CUE files in directory")

// loadCUEPackage loads a CUE package as a value. We avoid using cue/load because
// that depends on the standard library and as this generator is generating the standard
// library, we don't want that cyclic dependency.
// It only has to deal with the fairly limited subset of CUE packages that are
// present inside pkg/....
func loadCUEPackage(ctx *cue.Context, dir, pkgPath string) (cue.Value, error) {
	inst := &build.Instance{
		PkgName:     path.Base(pkgPath),
		Dir:         dir,
		DisplayPath: pkgPath,
		ImportPath:  pkgPath,
	}
	cuefiles, err := filepath.Glob(filepath.Join(dir, "*.cue"))
	if err != nil {
		return cue.Value{}, err
	}
	if len(cuefiles) == 0 {
		return cue.Value{}, errNoCUEFiles
	}
	for _, file := range cuefiles {
		if err := inst.AddFile(file, nil); err != nil {
			return cue.Value{}, err
		}
	}
	if err := inst.Complete(); err != nil {
		return cue.Value{}, err
	}
	vals, err := ctx.BuildInstances([]*build.Instance{inst})
	if err != nil {
		return cue.Value{}, err
	}
	return vals[0], nil
}

// Avoid using cuecontext.New because that package imports
// the entire stdlib which we are generating.
func newContext() *cue.Context {
	r := runtime.New()
	return (*cue.Context)(r)
}
