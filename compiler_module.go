package goja

import (
	js_ast "github.com/dop251/goja/ast"
	"github.com/dop251/goja/file"
	"github.com/dop251/goja/token"
	"github.com/dop251/goja/unistring"
)

const moduleDefaultName unistring.String = "*default*"

func compileModuleAST(prg *js_ast.Program) (p *Program, err error) {
	c := newCompiler()

	defer func() {
		if x := recover(); x != nil {
			p = nil
			switch x1 := x.(type) {
			case *CompilerSyntaxError:
				err = x1
			default:
				panic(x)
			}
		}
	}()

	c.compileModule(prg)
	return c.p, nil
}

func (c *compiler) compileModule(in *js_ast.Program) {
	c.p.src = in.File
	meta := &moduleMeta{
		ownExports: make(map[unistring.String]int),
		cellIndex:  make(map[unistring.String]int),
		varCells:   make(map[int]bool),
	}
	c.p.moduleMeta = meta

	// A sentinel outer scope makes the module top-level scope behave like
	// a function scope for name resolution (nested functions must find
	// module bindings instead of falling through to the global object).
	c.newScope()
	sentinel := c.scope
	sentinel.strict = true
	c.newScope()
	scope := c.scope
	scope.strict = true
	scope.isModule = true

	// phase 1: create every top-level name and record export/import info.
	hoistedFuncs := map[*js_ast.FunctionDeclaration]bool{}
	defaultExprs := map[*js_ast.ExportDefaultDeclaration]js_ast.Expression{}

	// lexically create a name in the module scope, flagging duplicates.
	declare := func(name unistring.String, offset int, isConst bool) *binding {
		b, created := scope.bindNameLexical(name, true, offset)
		if isConst {
			b.isConst = true
		}
		_ = created
		return b
	}

	recordExport := func(exported unistring.String, cell int) {
		if _, exists := meta.ownExports[exported]; exists {
			c.throwSyntaxErrorf(0, "Duplicate export of '%s'", exported.String())
		}
		meta.ownExports[exported] = cell
	}

	// imports
	for _, st := range in.Body {
		imp, ok := st.(*js_ast.ImportDeclaration)
		if !ok {
			continue
		}
		req := moduleImportRequest{specifier: imp.Module}
		addImport := func(local, imported unistring.String, idx file.Idx) {
			b := declare(local, int(idx)-1, true)
			b.isImport = true
			req.bindings = append(req.bindings, moduleImportBinding{local: local, imported: imported})
		}
		if imp.Default != nil {
			addImport(imp.Default.Name, "default", imp.Default.Idx)
		}
		for _, spec := range imp.Imports {
			addImport(spec.Local.Name, spec.Imported.Name, spec.Local.Idx)
		}
		if imp.Default == nil && len(imp.Imports) == 0 {
			req.sideEffectOnly = true
		}
		meta.imports = append(meta.imports, req)
	}

	// exports and hoistable declarations
	for _, st := range in.Body {
		switch st := st.(type) {
		case *js_ast.ExportNamedDeclaration:
			if st.HasSource {
				re := moduleReExport{specifier: st.Source}
				for _, spec := range st.Specifiers {
					re.bindings = append(re.bindings, moduleReExportBinding{
						local:    spec.Local.Name,
						exported: spec.Exported.Name,
					})
				}
				meta.reExports = append(meta.reExports, re)
				continue
			}
			if st.Declaration != nil {
				c.createModuleDeclBindings(st.Declaration, declare)
				if fn, ok := st.Declaration.(*js_ast.FunctionDeclaration); ok {
					hoistedFuncs[fn] = true
				}
				for _, name := range declarationBoundNames(st.Declaration) {
					recordExport(name, 0) // cell index filled after allocation
				}
			}
			for _, spec := range st.Specifiers {
				if _, exists := scope.boundNames[spec.Local.Name]; !exists {
					c.throwSyntaxErrorf(int(spec.Local.Idx)-1,
						"Exported name '%s' is not declared in this module", spec.Local.Name.String())
				}
				recordExport(spec.Exported.Name, 0)
			}
		case *js_ast.ExportDefaultDeclaration:
			switch d := st.Declaration.(type) {
			case *js_ast.FunctionDeclaration:
				name := d.Function.Name.Name
				if name == "" {
					// anonymous default function: bind it to the internal
					// *default* cell like a default expression.
					d.Function.Name = &js_ast.Identifier{Name: moduleDefaultName, Idx: st.Default}
					declare(moduleDefaultName, int(st.Default)-1, true)
					hoistedFuncs[d] = true
					recordExport("default", 0)
					continue
				}
				declare(name, int(st.Default)-1, false)
				hoistedFuncs[d] = true
				// "default" aliases the same binding cell as the name.
				recordExport("default", 0)
			case *js_ast.ClassDeclaration:
				if d.Class.Name != nil {
					declare(d.Class.Name.Name, int(d.Class.Class)-1, false)
				}
				declare(moduleDefaultName, int(st.Default)-1, true)
				recordExport("default", 0)
			default:
				expr, ok := d.(js_ast.Expression)
				if !ok {
					c.throwSyntaxError(int(st.Idx), "unsupported export default declaration")
				}
				declare(moduleDefaultName, int(st.Default)-1, true)
				recordExport("default", 0)
				defaultExprs[st] = expr
			}
		}
	}

	// ordinary top-level hoisted functions and var/let/const bindings
	// declareOrGet is used for var names, which legally redeclare other
	// var/function names but never lexical ones (handled by bindNameLexical).
	declareOrGet := func(name unistring.String, offset int, isConst bool) *binding {
		if b := scope.boundNames[name]; b != nil {
			return b
		}
		return declare(name, offset, false)
	}
	for _, st := range in.Body {
		switch st := st.(type) {
		case *js_ast.FunctionDeclaration:
			if _, exists := scope.boundNames[st.Function.Name.Name]; !exists {
				declare(st.Function.Name.Name, int(st.Function.Name.Idx1())-1, false)
			}
			hoistedFuncs[st] = true
		case *js_ast.VariableStatement:
			c.createModuleVarBindings(st.List, declareOrGet)
		case *js_ast.LexicalDeclaration:
			isConst := st.Token == token.CONST
			c.createModuleLexicalBindings(st.List, isConst, declare)
		case *js_ast.ClassDeclaration:
			if _, exists := scope.boundNames[st.Class.Name.Name]; !exists {
				declare(st.Class.Name.Name, int(st.Class.Class)-1, false)
			}
		}
	}

	// phase 2: allocate module cells. Every top-level binding gets a cell.
	for i, b := range scope.bindings {
		b.isModuleCell = true
		b.moduleCell = i
		meta.cellIndex[b.name] = i
		if b.isVar && !b.isArg {
			meta.varCells[i] = true
		}
	}
	meta.numCells = len(scope.bindings)
	for name := range meta.ownExports {
		var local unistring.String
		if name == "default" {
			local = moduleDefaultName
		} else {
			local = name
		}
		b := scope.boundNames[local]
		if b == nil {
			if name == "default" {
				// named default function/class: its cell is under the local name
				for _, st := range in.Body {
					if ed, ok := st.(*js_ast.ExportDefaultDeclaration); ok {
						if fd, ok := ed.Declaration.(*js_ast.FunctionDeclaration); ok && fd.Function.Name.Name != moduleDefaultName {
							b = scope.boundNames[fd.Function.Name.Name]
						}
					}
				}
			}
		}
		if b == nil {
			c.throwSyntaxErrorf(0, "Exported name '%s' is not declared in this module", name.String())
		}
		meta.ownExports[name] = b.moduleCell
	}

	// phase 3: emit code.
	c.emit(&enterModule{})

	for fn := range hoistedFuncs {
		c.compileFunctionLiteral(fn.Function, false).emitGetter(true)
		scope.boundNames[fn.Function.Name.Name].emitInitP()
	}

	for _, st := range in.Body {
		switch st := st.(type) {
		case *js_ast.ImportDeclaration:
			// no runtime code: imports are link-time cells
		case *js_ast.ExportNamedDeclaration:
			if st.HasSource {
				continue
			}
			if st.Declaration != nil {
				if _, isFn := st.Declaration.(*js_ast.FunctionDeclaration); isFn {
					continue // hoisted function declarations emit no body code
				}
				c.compileStatement(st.Declaration, false)
			}
			// 'export { a }' produces no code; 'export let x' declaration
			// already compiled above initialises the cell.
		case *js_ast.ExportDefaultDeclaration:
			if expr, ok := defaultExprs[st]; ok {
				c.compileExpression(expr).emitGetter(true)
				scope.boundNames[moduleDefaultName].emitInitP()
			} else if cl, ok := st.Declaration.(*js_ast.ClassDeclaration); ok {
				c.compileClassLiteral(cl.Class, false).emitGetter(true)
				// class value initialises both the (optional) name cell and
				// the default cell
				if cl.Class.Name != nil {
					scope.boundNames[cl.Class.Name.Name].emitInitP()
				}
				scope.boundNames[moduleDefaultName].emitInitP()
			}
			// default function declarations were hoisted and initialised
		case *js_ast.FunctionDeclaration:
			// hoisted and initialised before the body
		default:
			c.compileStatement(st, false)
		}
	}

	c.emit(&leaveModule{})

	scope.finaliseModuleAlloc()
	c.stringCache = nil
}

