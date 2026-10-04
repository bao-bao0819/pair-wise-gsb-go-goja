package parser

import (
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/file"
	"github.com/dop251/goja/token"
	"github.com/dop251/goja/unistring"
)

// checkModuleBody enforces the v1 module syntax boundary on an already parsed
// module AST. These are checks that are easier to perform structurally than in
// the hand-written recursive descent parser. All violations are reported as
// parser errors so the module is rejected before any evaluation.
func (self *_parser) checkModuleBody(prg *ast.Program) {
	for _, stmt := range prg.Body {
		switch stmt := stmt.(type) {
		case *ast.ExportNamedDeclaration:
			if stmt.Declaration != nil {
				self.checkExportDeclaration(stmt)
			}
		case *ast.ExportDefaultDeclaration:
			if await, ok := stmt.Value.(*ast.AwaitExpression); ok {
				self.error(await.Await, "Top-level await is not supported in modules")
			}
		}
	}
	self.checkTopLevelAwait(prg.Body)
}

// checkExportDeclaration rejects destructuring in exported var/let/const
// declarations for v1: `export const {a} = ...`. The implementation only
// links identifier bindings, so patterns cannot be exported live.
func (self *_parser) checkExportDeclaration(stmt *ast.ExportNamedDeclaration) {
	var bindings []*ast.Binding
	switch d := stmt.Declaration.(type) {
	case *ast.VariableStatement:
		bindings = d.List
	case *ast.LexicalDeclaration:
		bindings = d.List
	}
	for _, b := range bindings {
		if _, ok := b.Target.(*ast.Identifier); !ok {
			self.error(b.Target.Idx0(), "Destructuring in export declarations is not supported")
		}
	}
}

// checkTopLevelAwait walks statements at module scope and rejects any
// AwaitExpression that is not nested inside a function (async arrow, method or
// function body). Class and module-level code is module scope.
func (self *_parser) checkTopLevelAwait(body []ast.Statement) {
	v := moduleAwaitChecker{p: self}
	for _, stmt := range body {
		if _, isImport := stmt.(*ast.ImportDeclaration); isImport {
			continue
		}
		if _, isExport := stmt.(*ast.ExportNamedDeclaration); isExport {
			// export { ... } has no expressions; export declarations handled below.
		}
		v.stmt(stmt)
	}
}

type moduleAwaitChecker struct {
	p    *_parser
	skip bool
}

func (v *moduleAwaitChecker) err(node ast.Node) {
	v.p.error(node.Idx0(), "Top-level await is not supported in modules")
}

func (v *moduleAwaitChecker) exprList(list []ast.Expression) {
	for _, e := range list {
		v.expr(e)
	}
}

func (v *moduleAwaitChecker) expr(e ast.Expression) {
	if e == nil {
		return
	}
	switch e := e.(type) {
	case *ast.AwaitExpression:
		v.err(e)
	case *ast.FunctionLiteral, *ast.ArrowFunctionLiteral:
		return // await inside a function is fine
	case *ast.ClassLiteral:
		if e.SuperClass != nil {
			v.expr(e.SuperClass)
		}
		for _, el := range e.Body {
			switch el := el.(type) {
			case *ast.FieldDefinition:
				v.expr(el.Key)
				v.expr(el.Initializer)
			case *ast.MethodDefinition:
				// methods are function scopes
			case *ast.ClassStaticBlock:
				for _, s := range el.Block.List {
					v.stmt(s)
				}
			}
		}
	case *ast.ArrayLiteral:
		v.exprList(e.Value)
	case *ast.ObjectLiteral:
		for _, p := range e.Value {
			switch p := p.(type) {
			case *ast.PropertyShort:
				v.expr(p.Initializer)
			case *ast.PropertyKeyed:
				v.expr(p.Key)
				v.expr(p.Value)
			case *ast.SpreadElement:
				v.expr(p.Expression)
			}
		}
	case *ast.BinaryExpression:
		v.expr(e.Left)
		v.expr(e.Right)
	case *ast.AssignExpression:
		v.expr(e.Left)
		v.expr(e.Right)
	case *ast.UnaryExpression:
		v.expr(e.Operand)
	case *ast.CallExpression:
		v.expr(e.Callee)
		v.exprList(e.ArgumentList)
	case *ast.NewExpression:
		v.expr(e.Callee)
		v.exprList(e.ArgumentList)
	case *ast.ConditionalExpression:
		v.expr(e.Test)
		v.expr(e.Consequent)
		v.expr(e.Alternate)
	case *ast.DotExpression:
		v.expr(e.Left)
	case *ast.BracketExpression:
		v.expr(e.Left)
		v.expr(e.Member)
	case *ast.SequenceExpression:
		v.exprList(e.Sequence)
	case *ast.TemplateLiteral:
		v.expr(e.Tag)
		v.exprList(e.Expressions)
	case *ast.YieldExpression:
		v.expr(e.Argument)
	case *ast.SpreadElement:
		v.expr(e.Expression)
	case *ast.MetaProperty:
		if e.Meta.Name == "import" {
			v.err(e)
		}
	case *ast.PropertyShort:
		v.expr(e.Initializer)
	case *ast.PropertyKeyed:
		v.expr(e.Key)
		v.expr(e.Value)
	}
	// Binding targets / patterns are traversed via stmt paths where needed.
}

