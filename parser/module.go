package parser

import (
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/file"
	"github.com/dop251/goja/token"
)

// ParseModule parses src as an ES module. Only a deliberately limited v1
// subset of the module grammar is accepted:
//
//   - import defaultName from "mod";
//   - import { a, b as c } from "mod";
//   - import defaultName, { a, b as c } from "mod";
//   - export var/let/const/function/class ...;
//   - export { a, b as c };
//   - export { a, b as c } from "mod";           (explicit re-export)
//   - export default <functionDeclaration | classDeclaration | expression>;
//
// Everything else that would require unsupported runtime semantics is
// rejected at parse time, in particular dynamic import(), top-level await
// and export *.
func ParseModule(filename, src string, options ...Option) (*ast.Program, error) {
	p := _newParser(filename, src, 1, options...)
	p.module = true
	return p.parseModule()
}

func (self *_parser) parseModule() (*ast.Program, error) {
	self.openScope()
	defer self.closeScope()
	self.next()
	prg := &ast.Program{
		Body:            self.parseModuleBody(),
		DeclarationList: self.scope.declarationList,
		File:            self.file,
		Module:          true,
	}
	if len(self.errors) == 0 {
		self.file.SetSourceMap(self.parseSourceMap())
	}
	return prg, self.errors.Err()
}

func (self *_parser) parseModuleBody() (body []ast.Statement) {
	for self.token != token.EOF {
		self.scope.allowLet = true
		if self.token == token.KEYWORD {
			switch self.literal {
			case "import":
				body = append(body, self.parseImportDeclaration())
				continue
			case "export":
				body = append(body, self.parseExportDeclaration())
				continue
			}
		}
		if self.token == token.AWAIT {
			self.error(self.idx, "top-level await is not supported in module v1")
		}
		body = append(body, self.parseStatement())
	}
	return
}

func (self *_parser) parseModuleSpecifier() string {
	if self.token != token.STRING {
		self.error(self.idx, "Expected module specifier string literal")
		return ""
	}
	spec := string(self.parsedLiteral)
	self.next()
	return spec
}

func (self *_parser) parseImportDeclaration() ast.Statement {
	idx := self.idx
	self.next() // consume 'import'
	node := &ast.ImportDeclaration{Idx: idx}

	if self.token == token.STRING {
		// Side-effect-only import: 'import "mod";'. It creates the module
		// graph edge but introduces no local bindings.
		node.Module = string(self.parsedLiteral)
		self.next()
		self.semicolon()
		return node
	}

	if self.token == token.IDENTIFIER {
		node.Default = self.parseIdentifier()
		if self.token == token.COMMA {
			self.next() // consume ','
		}
	}

	if self.token == token.MULTIPLY {
		self.error(self.idx, "namespace imports ('import * as') are not supported in module v1")
	}

	if self.token == token.LEFT_BRACE {
		node.Imports = self.parseNamedImports()
	} else if node.Default == nil {
		self.error(self.idx, "Expected '{' or a default import identifier")
	}

	if self.literal != "from" {
		self.error(self.idx, "Expected 'from'")
	} else {
		self.next() // consume 'from'
	}
	node.Module = self.parseModuleSpecifier()
	self.semicolon()
	return node
}

func (self *_parser) parseNamedImports() []ast.ImportSpecifier {
	self.expect(token.LEFT_BRACE)
	var specs []ast.ImportSpecifier
	for self.token != token.RIGHT_BRACE && self.token != token.EOF {
		imported := self.parseModuleName()
		local := imported
		if self.literal == "as" {
			self.next()
			local = self.parseModuleName()
		}
		specs = append(specs, ast.ImportSpecifier{Imported: imported, Local: local})
		if self.token == token.COMMA {
			self.next()
			continue
		}
		break
	}
	self.expect(token.RIGHT_BRACE)
	return specs
}

