package goja

import (
	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/token"
	"github.com/dop251/goja/unistring"
)

const defaultExportName = unistring.String("default")

// importedBinding pairs a locally bound import name with the name it has in
// the exporting module.
type importedBinding struct {
	local    unistring.String
	imported unistring.String
	source   *moduleRecord
}

// collectedMeta holds the declarative result of reading a module's top level.
type collectedMeta struct {
	imports []rawImport

	// local map: local name -> bindingKind, for bindings defined by this
	// module (exported and non-exported alike are tracked later during the
	// transform; here we only need exported names for linking).
	localExport map[unistring.String]bindingKind

	// ordered local exports, to install deterministically.
	localExportOrder []localExport

	// export clauses { local as exported } without "from".
	clauseExports []exportClause

	// exports of locally-imported bindings without "from": export { x } where
	// x is an import. These are live re-exports, not local cells.
	clauseImportSpec []clauseImportSpec

	// export { ... } from "spec"
	reExports []rawReExport

	// default export, if present.
	defaultDecl *ast.ExportDefaultDeclaration
}

type clauseImportSpec struct {
	exported unistring.String
	spec     unistring.String
	imported unistring.String
}

type rawImport struct {
	specifier unistring.String
	defaults  unistring.String // "" if none
	named     []importClauseName
}

type importClauseName struct {
	local, imported unistring.String
}

type exportClause struct {
	local, exported unistring.String
}

type rawReExport struct {
	specifier unistring.String
	list      []exportClause // imported name in Local field
}

