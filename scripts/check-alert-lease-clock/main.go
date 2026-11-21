// Command check-alert-lease-clock fails when a query on the alert lease or membership tables
// reads the database clock without going through the shared millisecond clock helpers.
//
// SQLite's CURRENT_TIMESTAMP and datetime() only have whole-second precision, so a lease query
// bound with plain bind() silently shortens every TTL by up to a second (see the alertClockSQL
// comment in internal/meta/alert_engine.go).
package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	clockHelper = "alertClockSQL"
	hint        = "wrap the query in s.alertClockSQL(...) and build expiries with s.alertLeaseExpiry(ttl)"
)

var (
	leaseTable     = regexp.MustCompile(`\b(alert_(engine_members|rule_leases)|source_map_upload_pins)\b`)
	secondOnlyTime = regexp.MustCompile(`(?i)\bdatetime\(\s*'now'`)
)

type violation struct {
	pos     token.Position
	message string
}

func main() {
	dir := "internal/meta"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	violations, err := checkDir(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check-alert-lease-clock:", err)
		os.Exit(2)
	}
	if len(violations) == 0 {
		fmt.Printf("check-alert-lease-clock: %s lease and membership queries use the shared alert clock\n", dir)
		return
	}
	for _, v := range violations {
		fmt.Fprintf(os.Stderr, "%s:%d: %s\n", v.pos.Filename, v.pos.Line, v.message)
	}
	fmt.Fprintf(os.Stderr, "check-alert-lease-clock: %d violation(s); %s\n", len(violations), hint)
	os.Exit(1)
}

func checkDir(dir string) ([]violation, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	fset := token.NewFileSet()
	var violations []violation
	checked := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		checked++
		violations = append(violations, checkFile(fset, file)...)
	}
	if checked == 0 {
		return nil, fmt.Errorf("no non-test Go files in %s", dir)
	}
	return violations, nil
}

// checkFile inspects every function on its own: a clock-reading lease query must either be passed
// straight to alertClockSQL or live in a local variable that only ever reaches alertClockSQL.
func checkFile(fset *token.FileSet, file *ast.File) []violation {
	var violations []violation
	report := func(pos token.Pos, format string, args ...any) {
		violations = append(violations, violation{pos: fset.Position(pos), message: fmt.Sprintf(format, args...)})
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			checkScope(decl, report)
			continue
		}
		checkScope(fn.Body, report)
	}
	return violations
}

func checkScope(scope ast.Node, report func(token.Pos, string, ...any)) {
	// Local variables holding a clock-reading lease query, keyed by name.
	queryVars := map[string]token.Pos{}
	walk(scope, func(node ast.Node, stack []ast.Node) {
		expr, ok := node.(ast.Expr)
		if !ok || !isStringRoot(expr, stack) {
			return
		}
		text := literalText(expr)
		if !leaseTable.MatchString(text) {
			return
		}
		if secondOnlyTime.MatchString(text) {
			report(expr.Pos(), "lease/membership query uses second-precision datetime('now', ...)")
		}
		if !strings.Contains(text, "CURRENT_TIMESTAMP") {
			return
		}
		parent := parentOf(stack)
		if isClockHelperCall(parent) {
			return
		}
		if name, ok := assignedName(parent, expr); ok {
			queryVars[name] = expr.Pos()
			return
		}
		report(expr.Pos(), "lease/membership query reads CURRENT_TIMESTAMP without %s", clockHelper)
	})
	if len(queryVars) == 0 {
		return
	}
	wrapped, misused := map[string]bool{}, map[string]bool{}
	walk(scope, func(node ast.Node, stack []ast.Node) {
		ident, ok := node.(*ast.Ident)
		if !ok {
			return
		}
		if _, tracked := queryVars[ident.Name]; !tracked {
			return
		}
		parent := parentOf(stack)
		switch {
		case isClockHelperCall(parent):
			wrapped[ident.Name] = true
		case isAssignTarget(parent, ident):
		default:
			misused[ident.Name] = true
			report(
				ident.Pos(),
				"%q holds a lease/membership query reading CURRENT_TIMESTAMP but is used without %s",
				ident.Name,
				clockHelper,
			)
		}
	})
	for name, pos := range queryVars {
		if !wrapped[name] && !misused[name] {
			report(pos, "%q holds a lease/membership query reading CURRENT_TIMESTAMP but never reaches %s",
				name, clockHelper)
		}
	}
}

// walk visits nodes depth-first and hands each one its ancestors, skipping parentheses.
func walk(root ast.Node, visit func(ast.Node, []ast.Node)) {
	var stack []ast.Node
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		visit(node, stack)
		stack = append(stack, node)
		return true
	})
}

func parentOf(stack []ast.Node) ast.Node {
	for i := len(stack) - 1; i >= 0; i-- {
		if _, paren := stack[i].(*ast.ParenExpr); !paren {
			return stack[i]
		}
	}
	return nil
}

// isStringRoot reports whether expr is the outermost expression of a string literal or of a
// `+` concatenation, so each query is examined once and as a whole.
func isStringRoot(expr ast.Expr, stack []ast.Node) bool {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return false
		}
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return false
		}
	default:
		return false
	}
	parent, ok := parentOf(stack).(*ast.BinaryExpr)
	return !ok || parent.Op != token.ADD
}

// literalText joins the string literals of a concatenation; non-literal operands become a space.
func literalText(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			if text, err := strconv.Unquote(e.Value); err == nil {
				return text
			}
		}
	case *ast.ParenExpr:
		return literalText(e.X)
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return literalText(e.X) + literalText(e.Y)
		}
	}
	return " "
}

func isClockHelperCall(node ast.Node) bool {
	call, ok := node.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		return fun.Sel.Name == clockHelper
	case *ast.Ident:
		return fun.Name == clockHelper
	}
	return false
}

// assignedName returns the local variable that receives expr in `name := expr`, `name = expr`,
// `name += expr`, or `var name = expr`.
func assignedName(parent ast.Node, expr ast.Expr) (string, bool) {
	switch p := parent.(type) {
	case *ast.AssignStmt:
		for i, rhs := range p.Rhs {
			if unparen(rhs) != expr || i >= len(p.Lhs) {
				continue
			}
			if ident, ok := p.Lhs[i].(*ast.Ident); ok {
				return ident.Name, true
			}
		}
	case *ast.ValueSpec:
		for i, value := range p.Values {
			if unparen(value) == expr && i < len(p.Names) {
				return p.Names[i].Name, true
			}
		}
	}
	return "", false
}

func isAssignTarget(parent ast.Node, ident *ast.Ident) bool {
	switch p := parent.(type) {
	case *ast.AssignStmt:
		for _, lhs := range p.Lhs {
			if lhs == ident {
				return true
			}
		}
	case *ast.ValueSpec:
		for _, name := range p.Names {
			if name == ident {
				return true
			}
		}
	}
	return false
}

func unparen(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = paren.X
	}
}