// parseModuleName parses an identifier in a module clause. The keyword
// 'default' is legal as an imported export name.
func (self *_parser) parseModuleName() *ast.Identifier {
	if self.token == token.DEFAULT {
		id := &ast.Identifier{Name: "default", Idx: self.idx}
		self.next()
		return id
	}
	return self.parseIdentifier()
}

func (self *_parser) parseExportDeclaration() ast.Statement {
	idx := self.idx
	self.next() // consume 'export'

	switch self.token {
	case token.MULTIPLY:
		self.error(self.idx, "'export *' is not supported in module v1")
		return &ast.BadStatement{From: idx, To: idx + 1}
	case token.DEFAULT:
		return self.parseExportDefault(idx)
	case token.LEFT_BRACE:
		return self.parseExportClause(idx)
	case token.VAR, token.LET, token.CONST:
		if self.token == token.VAR {
			return &ast.ExportNamedDeclaration{Idx: idx, Declaration: self.parseVariableStatement()}
		}
		return &ast.ExportNamedDeclaration{Idx: idx, Declaration: self.parseLexicalDeclaration(self.token)}
	case token.FUNCTION:
		fn := self.parseFunction(true, false, self.idx)
		self.semicolon()
		return &ast.ExportNamedDeclaration{
			Idx:         idx,
			Declaration: &ast.FunctionDeclaration{Function: fn},
		}
	case token.CLASS:
		cl := self.parseClass(true)
		self.semicolon()
		return &ast.ExportNamedDeclaration{
			Idx:         idx,
			Declaration: &ast.ClassDeclaration{Class: cl},
		}
	case token.ASYNC:
		if f := self.parseMaybeAsyncFunction(true); f != nil {
			self.semicolon()
			return &ast.ExportNamedDeclaration{
				Idx:         idx,
				Declaration: &ast.FunctionDeclaration{Function: f},
			}
		}
	}
	self.errorUnexpectedToken(self.token)
	return &ast.BadStatement{From: idx, To: idx + 1}
}

func (self *_parser) parseExportDefault(idx file.Idx) *ast.ExportDefaultDeclaration {
	defaultIdx := self.idx
	self.next() // consume 'default'
	node := &ast.ExportDefaultDeclaration{Idx: idx, Default: defaultIdx}
	switch self.token {
	case token.FUNCTION:
		fn := self.parseFunction(false, false, self.idx)
		if fn.Name == nil {
			fn.Name = &ast.Identifier{Name: "*default*", Idx: defaultIdx}
		}
		node.Declaration = &ast.FunctionDeclaration{Function: fn}
	case token.CLASS:
		node.Declaration = &ast.ClassDeclaration{Class: self.parseClass(false)}
	case token.ASYNC:
		if f := self.parseMaybeAsyncFunction(false); f != nil {
			if f.Name == nil {
				f.Name = &ast.Identifier{Name: "*default*", Idx: defaultIdx}
			}
			node.Declaration = &ast.FunctionDeclaration{Function: f}
		} else {
			node.Declaration = self.parseAssignmentExpression()
		}
	default:
		node.Declaration = self.parseAssignmentExpression()
	}
	self.semicolon()
	return node
}

func (self *_parser) parseExportClause(idx file.Idx) *ast.ExportNamedDeclaration {
	self.expect(token.LEFT_BRACE)
	node := &ast.ExportNamedDeclaration{Idx: idx}
	for self.token != token.RIGHT_BRACE && self.token != token.EOF {
		if self.token == token.MULTIPLY {
			self.error(self.idx, "'export *' is not supported in module v1")
			self.next()
			continue
		}
		local := self.parseModuleName()
		exported := local
		if self.literal == "as" {
			self.next()
			exported = self.parseModuleName()
		}
		node.Specifiers = append(node.Specifiers, ast.ExportSpecifier{Local: local, Exported: exported})
		if self.token == token.COMMA {
			self.next()
			continue
		}
		break
	}
	self.expect(token.RIGHT_BRACE)
	if self.literal == "from" {
		self.next()
		node.HasSource = true
		node.Source = self.parseModuleSpecifier()
	}
	self.semicolon()
	return node
}