func (l *ModuleLoader) collectModuleMetaData(m *moduleRecord) error {
	meta := &collectedMeta{
		localExport: make(map[unistring.String]bindingKind),
	}

	addLocalExport := func(name unistring.String, kind bindingKind, pos ast.Node) error {
		if existing, ok := meta.localExport[name]; ok {
			_ = existing
			return &LinkError{ID: m.id, Msg: "Duplicate export of '" + name.String() + "'"}
		}
		meta.localExport[name] = kind
		meta.localExportOrder = append(meta.localExportOrder, localExport{name: name, kind: kind})
		return nil
	}

	for _, stmt := range m.program.Body {
		switch s := stmt.(type) {
		case *ast.ImportDeclaration:
			ri := rawImport{specifier: s.Module.Value}
			if s.Default != nil {
				ri.defaults = s.Default.Name
			}
			for _, sp := range s.List {
				ri.named = append(ri.named, importClauseName{
					local:    sp.Local.Name,
					imported: sp.Imported.Name,
				})
			}
			meta.imports = append(meta.imports, ri)

		case *ast.ExportNamedDeclaration:
			if s.Module != nil {
				// explicit re-export
				re := rawReExport{specifier: s.Module.Value}
				for _, sp := range s.List {
					re.list = append(re.list, exportClause{
						local:    sp.Local.Name,
						exported: sp.Exported.Name,
					})
				}
				meta.reExports = append(meta.reExports, re)
				continue
			}

			if s.Declaration != nil {
				if err := collectExportedDeclaration(s.Declaration, addLocalExport); err != nil {
					return err
				}
				continue
			}

			for _, sp := range s.List {
				meta.clauseExports = append(meta.clauseExports, exportClause{
					local:    sp.Local.Name,
					exported: sp.Exported.Name,
				})
			}

		case *ast.ExportDefaultDeclaration:
			if meta.defaultDecl != nil {
				return &LinkError{ID: m.id, Msg: "Multiple default exports"}
			}
			meta.defaultDecl = s
		}
	}

	if meta.defaultDecl != nil {
		kind := bindDefault
		if fn, ok := meta.defaultDecl.Declaration.(*ast.FunctionDeclaration); ok && fn.Function.Name != nil {
			kind = bindFunc
		}
		if cl, ok := meta.defaultDecl.Declaration.(*ast.ClassDeclaration); ok && cl.Class.Name != nil {
			kind = bindClass
		}
		if err := addLocalExport(defaultExportName, kind, meta.defaultDecl); err != nil {
			return err
		}
	}

	// Build a map of imported local names so clauses re-exporting an import
	// become live indirect exports rather than local cells.
	importedLocals := make(map[unistring.String]struct {
		source   unistring.String
		imported unistring.String
	})
	for _, imp := range meta.imports {
		if imp.defaults != "" {
			importedLocals[imp.defaults] = struct {
				source   unistring.String
				imported unistring.String
			}{imp.specifier, defaultExportName}
		}
		for _, n := range imp.named {
			importedLocals[n.local] = struct {
				source   unistring.String
				imported unistring.String
			}{imp.specifier, n.imported}
		}
	}

	// Resolve clause exports against locally declared bindings. This is done
	// after walking the whole body so order does not matter.
	locals := collectTopLevelDeclaredNames(m.program)
	for _, c := range meta.clauseExports {
		if imp, isImport := importedLocals[c.local]; isImport {
			meta.clauseImportSpec = append(meta.clauseImportSpec, clauseImportSpec{
				exported: c.exported,
				spec:     imp.source,
				imported: imp.imported,
			})
			continue
		}
		kind, ok := locals[c.local]
		if !ok {
			return &LinkError{
				ID:  m.id,
				Msg: "Exported name '" + c.local.String() + "' is not declared in this module",
			}
		}
		// `export let v; ... export { v };` re-exposes the same slot, it is
		// not a duplicate export. Only a second declaration of the same public
		// name from a different local is an error.
		if _, already := meta.localExport[c.local]; already {
			continue
		}
		if err := addLocalExport(c.exported, kind, nil); err != nil {
			return err
		}
	}

	m.meta = meta

	// Recursively parse dependencies (imports + re-exports), resolving their
	// canonical IDs. Aliases share a module record because the map is keyed by
	// canonical ID.
	depSpecs := make(map[string]struct{})
	for _, imp := range meta.imports {
		depSpecs[imp.specifier.String()] = struct{}{}
	}
	for _, re := range meta.reExports {
		depSpecs[re.specifier.String()] = struct{}{}
	}

	for spec := range depSpecs {
		id, err := l.canonical(m.id, spec)
		if err != nil {
			return err
		}
		dep, err := l.getOrParse(id)
		if err != nil {
			return err
		}
		m.dependencies = append(m.dependencies, dep)
	}

	return nil
}

func collectExportedDeclaration(decl ast.Statement, add func(unistring.String, bindingKind, ast.Node) error) error {
	switch d := decl.(type) {
	case *ast.VariableStatement:
		for _, b := range d.List {
			id := b.Target.(*ast.Identifier)
			if err := add(id.Name, bindVar, b); err != nil {
				return err
			}
		}
	case *ast.LexicalDeclaration:
		kind := bindLet
		if d.Token == token.CONST {
			kind = bindConst
		}
		for _, b := range d.List {
			id := b.Target.(*ast.Identifier)
			if err := add(id.Name, kind, b); err != nil {
				return err
			}
		}
	case *ast.FunctionDeclaration:
		name := d.Function.Name
		if name == nil {
			// export default function(){} handled elsewhere
			return nil
		}
		return add(name.Name, bindFunc, d)
	case *ast.ClassDeclaration:
		if d.Class.Name == nil {
			return nil
		}
		return add(d.Class.Name.Name, bindClass, d)
	}
	return nil
}

