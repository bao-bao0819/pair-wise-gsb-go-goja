package goja

import (
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/unistring"
)

// collectVarDeclarations gathers every var declaration reachable from a
// function body without entering nested functions, mirroring the list the
// parser normally attaches to FunctionLiteral.DeclarationList. The compiler
// uses it to allocate function-scope var bindings; without it, var references
// are mistaken for globals.
func collectVarDeclarations(body []ast.Statement) []*ast.VariableDeclaration {
	var out []*ast.VariableDeclaration
	var walk func(s ast.Statement)
	walk = func(s ast.Statement) {
		switch s := s.(type) {
		case *ast.VariableStatement:
			out = append(out, &ast.VariableDeclaration{Var: s.Var, List: s.List})
		case *ast.FunctionDeclaration, *ast.ClassDeclaration:
			return
		case *ast.BlockStatement:
			for _, st := range s.List {
				walk(st)
			}
		case *ast.IfStatement:
			walk(s.Consequent)
			walk(s.Alternate)
		case *ast.ForStatement:
			if v, ok := s.Initializer.(*ast.ForLoopInitializerVarDeclList); ok {
				out = append(out, &ast.VariableDeclaration{Var: v.Var, List: v.List})
			}
			// lexical (let/const) for-loop bindings are block scoped, not hoisted.
			walk(s.Body)
		case *ast.ForInStatement:
			// for-in/for-of lexical targets are block scoped.
			walk(s.Body)
		case *ast.ForOfStatement:
			walk(s.Body)
		case *ast.WhileStatement:
			walk(s.Body)
		case *ast.DoWhileStatement:
			walk(s.Body)
		case *ast.TryStatement:
			for _, st := range s.Body.List {
				walk(st)
			}
			if s.Catch != nil {
				for _, st := range s.Catch.Body.List {
					walk(st)
				}
			}
			if s.Finally != nil {
				for _, st := range s.Finally.List {
					walk(st)
				}
			}
		case *ast.SwitchStatement:
			for _, c := range s.Body {
				for _, st := range c.Consequent {
					walk(st)
				}
			}
		case *ast.LabelledStatement:
			walk(s.Statement)
		case *ast.WithStatement:
			walk(s.Body)
		}
	}
	for _, s := range body {
		walk(s)
	}
	return out
}

// syntheticDefault is the hidden local name used for anonymous
// `export default function/class` so the declaration can hoist normally.
const syntheticDefault = unistring.String("*default*")

// hoistTop performs instantiation-time declaration hoisting for module-scope
// statements, emitting declarations and exported-function publication into the
// outer wrapper. Evaluator statements are emitted in the second pass.
func (t *moduleTransformer) hoistTop(stmt ast.Statement) {
	switch s := stmt.(type) {
	case *ast.ImportDeclaration:
		// no local declarations; bindings come from the imports parameter.

	case *ast.ExportNamedDeclaration:
		if s.Module != nil {
			return // re-export, linked via accessors
		}
		if s.Declaration != nil {
			t.hoistDeclaration(s.Declaration, true)
			return
		}
		// export { a as b }: nothing to hoist (local must exist elsewhere).

	case *ast.ExportDefaultDeclaration:
		t.hoistDefault(s)

	case *ast.FunctionDeclaration:
		t.hoistDeclaration(s, false)

	case *ast.VariableStatement:
		t.hoistVar(s)

	case *ast.LexicalDeclaration:
		t.hoistLex(s)

	case *ast.ClassDeclaration:
		// Non-exported class: outer binding exists (TDZ emulation handled for
		// exported names by cells; for a local class read before its statement
		// the outer var is simply undefined).
		if s.Class.Name != nil {
			t.declareOuter(s.Class.Name.Name)
		}
	}
}

