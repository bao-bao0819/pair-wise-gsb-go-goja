package goja

import (
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/token"
	"github.com/dop251/goja/unistring"
)

// ---- scope tracking ----

type scopeKind byte

const (
	scopeModule scopeKind = iota
	scopeFunction
	scopeBlock
)

type mScope struct {
	kind   scopeKind
	parent *mScope
	names  map[unistring.String]bool
}

func (t *moduleTransformer) push(kind scopeKind, names map[unistring.String]bool) {
	t.scope = &mScope{kind: kind, parent: t.scope, names: names}
}

func (t *moduleTransformer) pop() { t.scope = t.scope.parent }

// innerShadows reports whether name is bound by a scope nested between the
// reference site and the module scope, i.e. whether a module import/export is
// shadowed there.
func (t *moduleTransformer) innerShadows(name unistring.String) bool {
	for cur := t.scope; cur != nil && cur.kind != scopeModule; cur = cur.parent {
		if cur.names[name] {
			return true
		}
		if cur.kind == scopeFunction {
			return false
		}
	}
	return false
}

// resolveBinding returns the backing object for a module-level import/export.
func (t *moduleTransformer) resolveBinding(name unistring.String) (obj unistring.String, ok bool) {
	if t.scope.kind != scopeModule && t.innerShadows(name) {
		return "", false
	}
	if t.imported[name] {
		return paramImports, true
	}
	if _, ex := t.exported[name]; ex {
		return paramExports, true
	}
	return "", false
}

// rewriteRef maps a bare identifier to its live-binding read expression.
func (t *moduleTransformer) rewriteRef(name unistring.String) ast.Expression {
	if obj, ok := t.resolveBinding(name); ok {
		return member(obj, name)
	}
	return ident(name)
}

// isWriteTarget reports whether id, in expression e, appears as an assignment
// target that must be redirected to the backing cell.
//
// Rather than detect this at the identifier, assignments are handled at their
// AssignExpression/Update node: a plain Identifier left-hand side is rewritten
// to a member expression directly.

// ---- function / class scope handling ----

func functionParamNames(fl *ast.FunctionLiteral) map[unistring.String]bool {
	names := make(map[unistring.String]bool)
	if fl.ParameterList != nil {
		for _, p := range fl.ParameterList.List {
			forEachBindingName(p.Target, func(n unistring.String) { names[n] = true })
		}
		if fl.ParameterList.Rest != nil {
			if id, ok := fl.ParameterList.Rest.(*ast.Identifier); ok {
				names[id.Name] = true
			}
		}
	}
	return names
}

func (t *moduleTransformer) rewriteFunction(fl *ast.FunctionLiteral) *ast.FunctionLiteral {
	params := functionParamNames(fl)
	// Add var/function names visible in the whole function scope so that
	// references to them are not rewritten as module bindings.
	if fl.DeclarationList != nil {
		for _, vd := range fl.DeclarationList {
			for _, b := range vd.List {
				forEachBindingName(b.Target, func(n unistring.String) { params[n] = true })
			}
		}
	}
	collectFunctionScopedNames(fl.Body, params)
	if fl.Name != nil {
		params[fl.Name.Name] = true
	}
	t.push(scopeFunction, params)

	var bodyList []ast.Statement
	for _, st := range fl.Body.List {
		bodyList = append(bodyList, t.rewriteStmt(st))
	}
	t.pop()

	out := *fl
	out.Body = &ast.BlockStatement{LeftBrace: fl.Body.LeftBrace, List: bodyList}
	return &out
}

func (t *moduleTransformer) rewriteArrow(al *ast.ArrowFunctionLiteral) *ast.ArrowFunctionLiteral {
	params := map[unistring.String]bool{}
	if al.ParameterList != nil {
		for _, p := range al.ParameterList.List {
			forEachBindingName(p.Target, func(n unistring.String) { params[n] = true })
		}
		if al.ParameterList.Rest != nil {
			if id, ok := al.ParameterList.Rest.(*ast.Identifier); ok {
				params[id.Name] = true
			}
		}
	}
	if al.DeclarationList != nil {
		for _, vd := range al.DeclarationList {
			for _, b := range vd.List {
				forEachBindingName(b.Target, func(n unistring.String) { params[n] = true })
			}
		}
	}
	switch b := al.Body.(type) {
	case *ast.BlockStatement:
		collectFunctionScopedNames(b, params)
	}
	t.push(scopeFunction, params)

	out := *al
	switch b := al.Body.(type) {
	case *ast.BlockStatement:
		var list []ast.Statement
		for _, st := range b.List {
			list = append(list, t.rewriteStmt(st))
		}
		out.Body = &ast.BlockStatement{LeftBrace: b.LeftBrace, List: list}
	case *ast.ExpressionBody:
		out.Body = &ast.ExpressionBody{Expression: t.rewriteExpr(b.Expression)}
	}
	t.pop()
	return &out
}

