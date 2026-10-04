package goja

import (
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/unistring"
)

// rewriteStmt returns a structurally equal statement with module-level import
// and export references rewritten to live-binding accessor reads/writes.
func (t *moduleTransformer) rewriteStmt(s ast.Statement) ast.Statement {
	if s == nil {
		return nil
	}
	switch s := s.(type) {
	case *ast.BlockStatement:
		names := collectBlockLexicalNames(s.List)
		t.push(scopeBlock, names)
		list := t.rewriteStmtList(s.List)
		t.pop()
		return &ast.BlockStatement{LeftBrace: s.LeftBrace, List: list, RightBrace: s.RightBrace}

	case *ast.ExpressionStatement:
		return &ast.ExpressionStatement{Expression: t.rewriteExpr(s.Expression)}

	case *ast.VariableStatement:
		return &ast.VariableStatement{Var: s.Var, List: t.rewriteBindingList(s.List)}

	case *ast.LexicalDeclaration:
		return &ast.LexicalDeclaration{
			Idx: s.Idx, Token: s.Token, List: t.rewriteBindingList(s.List),
		}

	case *ast.FunctionDeclaration:
		return &ast.FunctionDeclaration{Function: t.rewriteFunction(s.Function)}

	case *ast.ClassDeclaration:
		return &ast.ClassDeclaration{Class: t.rewriteClass(s.Class)}

	case *ast.ReturnStatement:
		return &ast.ReturnStatement{Return: s.Return, Argument: t.rewriteNullable(s.Argument)}

	case *ast.ThrowStatement:
		return &ast.ThrowStatement{Throw: s.Throw, Argument: t.rewriteExpr(s.Argument)}

	case *ast.IfStatement:
		return &ast.IfStatement{
			If: s.If, Test: t.rewriteExpr(s.Test),
			Consequent: t.rewriteStmt(s.Consequent), Alternate: t.rewriteStmt(s.Alternate),
		}

	case *ast.WhileStatement:
		return &ast.WhileStatement{While: s.While, Test: t.rewriteExpr(s.Test), Body: t.rewriteStmt(s.Body)}

	case *ast.DoWhileStatement:
		return &ast.DoWhileStatement{
			Do: s.Do, Test: t.rewriteExpr(s.Test), Body: t.rewriteStmt(s.Body),
			RightParenthesis: s.RightParenthesis,
		}

	case *ast.ForStatement:
		return t.rewriteFor(s)

	case *ast.ForInStatement:
		return t.rewriteForIn(s)

	case *ast.ForOfStatement:
		return t.rewriteForOf(s)

	case *ast.TryStatement:
		return t.rewriteTry(s)

	case *ast.SwitchStatement:
		return t.rewriteSwitch(s)

	case *ast.LabelledStatement:
		return &ast.LabelledStatement{
			Label: s.Label, Colon: s.Colon, Statement: t.rewriteStmt(s.Statement),
		}

	case *ast.BranchStatement:
		return s
	case *ast.DebuggerStatement:
		return s
	case *ast.EmptyStatement:
		return s
	case *ast.WithStatement:
		return &ast.WithStatement{With: s.With, Object: t.rewriteExpr(s.Object), Body: t.rewriteStmt(s.Body)}
	}
	return s
}

func (t *moduleTransformer) rewriteStmtList(list []ast.Statement) []ast.Statement {
	out := make([]ast.Statement, 0, len(list))
	for _, st := range list {
		out = append(out, t.rewriteStmt(st))
	}
	return out
}

func (t *moduleTransformer) rewriteBindingList(list []*ast.Binding) []*ast.Binding {
	out := make([]*ast.Binding, 0, len(list))
	for _, b := range list {
		out = append(out, &ast.Binding{
			Target:      t.rewritePatternTarget(b.Target),
			Initializer: t.rewriteNullable(b.Initializer),
		})
	}
	return out
}

