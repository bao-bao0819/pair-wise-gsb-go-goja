package goja

import (
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/token"
	"github.com/dop251/goja/unistring"
)

// transformTopStatement emits evaluator-body statements. Declarations that
// were hoisted are reduced to their initialisers; import/re-export statements
// vanish; exports become writes to the export cell.
func (t *moduleTransformer) transformTopStatement(stmt ast.Statement) {
	switch s := stmt.(type) {
	case *ast.ImportDeclaration:
		return

	case *ast.ExportNamedDeclaration:
		if s.Module != nil {
			return // re-export handled entirely by linked accessors
		}
		if s.Declaration != nil {
			t.emitExportedDeclaration(s.Declaration)
			return
		}
		for _, sp := range s.List {
			if t.indirectClause[sp.Exported.Name] {
				continue // linked as an accessor, not written by the body
			}
			// export { local as exported }
			t.body = append(t.body,
				assignStmt(paramExports, sp.Exported.Name, t.readLocal(sp.Local.Name)),
			)
		}

	case *ast.ExportDefaultDeclaration:
		t.emitDefault(s)

	case *ast.FunctionDeclaration:
		// declaration hoisted; nothing to run at evaluation.

	case *ast.VariableStatement:
		t.emitVar(s)

	case *ast.LexicalDeclaration:
		t.emitLex(s)

	case *ast.ClassDeclaration:
		if s.Class.Name != nil {
			name := s.Class.Name.Name
			t.body = append(t.body, localAssignStmt(name, t.rewriteClass(s.Class)))
		} else {
			// anonymous class declaration statement is invalid; parser ensures named.
			t.body = append(t.body, t.rewriteStmt(s))
		}

	default:
		t.body = append(t.body, t.rewriteStmt(s))
	}
}

// readLocal returns a read expression for a module-scope name: an imported
// binding reads through imports; an exported name reads through exports; other
// locals read directly.
func (t *moduleTransformer) readLocal(name unistring.String) ast.Expression {
	if t.imported[name] {
		return member(paramImports, name)
	}
	if _, ex := t.exported[name]; ex {
		return member(paramExports, name)
	}
	return ident(name)
}

func (t *moduleTransformer) emitExportedDeclaration(decl ast.Statement) {
	switch d := decl.(type) {
	case *ast.VariableStatement:
		t.emitVar(d)
	case *ast.LexicalDeclaration:
		t.emitLex(d)
	case *ast.FunctionDeclaration:
		// hoisted and published during instantiation; nothing at evaluation.
	case *ast.ClassDeclaration:
		name := d.Class.Name.Name
		t.body = append(t.body, localAssignStmt(name, t.rewriteClass(d.Class)))
		t.body = append(t.body, assignStmt(paramExports, name, ident(name)))
	}
}

// emitVar turns each declarator into initialiser assignments. The outer `var`
// declarations already exist (hoisted); exports are written through the cell so
// live bindings propagate.
func (t *moduleTransformer) emitVar(s *ast.VariableStatement) {
	for _, b := range s.List {
		t.emitBindingInit(b, false)
	}
}

// emitLex turns let/const declarators into initialiser assignments. Exported
// names write the cell; the outer lexical binding is then synced so local
// reads observe the same value.
func (t *moduleTransformer) emitLex(s *ast.LexicalDeclaration) {
	for _, b := range s.List {
		t.emitBindingInit(b, true)
	}
}

// emitBindingInit emits the runtime effect of one declarator. Identifier
// targets are handled directly; pattern targets are rewritten and keep their
// normal destructuring semantics on the outer binding.
func (t *moduleTransformer) emitBindingInit(b *ast.Binding, lexical bool) {
	id, isID := b.Target.(*ast.Identifier)
	if !isID {
		// Non-exported destructuring declaration (destructuring in export
		// position is rejected by the parser). Outer leaf bindings were hoisted.
		nb := &ast.Binding{Target: b.Target, Initializer: t.rewriteNullable(b.Initializer)}
		if lexical {
			t.body = append(t.body, &ast.LexicalDeclaration{
				Token: token.LET, List: []*ast.Binding{nb},
			})
			return
		}
		t.body = append(t.body, &ast.VariableStatement{List: []*ast.Binding{nb}})
		return
	}

	name := id.Name
	_, isExport := t.exported[name]

	if b.Initializer == nil {
		return // var without init stays undefined; let without init stays undefined.
	}

	init := t.rewriteExpr(b.Initializer)
	if isExport {
		// exports.name = init; name = exports.name (sync local for nested reads).
		t.body = append(t.body, assignStmt(paramExports, name, init))
		t.body = append(t.body, localAssignStmt(name, member(paramExports, name)))
		return
	}
	t.body = append(t.body, localAssignStmt(name, init))
}

func (t *moduleTransformer) emitDefault(s *ast.ExportDefaultDeclaration) {
	switch d := s.Declaration.(type) {
	case *ast.FunctionDeclaration:
		// Hoisted + published at instantiation (named or synthetic anonymous).
		return
	case *ast.ClassDeclaration:
		cl := t.rewriteClass(d.Class)
		if d.Class.Name != nil {
			name := d.Class.Name.Name
			t.body = append(t.body, localAssignStmt(name, cl))
			t.body = append(t.body, assignStmt(paramExports, defaultExportName, ident(name)))
		} else {
			t.body = append(t.body, assignStmt(paramExports, defaultExportName, cl))
		}
	default:
		t.body = append(t.body, assignStmt(paramExports, defaultExportName, t.rewriteExpr(s.Value)))
	}
}

func (t *moduleTransformer) rewriteNullable(e ast.Expression) ast.Expression {
	if e == nil {
		return nil
	}
	return t.rewriteExpr(e)
}
