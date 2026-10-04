package goja

import (
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/token"
	"github.com/dop251/goja/unistring"
)

// Reserved parameter and inner-function names injected by the transform. They
// start with a space so they can never equal a legal JavaScript identifier.
const (
	paramExports  = unistring.String(" exports")
	paramImports  = unistring.String(" imports")
	evaluatorName = unistring.String(" evaluate")
)

// transformModule rewrites a parsed module into an ordinary strict program
// that implements ES module semantics on top of the existing compiler, without
// changing the VM. The emitted program evaluates one expression:
//
//	(function (exports, imports) {
//	  "use strict";
//	  /* hoisted top-level function declarations */
//	  /* var declarations (hoisted) */
//	  /* lexical declarations (let, TDZ until evaluation) */
//	  /* exports["f"] = f  for hoisted exported functions */
//	  function evaluate() {
//	    "use strict";
//	    /* module body: initialisers, statements, references rewritten */
//	  }
//	  return evaluate;
//	})()
//
// The program does not call the wrapper: the runtime evaluates it once to
// obtain the wrapper function (no module code runs), then at evaluation calls
// wrapper(cell, importView), which performs function/var instantiation and
// hoisted export publication and returns the evaluator, which is immediately
// invoked. Because exports and imports are parameters of the outer wrapper,
// hoisted functions close over them just like spec module functions close over
// the module environment record.
//
// Because the evaluator and every hoisted top-level function are nested in the
// same outer function scope, hoisted functions that read lexical bindings
// during a circular import observe the same TDZ / values as spec modules.
//
// Reference rewriting:
//
//   - imported binding `a`  -> imports["a"]   (getter-only, live, TDZ-safe)
//   - exported local `b`     -> exports["b"]  (getter/setter, live, TDZ-safe)
//   - other module locals    -> left as-is (they live in the outer scope)
func (m *moduleRecord) buildProgram() (*ast.Program, error) {
	t := newModuleTransformer(m)
	return t.transform()
}

type moduleTransformer struct {
	mod *moduleRecord

	scope *mScope

	// exported maps an exported public name to its slot kind.
	exported map[unistring.String]bindingKind
	imported map[unistring.String]bool
	locals   map[unistring.String]bindingKind

	// clause re-exports of an imported binding (no "from"): public name -> true.
	indirectClause map[unistring.String]bool

	outer []ast.Statement // hoisted decls + publish writes (instantiation)
	body  []ast.Statement // evaluator body (evaluation)

	// local names already declared in the outer scope.
	declaredOuter map[unistring.String]bool
}

func newModuleTransformer(m *moduleRecord) *moduleTransformer {
	t := &moduleTransformer{
		mod:            m,
		exported:       make(map[unistring.String]bindingKind),
		imported:       make(map[unistring.String]bool),
		locals:         collectTopLevelDeclaredNames(m.program),
		declaredOuter:  make(map[unistring.String]bool),
		indirectClause: make(map[unistring.String]bool),
	}
	for _, le := range m.meta.localExportOrder {
		t.exported[le.name] = le.kind
	}
	for _, imp := range m.meta.imports {
		if imp.defaults != "" {
			t.imported[imp.defaults] = true
		}
		for _, n := range imp.named {
			t.imported[n.local] = true
		}
	}
	for _, ci := range m.meta.clauseImportSpec {
		t.indirectClause[ci.exported] = true
	}
	return t
}

func (t *moduleTransformer) transform() (*ast.Program, error) {
	// First pass: hoist declarations into the outer scope in source order.
	for _, stmt := range t.mod.program.Body {
		t.hoistTop(stmt)
	}

	// Second pass: build the evaluator body.
	t.push(scopeModule, nil)
	for _, stmt := range t.mod.program.Body {
		t.transformTopStatement(stmt)
	}
	t.pop()

	inner := functionLiteral(nil, t.body)
	inner.Name = ident(evaluatorName)
	inner.DeclarationList = collectVarDeclarations(t.body)

	outer := append([]ast.Statement{}, t.outer...)
	outer = append(outer,
		&ast.FunctionDeclaration{Function: inner},
		&ast.ReturnStatement{Argument: ident(evaluatorName)},
	)
	wrapper := functionLiteral([]*ast.Binding{
		{Target: ident(paramExports)},
		{Target: ident(paramImports)},
	}, outer)
	wrapper.DeclarationList = collectVarDeclarations(outer)

	return &ast.Program{
		Body: []ast.Statement{
			exprStmt(wrapper),
		},
		File: t.mod.program.File,
	}, nil
}

// declareOuter adds a var/let declaration to the instantiation wrapper. var
// declarations are hoisted and initialised to undefined; let declarations
// establish the outer binding but are represented as `var` here, with TDZ for
// exported lexical bindings enforced by the cell accessors.
func (t *moduleTransformer) declareOuter(name unistring.String) {
	if name == "" || t.declaredOuter[name] {
		return
	}
	t.declaredOuter[name] = true
	t.outer = append(t.outer, &ast.VariableStatement{
		List: []*ast.Binding{{Target: ident(name)}},
	})
}

// ---- tiny AST builders ----

func ident(name unistring.String) *ast.Identifier {
	return &ast.Identifier{Name: name}
}

func strLit(s unistring.String) *ast.StringLiteral {
	return &ast.StringLiteral{Literal: s.String(), Value: s}
}

func member(obj, prop unistring.String) ast.Expression {
	return &ast.BracketExpression{
		Left:   ident(obj),
		Member: strLit(prop),
	}
}

func exprStmt(e ast.Expression) *ast.ExpressionStatement {
	return &ast.ExpressionStatement{Expression: e}
}

func assignStmt(obj, prop unistring.String, value ast.Expression) ast.Statement {
	return exprStmt(&ast.AssignExpression{
		Operator: token.ASSIGN,
		Left:     member(obj, prop),
		Right:    value,
	})
}

func localAssignStmt(name unistring.String, value ast.Expression) ast.Statement {
	return exprStmt(&ast.AssignExpression{
		Operator: token.ASSIGN,
		Left:     ident(name),
		Right:    value,
	})
}

func strictDirective() ast.Statement {
	return exprStmt(strLit("use strict"))
}

func functionLiteral(params []*ast.Binding, body []ast.Statement) *ast.FunctionLiteral {
	pl := &ast.ParameterList{}
	pl.List = params
	list := append([]ast.Statement{strictDirective()}, body...)
	return &ast.FunctionLiteral{
		ParameterList: pl,
		Body:          &ast.BlockStatement{List: list},
	}
}

var _ = token.ASSIGN