func (v *moduleAwaitChecker) bindingList(list []*ast.Binding) {
	for _, b := range list {
		v.expr(b.Initializer)
	}
}

func (v *moduleAwaitChecker) stmt(s ast.Statement) {
	if s == nil {
		return
	}
	switch s := s.(type) {
	case *ast.ExportNamedDeclaration:
		if s.Declaration != nil {
			v.stmt(s.Declaration)
		}
		return
	case *ast.ExportDefaultDeclaration:
		if s.Declaration != nil {
			return // function/class declaration
		}
		v.expr(s.Value)
		return
	case *ast.ImportDeclaration:
		return
	case *ast.ExpressionStatement:
		v.expr(s.Expression)
	case *ast.BlockStatement:
		for _, st := range s.List {
			v.stmt(st)
		}
	case *ast.IfStatement:
		v.expr(s.Test)
		v.stmt(s.Consequent)
		v.stmt(s.Alternate)
	case *ast.ForStatement:
		if init, ok := s.Initializer.(*ast.ForLoopInitializerExpression); ok {
			v.expr(init.Expression)
		} else if init, ok := s.Initializer.(*ast.ForLoopInitializerVarDeclList); ok {
			v.bindingList(init.List)
		} else if init, ok := s.Initializer.(*ast.ForLoopInitializerLexicalDecl); ok {
			v.bindingList(init.LexicalDeclaration.List)
		}
		v.expr(s.Test)
		v.expr(s.Update)
		v.stmt(s.Body)
	case *ast.ForInStatement:
		v.forInto(s.Into)
		v.expr(s.Source)
		v.stmt(s.Body)
	case *ast.ForOfStatement:
		v.forInto(s.Into)
		v.expr(s.Source)
		v.stmt(s.Body)
	case *ast.WhileStatement:
		v.expr(s.Test)
		v.stmt(s.Body)
	case *ast.DoWhileStatement:
		v.stmt(s.Body)
		v.expr(s.Test)
	case *ast.ReturnStatement:
		v.expr(s.Argument)
	case *ast.ThrowStatement:
		v.expr(s.Argument)
	case *ast.TryStatement:
		for _, st := range s.Body.List {
			v.stmt(st)
		}
		if s.Catch != nil {
			for _, st := range s.Catch.Body.List {
				v.stmt(st)
			}
		}
		if s.Finally != nil {
			for _, st := range s.Finally.List {
				v.stmt(st)
			}
		}
	case *ast.SwitchStatement:
		v.expr(s.Discriminant)
		for _, c := range s.Body {
			v.expr(c.Test)
			for _, st := range c.Consequent {
				v.stmt(st)
			}
		}
	case *ast.LabelledStatement:
		v.stmt(s.Statement)
	case *ast.VariableStatement:
		v.bindingList(s.List)
	case *ast.LexicalDeclaration:
		v.bindingList(s.List)
	case *ast.FunctionDeclaration, *ast.ClassDeclaration:
		// scoped; await inside handled by parser's async-scope rules
	}
}

func (v *moduleAwaitChecker) forInto(into ast.ForInto) {
	switch into := into.(type) {
	case *ast.ForIntoVar:
		v.expr(into.Binding.Initializer)
	case *ast.ForDeclaration:
		if b, ok := into.Target.(*ast.Identifier); ok {
			_ = b
		}
	case *ast.ForIntoExpression:
		v.expr(into.Expression)
	}
}

