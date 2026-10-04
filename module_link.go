package goja

import (
	"github.com/dop251/goja/unistring"
)

// instantiate resolves and links the whole graph reachable from the given
// entry ID, then compiles every module body. Parsing must have completed
// first. Linking is deterministic and side-effect free apart from installing
// accessor objects; no module code is evaluated.
//
// The walk is a depth-first post-order traversal with a visiting set, which
// tolerates cycles: a dependency that is currently being visited is simply
// skipped on the stack, because its bindings are installed as soon as its
// record exists.
func (l *ModuleLoader) instantiate(entryID string) (*moduleRecord, error) {
	entry, ok := l.modules[entryID]
	if !ok {
		return nil, &LinkError{ID: entryID, Msg: "module has not been parsed"}
	}

	visiting := make(map[string]bool)
	done := make(map[string]bool)
	var order []*moduleRecord
	var linkErr error
	var visit func(m *moduleRecord)
	visit = func(m *moduleRecord) {
		if linkErr != nil {
			return
		}
		if m.status >= statusInstantiated || visiting[m.id] || done[m.id] {
			return
		}
		visiting[m.id] = true
		for _, dep := range m.dependencies {
			visit(dep)
		}
		visiting[m.id] = false
		done[m.id] = true
		order = append(order, m)
	}
	visit(entry)
	if linkErr != nil {
		return nil, linkErr
	}

	// Two passes over the deterministic order: first create every cell and
	// import view (so cross references can point at them even in a cycle),
	// then wire the accessors and compile the bodies.
	for _, m := range order {
		if m.cell != nil {
			continue
		}
		if err := m.createBindings(); err != nil {
			return nil, err
		}
	}
	for _, m := range order {
		if m.status >= statusInstantiated || m.linked {
			continue
		}
		if err := m.linkBindings(); err != nil {
			return nil, err
		}
		m.linked = true
	}
	for _, m := range order {
		if m.instantiateProgram != nil {
			m.status = statusInstantiated
			continue
		}
		if err := m.compile(); err != nil {
			return nil, err
		}
		m.status = statusInstantiated
	}
	return entry, nil
}

// resolveExport finds the module and exported name that ultimately back a
// public name of m, following explicit re-exports. Cycles in re-export chains
// are guarded against by resolveStack.
func (m *moduleRecord) resolveExport(name unistring.String, stack map[*moduleRecord]bool) (*moduleRecord, unistring.String, bool) {
	if stack[m] {
		return nil, "", false
	}
	if _, ok := m.meta.localExport[name]; ok {
		return m, name, true
	}
	for _, ie := range m.indirectExports {
		if ie.exported == name {
			stack[m] = true
			tm, tn, ok := ie.target.resolveExport(ie.imported, stack)
			delete(stack, m)
			if ok {
				return tm, tn, true
			}
		}
	}
	return nil, "", false
}

// createBindings allocates the module's export cell and predeclares all local
// export slots, and records indirect (re-export) edges.
func (m *moduleRecord) createBindings() error {
	r := m.loader.runtime
	m.cell = r.NewObject()
	m.importView = r.NewObject()

	for _, le := range m.meta.localExportOrder {
		m.tz.predeclare(le.name, le.kind)
	}

	// Build indirect export links. Specifier -> target record.
	targetBySpec := make(map[unistring.String]*moduleRecord)
	for _, dep := range m.dependencies {
		// dependencies are keyed by canonical ID; map via raw re-exports below
		_ = dep
	}
	_ = targetBySpec

	for _, re := range m.meta.reExports {
		target, err := m.dependencyForSpecifier(re.specifier)
		if err != nil {
			return err
		}
		for _, c := range re.list {
			m.indirectExports = append(m.indirectExports, indirectExport{
				exported: c.exported,
				imported: c.local,
				target:   target,
			})
		}
	}

	// export { importedLocal as exported } (no "from"): re-export an import.
	for _, ci := range m.meta.clauseImportSpec {
		target, err := m.dependencyForSpecifier(ci.spec)
		if err != nil {
			return err
		}
		m.indirectExports = append(m.indirectExports, indirectExport{
			exported: ci.exported,
			imported: ci.imported,
			target:   target,
		})
	}
	return nil
}

