// Package apicheck renders the exported surface of the supported packages so a
// change to it has to be deliberate.
//
// The problem this exists for is not hypothetical. Over two days this repository
// added five methods to matching.CommandLog, changed what matching.Shards accepts,
// added fields to orderentry.Msg and bumped the wire protocol twice. Every one of
// those breaks somebody, every one was defensible on its own, and nothing in the
// build or the test suite said "this is a compatibility event" — the tests were
// updated along with the code, which is exactly what makes a break invisible.
//
// So the surface is rendered to a golden file, the same trick internal/wire uses
// for the protocol. The check does not stop a breaking change. It makes one
// impossible to ship without a human regenerating the file and, one hopes, noticing
// what they are regenerating. See docs/COMPATIBILITY.md.
package apicheck

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Covered lists the package paths under pkg/ whose exported surface is frozen.
// Anything not here — internal/, cmd/, examples/ — carries no promise, and
// internal/ additionally cannot be imported.
func Covered(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "pkg"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// Render walks the covered packages and returns their exported surface, one
// declaration per line, sorted. The output is deliberately plain text: a diff of it
// should be readable in a pull request without tooling.
func Render(root string) (string, error) {
	pkgs, err := Covered(root)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, pkg := range pkgs {
		got, err := renderPackage(filepath.Join(root, "pkg", pkg), pkg)
		if err != nil {
			return "", err
		}
		lines = append(lines, got...)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n", nil
}

func renderPackage(dir, pkg string) ([]string, error) {
	fset := token.NewFileSet()
	// Test files are excluded: a test helper is not API, and including them would
	// make every new test a compatibility event.
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		return nil, err
	}

	var lines []string
	for _, p := range pkgs {
		for _, file := range p.Files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if l := renderFunc(fset, pkg, d); l != "" {
						lines = append(lines, l)
					}
				case *ast.GenDecl:
					lines = append(lines, renderGen(fset, pkg, d)...)
				}
			}
		}
	}
	return lines, nil
}

func renderFunc(fset *token.FileSet, pkg string, d *ast.FuncDecl) string {
	if !d.Name.IsExported() {
		return ""
	}
	sig := signatureString(fset, d.Type)
	if d.Recv == nil {
		return fmt.Sprintf("%s.%s%s", pkg, d.Name.Name, sig)
	}
	// A method counts only when its receiver is exported; a method on an unexported
	// type is unreachable from outside and so is not surface.
	recv := typeString(fset, d.Recv.List[0].Type)
	bare := strings.TrimPrefix(strings.TrimPrefix(recv, "*"), "[]")
	if bare == "" || !ast.IsExported(bare) {
		return ""
	}
	return fmt.Sprintf("%s.(%s).%s%s", pkg, recv, d.Name.Name, sig)
}

func renderGen(fset *token.FileSet, pkg string, d *ast.GenDecl) []string {
	var lines []string
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			if !s.Name.IsExported() {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s.%s %s", pkg, s.Name.Name, kindOf(s.Type)))
			lines = append(lines, renderMembers(fset, pkg, s)...)
		case *ast.ValueSpec:
			kw := "var"
			if d.Tok == token.CONST {
				kw = "const"
			}
			for _, n := range s.Names {
				if n.IsExported() {
					lines = append(lines, fmt.Sprintf("%s.%s %s", pkg, n.Name, kw))
				}
			}
		}
	}
	return lines
}

// renderMembers records struct fields and interface methods.
//
// Interface methods are the sharp ones: adding a method to an exported interface
// breaks every implementer outside this repository, silently, at their build rather
// than ours. That is exactly what happened to matching.CommandLog, and it is the
// case this file most wants to make loud.
func renderMembers(fset *token.FileSet, pkg string, s *ast.TypeSpec) []string {
	var lines []string
	switch t := s.Type.(type) {
	case *ast.StructType:
		for _, f := range fieldList(t.Fields) {
			for _, n := range f.names {
				lines = append(lines, fmt.Sprintf("%s.%s.%s %s", pkg, s.Name.Name, n, typeString(fset, f.typ)))
			}
		}
	case *ast.InterfaceType:
		for _, f := range fieldList(t.Methods) {
			for _, n := range f.names {
				lines = append(lines, fmt.Sprintf("%s.%s.%s%s (interface method)", pkg, s.Name.Name, n, signatureString(fset, f.typ)))
			}
		}
	}
	return lines
}

type member struct {
	names []string
	typ   ast.Expr
}

func fieldList(fl *ast.FieldList) []member {
	if fl == nil {
		return nil
	}
	var out []member
	for _, f := range fl.List {
		var names []string
		for _, n := range f.Names {
			if n.IsExported() {
				names = append(names, n.Name)
			}
		}
		if len(names) > 0 {
			out = append(out, member{names: names, typ: f.Type})
		}
	}
	return out
}

func kindOf(e ast.Expr) string {
	switch e.(type) {
	case *ast.StructType:
		return "struct"
	case *ast.InterfaceType:
		return "interface"
	case *ast.FuncType:
		return "func type"
	default:
		return "type"
	}
}

// signatureString renders a func type without its leading "func" keyword, so a
// method reads as Cancel(id int64) error rather than Cancelfunc(id int64) error.
// The diff is meant to be read in a pull request; it has to look like Go.
func signatureString(fset *token.FileSet, e ast.Expr) string {
	return strings.TrimPrefix(typeString(fset, e), "func")
}

func typeString(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, fset, e); err != nil {
		return "?"
	}
	// Collapse whitespace so a gofmt-driven line break does not read as an API
	// change.
	return strings.Join(strings.Fields(b.String()), " ")
}