func (t *moduleTransformer) rewriteClass(cl *ast.ClassLiteral) *ast.ClassLiteral {
	out := *cl
	if cl.SuperClass != nil {
		out.SuperClass = t.rewriteExpr(cl.SuperClass)
	}
	classNames := map[unistring.String]bool{}
	if cl.Name != nil {
		classNames[cl.Name.Name] = true
	}
	t.push(scopeFunction, classNames) // class body has its own function-like scope
	var body []ast.ClassElement
	for _, el := range cl.Body {
		body = append(body, t.rewriteClassElement(el))
	}
	t.pop()
	out.Body = body
	return &out
}

func (t *moduleTransformer) rewriteClassElement(el ast.ClassElement) ast.ClassElement {
	switch e := el.(type) {
	case *ast.FieldDefinition:
		nf := *e
		nf.Key = t.rewriteExpr(e.Key)
		nf.Initializer = t.rewriteNullable(e.Initializer)
		return &nf
	case *ast.MethodDefinition:
		nm := *e
		nm.Key = t.rewriteExpr(e.Key)
		nm.Body = t.rewriteFunction(e.Body)
		return &nm
	case *ast.ClassStaticBlock:
		names := map[unistring.String]bool{}
		if e.DeclarationList != nil {
			for _, vd := range e.DeclarationList {
				for _, b := range vd.List {
					forEachBindingName(b.Target, func(n unistring.String) { names[n] = true })
				}
			}
		}
		collectFunctionScopedNames(e.Block, names)
		t.push(scopeFunction, names)
		var list []ast.Statement
		for _, st := range e.Block.List {
			list = append(list, t.rewriteStmt(st))
		}
		t.pop()
		ncb := *e
		ncb.Block = &ast.BlockStatement{LeftBrace: e.Block.LeftBrace, List: list}
		return &ncb
	}
	return el
}

// collectFunctionScopedNames adds to names every var/function/parameter-level
// binding reachable in the function body (without entering nested functions),
// so that they shadow module bindings for reference rewriting.
func collectFunctionScopedNames(body *ast.BlockStatement, names map[unistring.String]bool) {
	if body == nil {
		return
	}
	var walkStmt func(ast.Statement, func(ast.Statement))
	walkBlock := func(b *ast.BlockStatement) {
		for _, s := range b.List {
			walkStmt(s, nil)
		}
	}
	_ = walkBlock

	var visitStmt func(s ast.Statement, blockLexical map[unistring.String]bool, top bool)
	visitStmt = func(s ast.Statement, blockLexical map[unistring.String]bool, top bool) {
		switch s := s.(type) {
		case *ast.VariableStatement:
			for _, b := range s.List {
				forEachBindingName(b.Target, func(n unistring.String) { names[n] = true })
			}
		case *ast.FunctionDeclaration:
			if s.Function.Name != nil {
				names[s.Function.Name.Name] = true
			}
		case *ast.ClassDeclaration:
			if top {
				if s.Class.Name != nil {
					names[s.Class.Name.Name] = true
				}
			}
		case *ast.LexicalDeclaration:
			for _, b := range s.List {
				forEachBindingName(b.Target, func(n unistring.String) {
					if top {
						names[n] = true
					}
				})
			}
		case *ast.BlockStatement:
			for _, st := range s.List {
				visitStmt(st, nil, top)
			}
		case *ast.IfStatement:
			visitStmt(s.Consequent, nil, top)
			visitStmt(s.Alternate, nil, top)
		case *ast.ForStatement:
			visitStmt(s.Body, nil, top)
		case *ast.ForInStatement:
			visitStmt(s.Body, nil, top)
		case *ast.ForOfStatement:
			visitStmt(s.Body, nil, top)
		case *ast.WhileStatement:
			visitStmt(s.Body, nil, top)
		case *ast.DoWhileStatement:
			visitStmt(s.Body, nil, top)
		case *ast.TryStatement:
			for _, st := range s.Body.List {
				visitStmt(st, nil, top)
			}
			if s.Catch != nil {
				for _, st := range s.Catch.Body.List {
					visitStmt(st, nil, top)
				}
			}
			if s.Finally != nil {
				for _, st := range s.Finally.List {
					visitStmt(st, nil, top)
				}
			}
		case *ast.SwitchStatement:
			for _, c := range s.Body {
				for _, st := range c.Consequent {
					visitStmt(st, nil, top)
				}
			}
		}
	}
	for _, s := range body.List {
		visitStmt(s, nil, true)
	}
}

var _ = token.ASSIGN
