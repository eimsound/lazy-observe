package meta

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const entImportPrefix = "github.com/eimsound/lazy-observe/internal/db/ent"

// TestADR0004MetaPublicAPIDoesNotLeakEntTypes enforces ADR 0004: no exported
// declaration in internal/meta may reference any type, constant, or alias from
// internal/db/ent (or its subpackages).
func TestADR0004MetaPublicAPIDoesNotLeakEntTypes(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no Go source files found in internal/meta")
	}

	var violations []string
	for _, filename := range entries {
		if strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filename, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filename, err)
		}
		entAliases := collectEntImportAliases(file)
		if len(entAliases) == 0 {
			continue
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !ast.IsExported(s.Name.Name) {
							continue
						}
						checkExprForEntLeak(fset, filename, "type "+s.Name.Name, s.Type, entAliases, &violations)
					case *ast.ValueSpec:
						exported := false
						for _, name := range s.Names {
							if ast.IsExported(name.Name) {
								exported = true
								break
							}
						}
						if !exported {
							continue
						}
						label := "value " + s.Names[0].Name
						if s.Type != nil {
							checkExprForEntLeak(fset, filename, label, s.Type, entAliases, &violations)
						}
						for _, val := range s.Values {
							checkExprForEntLeak(fset, filename, label, val, entAliases, &violations)
						}
					}
				}
			case *ast.FuncDecl:
				if !ast.IsExported(d.Name.Name) {
					continue
				}
				if d.Recv != nil {
					exportedRecv := false
					for _, field := range d.Recv.List {
						if isExportedReceiverType(field.Type) {
							exportedRecv = true
						}
					}
					if !exportedRecv {
						continue
					}
				}
				label := "func " + d.Name.Name
				if d.Type.TypeParams != nil {
					for _, field := range d.Type.TypeParams.List {
						checkExprForEntLeak(fset, filename, label, field.Type, entAliases, &violations)
					}
				}
				if d.Type.Params != nil {
					for _, field := range d.Type.Params.List {
						checkExprForEntLeak(fset, filename, label, field.Type, entAliases, &violations)
					}
				}
				if d.Type.Results != nil {
					for _, field := range d.Type.Results.List {
						checkExprForEntLeak(fset, filename, label, field.Type, entAliases, &violations)
					}
				}
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf(
			"ADR 0004 violation: internal/meta public API leaks internal/db/ent:\n%s",
			strings.Join(violations, "\n"),
		)
	}
}

// TestADR0004NoExternalPackageImportsEnt ensures internal/db/ent stays strictly
// encapsulated behind internal/meta across the entire repository (including tests).
func TestADR0004NoExternalPackageImportsEnt(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var violations []string
	for _, root := range []string{"cmd", "internal"} {
		scanRoot := filepath.Join(repoRoot, root)
		err := filepath.WalkDir(scanRoot, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				rel, err := filepath.Rel(repoRoot, path)
				if err != nil {
					return err
				}
				rel = filepath.ToSlash(rel)
				if rel == "internal/db" || strings.HasPrefix(rel, "internal/db/") ||
					rel == "internal/meta" || strings.HasPrefix(rel, "internal/meta/") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".go") {
				return nil
			}
			file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if parseErr != nil {
				return parseErr
			}
			for _, imp := range file.Imports {
				unquoted, unquoteErr := strconv.Unquote(imp.Path.Value)
				if unquoteErr != nil {
					continue
				}
				if unquoted == entImportPrefix || strings.HasPrefix(unquoted, entImportPrefix+"/") {
					rel, _ := filepath.Rel(repoRoot, path)
					violations = append(violations, filepath.ToSlash(rel)+" imports "+unquoted)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", scanRoot, err)
		}
	}
	if len(violations) > 0 {
		t.Fatalf(
			"ADR 0004 violation: packages outside internal/meta import internal/db/ent:\n%s",
			strings.Join(violations, "\n"),
		)
	}
}

func collectEntImportAliases(file *ast.File) map[string]string {
	aliases := make(map[string]string)
	for _, imp := range file.Imports {
		unquoted, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if unquoted != entImportPrefix && !strings.HasPrefix(unquoted, entImportPrefix+"/") {
			continue
		}
		var name string
		if imp.Name != nil {
			name = imp.Name.Name
		} else {
			parts := strings.Split(unquoted, "/")
			name = parts[len(parts)-1]
		}
		if name != "_" && name != "" {
			aliases[name] = unquoted
		}
	}
	return aliases
}

func isExportedReceiverType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return isExportedReceiverType(t.X)
	case *ast.Ident:
		return ast.IsExported(t.Name)
	case *ast.IndexExpr:
		return isExportedReceiverType(t.X)
	case *ast.IndexListExpr:
		return isExportedReceiverType(t.X)
	default:
		return false
	}
}

func checkExprForEntLeak(
	fset *token.FileSet,
	filename, contextLabel string,
	node ast.Node,
	entAliases map[string]string,
	violations *[]string,
) {
	if node == nil {
		return
	}
	ast.Inspect(node, func(n ast.Node) bool {
		if st, ok := n.(*ast.StructType); ok {
			if st.Fields != nil {
				for _, field := range st.Fields.List {
					exported := len(field.Names) == 0 && isExportedReceiverType(field.Type)
					for _, name := range field.Names {
						if ast.IsExported(name.Name) {
							exported = true
						}
					}
					if exported {
						checkExprForEntLeak(fset, filename, contextLabel, field.Type, entAliases, violations)
					}
				}
			}
			return false
		}
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if importPath, leaked := entAliases[ident.Name]; leaked {
			pos := fset.Position(sel.Pos())
			*violations = append(
				*violations,
				filename+":"+strconv.Itoa(
					pos.Line,
				)+" ("+contextLabel+") references "+ident.Name+"."+sel.Sel.Name+" from "+importPath,
			)
		}
		return true
	})
}
