package goja

import "github.com/dop251/goja/unistring"

// Instantiate parses (if needed), links and compiles the module graph for
// entrySpecifier, returning the canonical entry ID. Module bodies do not run.
// After Instantiate succeeds the graph is fully linked: import views and
// export cells exist, hoisted exported functions are published, and every
// evaluator is compiled.
func (l *ModuleLoader) Instantiate(entrySpecifier string) (string, error) {
	id, err := l.ParseModule(entrySpecifier)
	if err != nil {
		return "", err
	}
	if _, err := l.instantiate(id); err != nil {
		return "", err
	}
	return id, nil
}

// Evaluate runs the entry module's body (and, by dependency order, the bodies
// of its dependencies). Each module body runs at most once per loader, even in
// the presence of cycles or repeated entry points. A failed module sticks: its
// error is cached and re-attempting it through a different import order throws
// the same failure rather than silently re-running or succeeding.
//
// The returned value is the completion value of the entry module's body (often
// undefined); use ModuleNamespace to read the exports.
func (l *ModuleLoader) Evaluate(entryID string) (Value, error) {
	m, ok := l.modules[entryID]
	if !ok {
		return nil, &LinkError{ID: entryID, Msg: "module has not been parsed"}
	}
	if m.status < statusInstantiated {
		if _, err := l.instantiate(entryID); err != nil {
			return nil, err
		}
	}

	// Depth-first post-order evaluation over the dependency graph.
	var result Value
	var evalErr error
	var visit func(m *moduleRecord)
	visit = func(m *moduleRecord) {
		if evalErr != nil {
			return
		}
		switch m.status {
		case statusEvaluated:
			result = nil
			return
		case statusErrored:
			evalErr = m.evalError
			return
		case statusEvaluating:
			// A cycle leads back to a module whose body is mid-flight. Its
			// bindings are in place; reading a not-yet-initialised one throws.
			return
		}

		m.status = statusEvaluating
		for _, dep := range m.orderedDeps() {
			visit(dep)
			if evalErr != nil {
				m.fail(evalErr)
				return
			}
		}

		v, err := m.runBody()
		if err != nil {
			m.fail(err)
			evalErr = err
			return
		}
		m.status = statusEvaluated
		result = v
	}
	visit(m)
	if evalErr != nil {
		return nil, evalErr
	}
	return result, nil
}

// RunModule is a convenience for parse + instantiate + evaluate in one call.
func (l *ModuleLoader) RunModule(entrySpecifier string) (Value, error) {
	id, err := l.Instantiate(entrySpecifier)
	if err != nil {
		return nil, err
	}
	return l.Evaluate(id)
}

func (m *moduleRecord) fail(err error) {
	m.status = statusErrored
	m.evalError = err
}

// orderedDeps returns the module's direct dependencies in the order they were
// first referenced (imports then re-exports, in source order). Evaluation order
// only affects completion values, but a stable order keeps behaviour
// predictable.
func (m *moduleRecord) orderedDeps() []*moduleRecord {
	return m.dependencies
}

// runBody obtains the outer wrapper (once), calls it with the export cell and
// import view to instantiate hoisted bindings, then invokes the evaluator it
// returns to run the module body.
func (m *moduleRecord) runBody() (Value, error) {
	r := m.loader.runtime
	if m.evaluator == nil {
		v, err := r.RunProgram(m.instantiateProgram)
		if err != nil {
			return nil, err
		}
		wrapper, ok := AssertFunction(v)
		if !ok {
			return nil, &LinkError{ID: m.id, Msg: "internal error: module did not yield a wrapper function"}
		}
		wv, err := wrapper(_undefined, m.cell, m.importView)
		if err != nil {
			return nil, err
		}
		callable, ok := AssertFunction(wv)
		if !ok {
			return nil, &LinkError{ID: m.id, Msg: "internal error: module wrapper did not return a function"}
		}
		m.evaluator = callable
	}
	// this is undefined in module scope; exports/imports come from closure.
	return m.evaluator(_undefined)
}

// ModuleNamespace returns a frozen-feeling namespace object whose own
// enumerable properties are live getters for the module's exported names,
// including "default". Re-exported names are included as well.
func (l *ModuleLoader) ModuleNamespace(id string) (*Object, error) {
	m, ok := l.modules[id]
	if !ok {
		return nil, &LinkError{ID: id, Msg: "module has not been parsed"}
	}
	if m.namespace != nil {
		return m.namespace, nil
	}
	r := l.runtime
	ns := r.NewObject()

	names := m.exportNames()
	for _, name := range names {
		n := name
		getter := r.newNativeFunc(func(FunctionCall) Value {
			tm, tn, ok := m.resolveExport(n, make(map[*moduleRecord]bool))
			if !ok {
				panic(r.newError(r.getReferenceError(), "Export '"+n.String()+"' is not present"))
			}
			return tm.cell.Get(tn.String())
		}, unistring.String("get "+n.String()), 0)
		if err := ns.DefineAccessorProperty(n.String(), getter, nil, FLAG_TRUE, FLAG_FALSE); err != nil {
			return nil, err
		}
	}
	m.namespace = ns
	return ns, nil
}

func (m *moduleRecord) exportNames() []unistring.String {
	seen := make(map[unistring.String]bool)
	var out []unistring.String
	for _, le := range m.meta.localExportOrder {
		if !seen[le.name] {
			seen[le.name] = true
			out = append(out, le.name)
		}
	}
	for _, ie := range m.indirectExports {
		if !seen[ie.exported] {
			seen[ie.exported] = true
			out = append(out, ie.exported)
		}
	}
	return out
}

var _ = unistring.String("")