// rewritePatternTarget rewrites initialiser/default expressions inside a
// binding pattern without treating the bound identifiers as reads.
func (t *moduleTransformer) rewritePatternTarget(target ast.BindingTarget) ast.BindingTarget {
	switch p := target.(type) {
	case *ast.Identifier:
		return p // a binding, not a reference
	case *ast.ObjectPattern:
		np := &ast.ObjectPattern{
			LeftBrace: p.LeftBrace, RightBrace: p.RightBrace,
			Rest: t.rewritePatternTargetOrNil(p.Rest),
		}
		for _, prop := range p.Properties {
			switch pr := prop.(type) {
			case *ast.PropertyShort:
				np.Properties = append(np.Properties, &ast.PropertyShort{
					Name:        pr.Name,
					Initializer: t.rewriteNullable(pr.Initializer),
				})
			case *ast.PropertyKeyed:
				np.Properties = append(np.Properties, &ast.PropertyKeyed{
					Key:      t.rewriteExpr(pr.Key),
					Kind:     pr.Kind,
					Value:    t.rewritePatternValue(pr.Value),
					Computed: pr.Computed,
				})
			}
		}
		return np
	case *ast.ArrayPattern:
		np := &ast.ArrayPattern{
			LeftBracket: p.LeftBracket, RightBracket: p.RightBracket,
			Elements: make([]ast.Expression, len(p.Elements)),
			Rest:     t.rewritePatternTargetOrNil(p.Rest),
		}
		for i, el := range p.Elements {
			if el == nil {
				continue
			}
			if b, ok := el.(*ast.Binding); ok {
				np.Elements[i] = &ast.Binding{
					Target:      t.rewritePatternTarget(b.Target),
					Initializer: t.rewriteNullable(b.Initializer),
				}
				continue
			}
			np.Elements[i] = t.rewriteExpr(el.(ast.Expression))
		}
		return np
	}
	return target
}

func (t *moduleTransformer) rewritePatternTargetOrNil(e ast.Expression) ast.Expression {
	if e == nil {
		return nil
	}
	if bt, ok := e.(ast.BindingTarget); ok {
		return t.rewritePatternTarget(bt).(ast.Expression)
	}
	return e
}

func (t *moduleTransformer) rewritePatternValue(v ast.Expression) ast.Expression {
	if b, ok := v.(*ast.Binding); ok {
		return &ast.Binding{
			Target:      t.rewritePatternTarget(b.Target),
			Initializer: t.rewriteNullable(b.Initializer),
		}
	}
	return t.rewriteExpr(v)
}

func (t *moduleTransformer) rewriteFor(s *ast.ForStatement) ast.Statement {
	var init ast.ForLoopInitializer
	scopeNames := map[unistring.String]bool{}
	switch i := s.Initializer.(type) {
	case *ast.ForLoopInitializerExpression:
		init = &ast.ForLoopInitializerExpression{Expression: t.rewriteExpr(i.Expression)}
	case *ast.ForLoopInitializerVarDeclList:
		init = &ast.ForLoopInitializerVarDeclList{Var: i.Var, List: t.rewriteBindingList(i.List)}
		for _, b := range i.List {
			forEachBindingName(b.Target, func(n unistring.String) { scopeNames[n] = true })
		}
	case *ast.ForLoopInitializerLexicalDecl:
		nd := &ast.LexicalDeclaration{
			Idx: i.LexicalDeclaration.Idx, Token: i.LexicalDeclaration.Token,
			List: t.rewriteBindingList(i.LexicalDeclaration.List),
		}
		init = &ast.ForLoopInitializerLexicalDecl{LexicalDeclaration: *nd}
		for _, b := range i.LexicalDeclaration.List {
			forEachBindingName(b.Target, func(n unistring.String) { scopeNames[n] = true })
		}
	}
	t.push(scopeBlock, scopeNames)
	body := t.rewriteStmt(s.Body)
	test := t.rewriteNullable(s.Test)
	update := t.rewriteNullable(s.Update)
	t.pop()
	return &ast.ForStatement{
		For: s.For, Initializer: init, Test: test, Update: update, Body: body,
	}
}

func (t *moduleTransformer) rewriteForIn(s *ast.ForInStatement) ast.Statement {
	names := map[unistring.String]bool{}
	into := t.rewriteForInto(s.Into, names)
	t.push(scopeBlock, names)
	body := t.rewriteStmt(s.Body)
	t.pop()
	return &ast.ForInStatement{
		For: s.For, Into: into, Source: t.rewriteExpr(s.Source), Body: body,
	}
}