// dependencyForSpecifier returns the parsed record for a raw specifier used by
// this module, applying the same canonicalisation as during parsing.
func (m *moduleRecord) dependencyForSpecifier(spec unistring.String) (*moduleRecord, error) {
	id, err := m.loader.canonical(m.id, spec.String())
	if err != nil {
		return nil, err
	}
	dep, ok := m.loader.modules[id]
	if !ok {
		return nil, &LinkError{ID: m.id, Msg: "dependency '" + spec.String() + "' was not parsed"}
	}
	return dep, nil
}

// linkBindings installs getter/setter accessors on the export cell and
// getter-only accessors on each importer's import view. Re-exports are getters
// that delegate through the exporter's own cell, so live bindings and the TDZ
// are preserved across any length of chain.
func (m *moduleRecord) linkBindings() error {
	r := m.loader.runtime

	// Own local exports.
	for _, le := range m.meta.localExportOrder {
		name := le.name
		getter := r.newNativeFunc(func(FunctionCall) Value {
			v, ok := m.tz.get(name)
			if !ok {
				panic(r.newError(r.getReferenceError(), "Cannot access '"+name.String()+"' before initialization"))
			}
			return v
		}, unistring.String("get "+name.String()), 0)

		// Every local export has a setter. const/default slots accept exactly
		// one write (their initialiser, taking them out of the TDZ) and throw
		// afterwards; let/var/function slots stay mutable.
		setter := r.newNativeFunc(func(call FunctionCall) Value {
			m.tz.set(r, name, call.Argument(0))
			return _undefined
		}, unistring.String("set "+name.String()), 1)
		if err := m.cell.DefineAccessorProperty(name.String(), getter, setter, FLAG_FALSE, FLAG_FALSE); err != nil {
			return err
		}
	}

	// Indirect re-exports on the same cell.
	for _, ie := range m.indirectExports {
		exported := ie.exported
		target := ie.target
		imported := ie.imported
		getter := r.newNativeFunc(func(FunctionCall) Value {
			tm, tn, ok := target.resolveExport(imported, make(map[*moduleRecord]bool))
			if !ok {
				panic(r.newError(r.getReferenceError(), "Export '"+imported.String()+"' is not present in '"+target.id+"'"))
			}
			return tm.cell.Get(tn.String())
		}, unistring.String("get "+exported.String()), 0)
		if err := m.cell.DefineAccessorProperty(exported.String(), getter, nil, FLAG_FALSE, FLAG_FALSE); err != nil {
			return err
		}
	}

	// Validate and wire imports. Multiple import statements may reference the
	// same module; group their clauses so each source is resolved once.
	type impBinding struct{ local, imported unistring.String }
	grouped := make(map[unistring.String][]impBinding)
	var orderedSpecs []unistring.String
	for _, imp := range m.meta.imports {
		if _, seen := grouped[imp.specifier]; !seen {
			orderedSpecs = append(orderedSpecs, imp.specifier)
		}
		if imp.defaults != "" {
			grouped[imp.specifier] = append(grouped[imp.specifier], impBinding{imp.defaults, defaultExportName})
		}
		for _, n := range imp.named {
			grouped[imp.specifier] = append(grouped[imp.specifier], impBinding{n.local, n.imported})
		}
	}
	for _, spec := range orderedSpecs {
		src, err := m.dependencyForSpecifier(spec)
		if err != nil {
			return err
		}
		bindings := grouped[spec]
		for _, b := range bindings {
			tm, tn, ok := src.resolveExport(b.imported, make(map[*moduleRecord]bool))
			if !ok {
				return &LinkError{
					ID:  m.id,
					Msg: "Module '" + src.id + "' does not provide an export named '" + b.imported.String() + "'",
				}
			}
			local, target, imported := b.local, tm, tn
			getter := r.newNativeFunc(func(FunctionCall) Value {
				return target.cell.Get(imported.String())
			}, unistring.String("get "+local.String()), 0)
			if err := m.importView.DefineAccessorProperty(local.String(), getter, nil, FLAG_FALSE, FLAG_FALSE); err != nil {
				return err
			}
		}
	}

	return nil
}