func (c *compiler) createModuleDeclBindings(st js_ast.Statement, declare func(unistring.String, int, bool) *binding) {
	switch st := st.(type) {
	case *js_ast.VariableStatement:
		c.createModuleVarBindings(st.List, declare)
	case *js_ast.LexicalDeclaration:
		c.createModuleLexicalBindings(st.List, st.Token == token.CONST, declare)
	case *js_ast.FunctionDeclaration:
		declare(st.Function.Name.Name, int(st.Function.Name.Idx1())-1, false)
	case *js_ast.ClassDeclaration:
		if st.Class.Name != nil {
			declare(st.Class.Name.Name, int(st.Class.Class)-1, false)
		}
	}
}

func (c *compiler) createModuleVarBindings(list []*js_ast.Binding, declare func(unistring.String, int, bool) *binding) {
	for _, b := range list {
		c.createBindings(b.Target, func(name unistring.String, offset int) {
			bd := declare(name, offset, false)
			bd.isVar = true
		})
	}
}

func (c *compiler) createModuleLexicalBindings(list []*js_ast.Binding, isConst bool, declare func(unistring.String, int, bool) *binding) {
	for _, b := range list {
		c.createBindings(b.Target, func(name unistring.String, offset int) {
			declare(name, offset, isConst)
		})
	}
}