// isImportToken reports whether the current token is the "import" keyword.
// import and export are both lexed as the generic KEYWORD future-keyword, so
// they have to be distinguished by their literal text.
func (self *_parser) isImportToken() bool {
	return self.token == token.KEYWORD && self.parsedLiteral == "import"
}

// isExportToken reports whether the current token is the "export" keyword.
func (self *_parser) isExportToken() bool {
	return self.token == token.KEYWORD && self.parsedLiteral == "export"
}

// parseModuleIdentifier parses an identifier in a module clause (import/export
// binding or alias). It accepts "default" and other contextual keywords.
func (self *_parser) parseModuleKeywordAsId(kw unistring.String) *ast.Identifier {
	idx := self.idx
	self.next()
	return &ast.Identifier{Name: kw, Idx: idx}
}

func (self *_parser) parseModuleBindingName() *ast.Identifier {
	if self.token == token.DEFAULT {
		return self.parseModuleKeywordAsId("default")
	}
	if self.token == token.KEYWORD && self.parsedLiteral == "from" {
		self.errorUnexpectedToken(self.token)
	}
	if !self.isBindingId(self.token) {
		self.errorUnexpectedToken(self.token)
		return &ast.Identifier{Name: self.parsedLiteral, Idx: self.idx}
	}
	name := self.parsedLiteral
	idx := self.idx
	self.next()
	return &ast.Identifier{Name: name, Idx: idx}
}

// parseExportedOrImportedName parses a name in an import/export clause.
// String literal names are supported per spec ("import {'a b' as ab}"), but the
// current v1 grammar only permits identifiers and the keyword "default".
func (self *_parser) parseModuleClauseName() *ast.Identifier {
	if self.token == token.STRING {
		self.error(self.idx, "String literal import/export names are not supported")
		idx := self.idx
		lit := self.parsedLiteral
		self.next()
		return &ast.Identifier{Name: lit, Idx: idx}
	}
	return self.parseModuleBindingName()
}

// parseModuleSpecifier parses "from" "string" or, for imports, a bare string.
// Returns the string literal and whether a "from" keyword preceded it.
func (self *_parser) parseModuleSpecifier(allowBare bool) *ast.StringLiteral {
	hasFrom := false
	if self.token == token.IDENTIFIER && self.parsedLiteral == "from" {
		self.next()
		hasFrom = true
	} else if !allowBare {
		self.error(self.idx, "Expected 'from' clause")
	}
	if self.token != token.STRING {
		self.error(self.idx, "Module specifier must be a string literal")
		return &ast.StringLiteral{Idx: self.idx}
	}
	node := &ast.StringLiteral{
		Idx:     self.idx,
		Literal: self.literal,
		Value:   self.parsedLiteral,
	}
	self.next()
	if !hasFrom && !allowBare {
		// unreachable, handled above
	}
	return node
}

// parseImportDeclaration parses:
//
//	import "specifier";
//	import defaultName from "specifier";
//	import { a, b as c } from "specifier";
//	import defaultName, { a } from "specifier";
//
// Explicitly rejected (v1 boundary):
//
//	import * as ns from "..." (namespace import)
//	import("...")          (dynamic import)
//	import.meta
func (self *_parser) parseImportDeclaration() *ast.ImportDeclaration {
	idx := self.idx
	self.next() // consume "import"

	node := &ast.ImportDeclaration{Idx: idx}

	if self.token == token.PERIOD {
		self.error(self.idx, "import.meta is not supported")
		self.nextStatement()
		return node
	}
	if self.token == token.LEFT_PARENTHESIS {
		self.error(self.idx, "Dynamic import() is not supported; use a static import statement")
		self.nextStatement()
		return node
	}

	// Side-effect only import: import "x";
	if self.token == token.STRING {
		node.Module = self.parseModuleSpecifier(true)
		self.semicolon()
		return node
	}

	if self.token == token.MULTIPLY {
		self.error(self.idx, "Namespace imports (import * as) are not supported")
		self.nextStatement()
		return node
	}

	// Default import binding.
	if self.token != token.LEFT_BRACE {
		node.Default = self.parseModuleBindingName()
		if self.token == token.COMMA {
			self.next()
		}
	}

	if self.token == token.LEFT_BRACE {
		self.next() // consume {
		for {
			if self.token == token.RIGHT_BRACE {
				break
			}
			if self.token == token.MULTIPLY {
				self.error(self.idx, "Namespace re-exports/imports are not supported")
				self.nextStatement()
				return node
			}
			imported := self.parseModuleClauseName()
			local := imported
			if self.token == token.IDENTIFIER && self.parsedLiteral == "as" {
				self.next()
				local = self.parseModuleBindingName()
			}
			node.List = append(node.List, ast.ImportSpecifier{
				Imported: imported,
				Local:    local,
			})
			if self.token == token.COMMA {
				self.next()
				if self.token == token.RIGHT_BRACE {
					break
				}
				continue
			}
			break
		}
		self.expect(token.RIGHT_BRACE)
	}

	if node.Default == nil && len(node.List) == 0 {
		self.error(self.idx, "Invalid import declaration")
	}

	node.Module = self.parseModuleSpecifier(false)
	self.semicolon()
	return node
}

