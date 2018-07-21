package main

import (
	"bytes"
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
	"reflect"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/tools/imports"
)

var tagRegex = regexp.MustCompile(`([0-9a-zA-Z,_=&\(\)\-]+)(:( )?"([0-9a-zA-Z,_=&\(\)\-]*)")?`)

var (
	srcTypeDir  = flag.String("srcdir", "", "comma-separated list of type names; must be set")
	srcTypeName = flag.String("srctype", "", "comma-separated list of type names; must be set")
	dstTypeName = flag.String("dsttype", "", "comma-separated list of type names; must be set")
)

// Usage is a replacement usage function for the flags package.
func Usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("repacker: ")
	flag.Usage = Usage
	flag.Parse()
	if len(*srcTypeDir) == 0 || len(*srcTypeName) == 0 || len(*dstTypeName) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	} else if len(args) > 1 {
		flag.Usage()
		os.Exit(2)
	}

	if !isDirectory(args[0]) {
		log.Fatalf("Directory must be specified")
		os.Exit(2)
	} else if !isDirectory(*srcTypeDir) {
		log.Fatalf("The specified srcdir is not directory")
		os.Exit(2)
	}

	g := &Generator{}

	g.dstpkg = &Package{}
	dir := args[0]
	dstFset, astFiles := g.parsePackageDir(dir)
	g.dstpkg.dir = dir
	g.dstpkg.name = astFiles[0].Name.Name
	g.dstpkg.astFiles = astFiles

	g.srcpkg = &Package{}
	srcFset, astFiles := g.parsePackageDir(*srcTypeDir)
	g.srcpkg.dir = *srcTypeDir
	g.srcpkg.name = astFiles[0].Name.Name
	g.srcpkg.astFiles = astFiles

	g.generateHead()

	conf := types.Config{
		Importer: importer.Default(),
		Error: func(err error) {
			fmt.Printf("!!! %#v\n", err)
		},
	}

	srcpkg, err := conf.Check(g.srcpkg.name, srcFset, g.srcpkg.astFiles, nil)
	if err != nil {
		log.Fatal(err)
	}
	src := srcpkg.Scope().Lookup(*srcTypeName)

	dstpkg, err := conf.Check(g.dstpkg.name, dstFset, g.dstpkg.astFiles, nil)
	if err != nil {
		log.Fatal(err)
	}
	dst := dstpkg.Scope().Lookup(*dstTypeName)

	if src == nil || dst == nil {
		log.Fatal("not found")
	}
	g.generate(src, dst)

	// Format the output.
	srcCode, err := g.goimport()
	if err != nil {
		log.Fatalf("Failed to format code: %s", err)
	}

	// Write to file.
	baseName := fmt.Sprintf("%s_repack.go", dst.Name())
	outputName := filepath.Join(g.dstpkg.dir, strings.ToLower(baseName))
	err = ioutil.WriteFile(outputName, srcCode, 0644)
	if err != nil {
		log.Fatalf("writing output: %s", err)
	}
}

// Generator holds the state of the analysis. Primarily used to buffer
// the output for format.Source.
type Generator struct {
	buf    bytes.Buffer
	srcpkg *Package
	dstpkg *Package
}

// parsePackageDir parses the package residing in the directory.
func (g *Generator) parsePackageDir(directory string) (*token.FileSet, []*ast.File) {
	pkg, err := build.Default.ImportDir(directory, 0)
	if err != nil {
		log.Fatalf("cannot process directory %s: %s", directory, err)
	}
	names := prefixDirectory(directory, pkg.GoFiles)
	return g.parsePackage(directory, names, nil)
}

// prefixDirectory places the directory name on the beginning of each name in the list.
func prefixDirectory(directory string, names []string) []string {
	if directory == "." {
		return names
	}
	ret := make([]string, len(names))
	for i, name := range names {
		ret[i] = filepath.Join(directory, name)
	}
	return ret
}

// parsePackage analyzes the single package constructed from the named files.
// If text is non-nil, it is a string to be used instead of the content of the file,
// to be used for testing. parsePackage exits if there is an error.
func (g *Generator) parsePackage(directory string, names []string, text interface{}) (fs *token.FileSet, files []*ast.File) {
	var astFiles []*ast.File
	fs = token.NewFileSet()
	for _, name := range names {
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_repack.go") {
			continue
		}
		parsedFile, err := parser.ParseFile(fs, name, text, parser.ParseComments)
		if err != nil {
			log.Fatalf("parsing package: %s: %s", name, err)
		}
		astFiles = append(astFiles, parsedFile)
	}
	if len(astFiles) == 0 {
		log.Fatalf("%s: no buildable Go files", directory)
	}
	return fs, astFiles
}

// Printf prints
func (g *Generator) Printf(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

func (g *Generator) generateHead() {
	g.Printf("// Code generated by \"repacker %s\"; DO NOT EDIT\n", strings.Join(os.Args[1:], " "))
	g.Printf("\n")
	g.Printf("package %s", g.dstpkg.name)
	g.Printf("\n")
	g.Printf("import \"encoding/json\"\n")
}

func (g *Generator) generate(src, dst types.Object) {
	srcInternal := src.Type().Underlying().(*types.Struct)
	dstInternal := dst.Type().Underlying().(*types.Struct)
	srcPkgName := src.Pkg().Name()

	g.Printf("// New%s creates %s from %s.%s\n", dst.Name(), dst.Name(), srcPkgName, src.Name())
	g.Printf("func New%s (s *%s.%s) *%s {\n", dst.Name(), srcPkgName, src.Name(), dst.Name())
	g.Printf("	return &%s{\n", dst.Name())
	for i := 0; i < srcInternal.NumFields(); i++ {
		for j := 0; j < dstInternal.NumFields(); j++ {
			srcField := srcInternal.Field(i)
			dstField := dstInternal.Field(j)
			srcTag, srcTagFound := reflect.StructTag(srcInternal.Tag(i)).Lookup("repack")
			dstTag, dstTagFound := reflect.StructTag(dstInternal.Tag(j)).Lookup("repack")

			if srcField.Name() == dstField.Name() || (srcTagFound && dstTagFound && (srcTag == dstTag)) {
				src := fmt.Sprintf("s.%s", srcField.Name())
				if srcField.Type() != dstField.Type() {
					switch dstField.Type().String() {
					case "string":
						src = fmt.Sprintf("fmt.Sprintf(%s)", src)
					default:
						log.Printf("skip field (%s) due to difference types", srcField.Name())
						continue
					}
				}
				g.Printf("		%s:  %s,\n", dstField.Name(), src)
				break
			}
		}
	}
	g.Printf("	}\n")
	g.Printf("}\n")

}

func (g *Generator) goimport() ([]byte, error) {
	src, err := imports.Process("", g.buf.Bytes(), nil)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to formats and adjusts imports for the provided file")
	}
	return src, nil
}

type Package struct {
	dir      string
	name     string
	astFiles []*ast.File
}

// isDirectory reports whether the named file is a directory.
func isDirectory(name string) bool {
	info, err := os.Stat(name)
	if err != nil {
		log.Fatal(err)
	}
	return info.IsDir()
}