// collectLocalNonImportNames returns module-scope names declared by the module
// itself (var/let/const/function/class), excluding imported bindings.
func collectLocalNonImportNames(prg *ast.Program) map[unistring.String]struct{} {
	out := make(map[unistring.String]struct{})
	var scanDecl func(stmt ast.Statement, declare func(unistring.String))
	scanDecl = func(stmt ast.Statement, declare func(unistring.String)) {
		switch s := stmt.(type) {
		case *ast.VariableStatement:
			for _, b := range s.List {
				if id, ok := b.Target.(*ast.Identifier); ok {
					declare(id.Name)
				}
			}
		case *ast.LexicalDeclaration:
			for _, b := range s.List {
				if id, ok := b.Target.(*ast.Identifier); ok {
					declare(id.Name)
				}
			}
		case *ast.FunctionDeclaration:
			if s.Function.Name != nil {
				declare(s.Function.Name.Name)
			}
		case *ast.ClassDeclaration:
			if s.Class.Name != nil {
				declare(s.Class.Name.Name)
			}
		case *ast.ExportNamedDeclaration:
			if s.Declaration != nil {
				scanDecl(s.Declaration, declare)
			}
		case *ast.ExportDefaultDeclaration:
			if s.Declaration != nil {
				scanDecl(s.Declaration, declare)
			}
		}
	}
	declare := func(name unistring.String) { out[name] = struct{}{} }
	for _, stmt := range prg.Body {
		scanDecl(stmt, declare)
	}
	return out
}

// collectTopLevelDeclaredNames returns the map from module-scope declared
// names (var/let/const/function/class, including imported bindings) to the kind
// of binding. Imports are treated as immutable lexical bindings.
func collectTopLevelDeclaredNames(prg *ast.Program) map[unistring.String]bindingKind {
	out := make(map[unistring.String]bindingKind)

	declare := func(name unistring.String, kind bindingKind) {
		if _, exists := out[name]; !exists {
			out[name] = kind
		}
	}

	for _, stmt := range prg.Body {
		switch s := stmt.(type) {
		case *ast.ImportDeclaration:
			if s.Default != nil {
				declare(s.Default.Name, bindImport)
			}
			for _, sp := range s.List {
				declare(sp.Local.Name, bindImport)
			}
		case *ast.VariableStatement:
			for _, b := range s.List {
				if id, ok := b.Target.(*ast.Identifier); ok {
					declare(id.Name, bindVar)
				}
			}
		case *ast.LexicalDeclaration:
			kind := bindLet
			if s.Token == token.CONST {
				kind = bindConst
			}
			for _, b := range s.List {
				if id, ok := b.Target.(*ast.Identifier); ok {
					declare(id.Name, kind)
				}
			}
		case *ast.FunctionDeclaration:
			if s.Function.Name != nil {
				declare(s.Function.Name.Name, bindFunc)
			}
		case *ast.ClassDeclaration:
			if s.Class.Name != nil {
				declare(s.Class.Name.Name, bindClass)
			}
		case *ast.ExportNamedDeclaration:
			if s.Declaration != nil {
				switch d := s.Declaration.(type) {
				case *ast.VariableStatement:
					for _, b := range d.List {
						if id, ok := b.Target.(*ast.Identifier); ok {
							declare(id.Name, bindVar)
						}
					}
				case *ast.LexicalDeclaration:
					kind := bindLet
					if d.Token == token.CONST {
						kind = bindConst
					}
					for _, b := range d.List {
						if id, ok := b.Target.(*ast.Identifier); ok {
							declare(id.Name, kind)
						}
					}
				case *ast.FunctionDeclaration:
					if d.Function.Name != nil {
						declare(d.Function.Name.Name, bindFunc)
					}
				case *ast.ClassDeclaration:
					if d.Class.Name != nil {
						declare(d.Class.Name.Name, bindClass)
					}
				}
			}
		case *ast.ExportDefaultDeclaration:
			if s.Declaration != nil {
				switch d := s.Declaration.(type) {
				case *ast.FunctionDeclaration:
					if d.Function.Name != nil {
						declare(d.Function.Name.Name, bindFunc)
					}
				case *ast.ClassDeclaration:
					if d.Class.Name != nil {
						declare(d.Class.Name.Name, bindClass)
					}
				}
			}
		}
	}
	return out
}