// parseExportDeclaration parses all supported export forms:
//
//	export var/let/const ...
//	export function [name] ... / export async function ... / export class ...
//	export default <expression|function|class>
//	export { a, b as c } [from "specifier"];
//
// Explicitly rejected (v1 boundary):
//
//	export * [as name] from "..."
//	export let/const with destructuring patterns
func (self *_parser) parseExportDeclaration() ast.Statement {
	idx := self.idx
	self.next() // consume "export"

	if self.token == token.MULTIPLY {
		self.error(self.idx, "export * is not supported")
		self.nextStatement()
		return &ast.ExportNamedDeclaration{Idx: idx}
	}

	if self.token == token.DEFAULT {
		return self.parseExportDefaultDeclaration(idx)
	}

	named := &ast.ExportNamedDeclaration{Idx: idx}

	switch {
	case self.token == token.LEFT_BRACE:
		self.next() // {
		for {
			if self.token == token.RIGHT_BRACE {
				break
			}
			local := self.parseModuleClauseName()
			exported := local
			if self.token == token.IDENTIFIER && self.parsedLiteral == "as" {
				self.next()
				exported = self.parseModuleClauseName()
			}
			named.List = append(named.List, ast.ExportSpecifier{
				Local:    local,
				Exported: exported,
			})
			if self.token == token.COMMA {
				self.next()
				if self.token == token.RIGHT_BRACE {
					break
				}
				continue
			}
			break
		}
		self.expect(token.RIGHT_BRACE)

		if self.token == token.IDENTIFIER && self.parsedLiteral == "from" {
			named.Module = self.parseModuleSpecifier(false)
		}
		self.semicolon()
		return named

	case self.token == token.VAR, self.token == token.LET, self.token == token.CONST:
		named.Declaration = self.parseStatement()
		return named

	case self.token == token.FUNCTION:
		fn := self.parseFunction(true, false, self.idx)
		named.Declaration = &ast.FunctionDeclaration{Function: fn}
		return named

	case self.token == token.ASYNC:
		if f := self.parseMaybeAsyncFunction(true); f != nil {
			named.Declaration = &ast.FunctionDeclaration{Function: f}
			return named
		}
		self.error(self.idx, "Unexpected token after export")
		return named

	case self.token == token.CLASS:
		cl := self.parseClass(true)
		named.Declaration = &ast.ClassDeclaration{Class: cl}
		return named
	}

	self.error(self.idx, "Unexpected token after export")
	return named
}

func (self *_parser) parseExportDefaultDeclaration(exportIdx file.Idx) *ast.ExportDefaultDeclaration {
	defaultIdx := self.idx
	self.next() // consume "default"

	node := &ast.ExportDefaultDeclaration{
		Idx:     exportIdx,
		Default: defaultIdx,
	}

	switch {
	case self.token == token.FUNCTION:
		fn := self.parseFunction(false, false, self.idx)
		node.Declaration = &ast.FunctionDeclaration{Function: fn}
		return node
	case self.token == token.ASYNC:
		if f := self.parseMaybeAsyncFunction(false); f != nil {
			node.Declaration = &ast.FunctionDeclaration{Function: f}
			return node
		}
	case self.token == token.CLASS:
		cl := self.parseClass(false)
		node.Declaration = &ast.ClassDeclaration{Class: cl}
		return node
	}

	node.Value = self.parseAssignmentExpression()
	self.semicolon()
	return node
}