func declarationBoundNames(st js_ast.Statement) []unistring.String {
	switch st := st.(type) {
	case *js_ast.VariableStatement:
		return bindingListNames(st.List)
	case *js_ast.LexicalDeclaration:
		return bindingListNames(st.List)
	case *js_ast.FunctionDeclaration:
		return []unistring.String{st.Function.Name.Name}
	case *js_ast.ClassDeclaration:
		return []unistring.String{st.Class.Name.Name}
	}
	return nil
}

func bindingListNames(list []*js_ast.Binding) []unistring.String {
	var names []unistring.String
	for _, b := range list {
		names = append(names, collectBindingNames(b.Target)...)
	}
	return names
}

func collectBindingNames(e js_ast.Expression) []unistring.String {
	switch e := e.(type) {
	case *js_ast.Identifier:
		return []unistring.String{e.Name}
	case *js_ast.ObjectPattern:
		var out []unistring.String
		for _, p := range e.Properties {
			switch p := p.(type) {
			case *js_ast.PropertyShort:
				out = append(out, p.Name.Name)
			case *js_ast.PropertyKeyed:
				out = append(out, collectBindingNames(p.Value)...)
			}
		}
		if e.Rest != nil {
			out = append(out, collectBindingNames(e.Rest)...)
		}
		return out
	case *js_ast.ArrayPattern:
		var out []unistring.String
		for _, el := range e.Elements {
			if el != nil {
				out = append(out, collectBindingNames(el)...)
			}
		}
		if e.Rest != nil {
			out = append(out, collectBindingNames(e.Rest)...)
		}
		return out
	case *js_ast.AssignExpression:
		return collectBindingNames(e.Left)
	}
	return nil
}