// hoistDeclaration handles a declaration preceded by `export`.
func (t *moduleTransformer) hoistDeclaration(decl ast.Statement, exported bool) {
	switch d := decl.(type) {
	case *ast.FunctionDeclaration:
		name := d.Function.Name
		if name == nil {
			return
		}
		fn := t.rewriteFunction(d.Function)
		t.outer = append(t.outer, &ast.FunctionDeclaration{Function: fn})
		if exported {
			t.publishHoisted(name.Name, name.Name)
		}
	case *ast.VariableStatement:
		t.hoistVar(d)
	case *ast.LexicalDeclaration:
		t.hoistLex(d)
	case *ast.ClassDeclaration:
		if d.Class.Name != nil {
			t.declareOuter(d.Class.Name.Name)
		}
	}
}

// publishHoisted emits exports[publicName] = localName in the outer wrapper,
// running at instantiation. This is what makes exported functions available to
// circular importers before evaluation reaches their declaration.
func (t *moduleTransformer) publishHoisted(publicName, localName unistring.String) {
	t.outer = append(t.outer, assignStmt(paramExports, publicName, ident(localName)))
}

func (t *moduleTransformer) hoistVar(s *ast.VariableStatement) {
	for _, b := range s.List {
		forEachBindingName(b.Target, func(n unistring.String) {
			t.declareOuter(n)
		})
	}
}

func (t *moduleTransformer) hoistLex(s *ast.LexicalDeclaration) {
	for _, b := range s.List {
		forEachBindingName(b.Target, func(n unistring.String) {
			t.declareOuter(n)
		})
	}
}

func (t *moduleTransformer) hoistDefault(s *ast.ExportDefaultDeclaration) {
	switch d := s.Declaration.(type) {
	case *ast.FunctionDeclaration:
		if d.Function.Name != nil {
			t.outer = append(t.outer, &ast.FunctionDeclaration{
				Function: t.rewriteFunction(d.Function),
			})
			t.publishHoisted(defaultExportName, d.Function.Name.Name)
		} else {
			// Anonymous default function: hoist under a synthetic name.
			fn := t.rewriteFunction(d.Function)
			fn.Name = ident(syntheticDefault)
			t.outer = append(t.outer, &ast.FunctionDeclaration{Function: fn})
			t.publishHoisted(defaultExportName, syntheticDefault)
		}
	case *ast.ClassDeclaration:
		// Classes are not hoisted; the outer binding is just created.
		if d.Class.Name != nil {
			t.declareOuter(d.Class.Name.Name)
		}
		// Anonymous default class needs a synthetic outer binding too.
		t.declareOuter(syntheticDefault)
	}
}

// forEachBindingName invokes fn for every identifier bound by a target. Only
// identifier targets are permitted in exported var/let/const (patterns are
// rejected by the parser), but patterns may appear in non-exported decls.
func forEachBindingName(target ast.BindingTarget, fn func(unistring.String)) {
	switch tgt := target.(type) {
	case *ast.Identifier:
		fn(tgt.Name)
	case *ast.ObjectPattern:
		for _, p := range tgt.Properties {
			switch p := p.(type) {
			case *ast.PropertyShort:
				fn(p.Name.Name)
			case *ast.PropertyKeyed:
				forEachBindingElement(p.Value, fn)
			}
		}
		if tgt.Rest != nil {
			if id, ok := tgt.Rest.(*ast.Identifier); ok {
				fn(id.Name)
			}
		}
	case *ast.ArrayPattern:
		for _, el := range tgt.Elements {
			if el != nil {
				forEachBindingElement(el, fn)
			}
		}
		if tgt.Rest != nil {
			if id, ok := tgt.Rest.(*ast.Identifier); ok {
				fn(id.Name)
			}
		}
	}
}

func forEachBindingElement(el ast.Expression, fn func(unistring.String)) {
	switch e := el.(type) {
	case *ast.Identifier:
		fn(e.Name)
	case ast.Pattern:
		_ = e.(ast.BindingTarget)
		forEachBindingName(e.(ast.BindingTarget), fn)
	case *ast.AssignExpression:
		forEachBindingElement(e.Left, fn)
	}
}