func (t *moduleTransformer) rewriteForOf(s *ast.ForOfStatement) ast.Statement {
	names := map[unistring.String]bool{}
	into := t.rewriteForInto(s.Into, names)
	t.push(scopeBlock, names)
	body := t.rewriteStmt(s.Body)
	t.pop()
	return &ast.ForOfStatement{
		For: s.For, Into: into, Source: t.rewriteExpr(s.Source), Body: body, Await: s.Await,
	}
}

func (t *moduleTransformer) rewriteForInto(into ast.ForInto, names map[unistring.String]bool) ast.ForInto {
	switch i := into.(type) {
	case *ast.ForIntoVar:
		forEachBindingName(i.Binding.Target, func(n unistring.String) { names[n] = true })
		return &ast.ForIntoVar{
			Binding: &ast.Binding{
				Target:      t.rewritePatternTarget(i.Binding.Target),
				Initializer: t.rewriteNullable(i.Binding.Initializer),
			},
		}
	case *ast.ForDeclaration:
		if id, ok := i.Target.(*ast.Identifier); ok {
			names[id.Name] = true
		}
		return &ast.ForDeclaration{Idx: i.Idx, IsConst: i.IsConst, Target: t.rewritePatternTarget(i.Target)}
	case *ast.ForIntoExpression:
		return &ast.ForIntoExpression{Expression: t.rewriteExpr(i.Expression)}
	}
	return into
}

func (t *moduleTransformer) rewriteTry(s *ast.TryStatement) ast.Statement {
	out := &ast.TryStatement{
		Try: s.Try,
		Body: &ast.BlockStatement{
			LeftBrace: s.Body.LeftBrace,
			List:      t.rewriteStmtList(s.Body.List),
		},
	}
	if s.Catch != nil {
		names := map[unistring.String]bool{}
		var param ast.BindingTarget
		if s.Catch.Parameter != nil {
			forEachBindingName(s.Catch.Parameter, func(n unistring.String) { names[n] = true })
			param = t.rewritePatternTarget(s.Catch.Parameter)
		}
		t.push(scopeBlock, names)
		out.Catch = &ast.CatchStatement{
			Catch:     s.Catch.Catch,
			Parameter: param,
			Body: &ast.BlockStatement{
				LeftBrace: s.Catch.Body.LeftBrace,
				List:      t.rewriteStmtList(s.Catch.Body.List),
			},
		}
		t.pop()
	}
	if s.Finally != nil {
		out.Finally = &ast.BlockStatement{
			LeftBrace: s.Finally.LeftBrace,
			List:      t.rewriteStmtList(s.Finally.List),
		}
	}
	return out
}

func (t *moduleTransformer) rewriteSwitch(s *ast.SwitchStatement) ast.Statement {
	names := map[unistring.String]bool{}
	for _, c := range s.Body {
		for _, st := range c.Consequent {
			collectLexicalNamesStmt(st, names)
		}
	}
	t.push(scopeBlock, names)
	cases := make([]*ast.CaseStatement, len(s.Body))
	for i, c := range s.Body {
		cases[i] = &ast.CaseStatement{
			Case:       c.Case,
			Test:       t.rewriteNullable(c.Test),
			Consequent: t.rewriteStmtList(c.Consequent),
		}
	}
	t.pop()
	return &ast.SwitchStatement{
		Switch: s.Switch, Discriminant: t.rewriteExpr(s.Discriminant),
		Default: s.Default, Body: cases, RightBrace: s.RightBrace,
	}
}

// collectBlockLexicalNames gathers let/const/class names declared directly in a
// statement list (not inside nested blocks/functions) for block scoping.
func collectBlockLexicalNames(list []ast.Statement) map[unistring.String]bool {
	names := map[unistring.String]bool{}
	for _, s := range list {
		collectLexicalNamesStmt(s, names)
	}
	return names
}

func collectLexicalNamesStmt(s ast.Statement, names map[unistring.String]bool) {
	switch s := s.(type) {
	case *ast.LexicalDeclaration:
		for _, b := range s.List {
			forEachBindingName(b.Target, func(n unistring.String) { names[n] = true })
		}
	case *ast.ClassDeclaration:
		if s.Class.Name != nil {
			names[s.Class.Name.Name] = true
		}
	}
}
