package goja

import (
	"fmt"

	"github.com/dop251/goja/unistring"
)

// ModuleSourceLoader is the host callback that supplies module source
// code. It receives the canonical ID of the importing module ("" for the
// entry point) and the specifier text written after from, and returns the
// source together with the canonical ID of the resolved module.
//
// Several specifiers (including aliases used by different importers) may
// resolve to the same canonical ID; in that case they share a single
// parsed, instantiated and evaluated module instance.
//
// The runtime never accesses the file system or the network on its own:
// all resolution and retrieval goes through this callback.
type ModuleSourceLoader func(referrerCanonical, specifier string) (canonicalID, source string, err error)

// CompileModule parses and compiles src as an ES module without linking
// or running it.
func (r *Runtime) CompileModule(name, src string) (*Program, error) {
	return r.compileModule(name, src)
}

func (r *Runtime) compileModule(name, src string) (p *Program, err error) {
	astPrg, err1 := parseModule(name, src, r.parserOptions...)
	if err1 != nil {
		return nil, &CompilerSyntaxError{
			CompilerError: CompilerError{Message: err1.Error()},
		}
	}
	p, err = compileModuleAST(astPrg)
	if err != nil {
		if x, ok := err.(*CompilerSyntaxError); ok {
			return nil, &Exception{
				val: r.builtin_new(r.getSyntaxError(), []Value{newStringValue(x.Error())}),
			}
		}
	}
	return p, err
}

type moduleState uint8

const (
	moduleLinked moduleState = iota // instantiated but not yet evaluated
	moduleEvaluating
	moduleEvaluated
	moduleErrored
)

// moduleInstance is a linked, at-most-once-evaluated module.
type moduleInstance struct {
	id  string
	prg *Program
	env *moduleEnvironment

	state moduleState
	err   *Exception // sticky evaluation error

	deps []*moduleInstance // evaluation dependencies (unique)
}

// ModuleRunner drives the explicit parse / link / evaluate phases for a
// module graph. A runner is bound to one Runtime and one source loader.
type ModuleRunner struct {
	r      *Runtime
	loader ModuleSourceLoader

	modules map[string]*moduleInstance
}

// NewModuleRunner creates a ModuleRunner bound to the runtime.
func (r *Runtime) NewModuleRunner(loader ModuleSourceLoader) *ModuleRunner {
	return &ModuleRunner{
		r:       r,
		loader:  loader,
		modules: make(map[string]*moduleInstance),
	}
}

// ResolvedModule describes the result of resolving and instantiating an
// entry module.
type ResolvedModule struct {
	entry *moduleInstance
	mr    *ModuleRunner
}

// Link resolves the entry module and its whole dependency graph, compiles
// every module (parse phase), instantiates environments and wires live
// bindings (link phase). No module body is executed. Syntax errors,
// resolution failures and missing exports are reported here.
func (mr *ModuleRunner) Link(entrySpecifier string) (*ResolvedModule, error) {
	entry, err := mr.fetch("", entrySpecifier)
	if err != nil {
		return nil, err
	}
	if err := mr.instantiate(entry, map[*moduleInstance]bool{}); err != nil {
		return nil, err
	}
	return &ResolvedModule{entry: entry, mr: mr}, nil
}

// Evaluate executes the module graph in post-order (evaluate phase). It
// links first if necessary. Each module body runs at most once; a module
// that throws is marked errored and is never run again, regardless of the
// import order used to reach it.
func (rm *ResolvedModule) Evaluate() (*Object, error) {
	if err := rm.mr.evaluate(rm.entry); err != nil {
		return nil, err
	}
	return rm.mr.buildNamespace(rm.entry), nil
}

// RunModule is a convenience combining Link and Evaluate.
func (mr *ModuleRunner) RunModule(entrySpecifier string) (*Object, error) {
	rm, err := mr.Link(entrySpecifier)
	if err != nil {
		return nil, err
	}
	return rm.Evaluate()
}

// fetch parses (if needed) and registers the module identified by
// specifier as seen from referrer's canonical ID.
func (mr *ModuleRunner) fetch(referrer, specifier string) (*moduleInstance, error) {
	id, src, err := mr.loader(referrer, specifier)
	if err != nil {
		return nil, err
	}
	if m, ok := mr.modules[id]; ok {
		return m, nil
	}
	p, err := mr.r.compileModule(id, src)
	if err != nil {
		return nil, err
	}
	m := &moduleInstance{id: id, prg: p}
	mr.modules[id] = m
	return m, nil
}

// instantiate parses the whole graph (via fetch) and then creates
// environments and wires live bindings. It proceeds in two passes over the
// post-order traversal: dependencies are visited first, so when a module's
// cells are linked the environments of all its imports already exist —
// including in the presence of cycles (an edge to an ancestor is simply
// resolved against the ancestor's already-allocated environment).
func (mr *ModuleRunner) instantiate(m *moduleInstance, visiting map[*moduleInstance]bool) error {
	return mr.instantiateDFS(m, visiting, map[*moduleInstance]bool{})
}

func (mr *ModuleRunner) instantiateDFS(m *moduleInstance, visiting, done map[*moduleInstance]bool) error {
	if m.env != nil || done[m] {
		return nil
	}
	if visiting[m] {
		return nil // cycle edge: ancestor is already being instantiated
	}
	visiting[m] = true

	meta := m.prg.moduleMeta

	// Create this module's environment before descending so that cycle edges
	// from descendants can resolve against it. Cells start in the TDZ.
	if m.env == nil {
		env := &moduleEnvironment{cells: make([]*moduleCell, meta.numCells)}
		for i := range env.cells {
			c := &moduleCell{}
			if meta.varCells[i] {
				c.v = _undefined
			}
			env.cells[i] = c
		}
		m.env = env
	}

	// Pass 1: resolve (parse) every dependency and recurse.
	depSet := map[*moduleInstance]bool{}
	var orderedDeps []*moduleInstance
	addDep := func(dep *moduleInstance) {
		if !depSet[dep] {
			depSet[dep] = true
			orderedDeps = append(orderedDeps, dep)
		}
	}
	for _, req := range meta.imports {
		dep, err := mr.fetch(m.id, req.specifier)
		if err != nil {
			return err
		}
		if err := mr.instantiateDFS(dep, visiting, done); err != nil {
			return err
		}
		addDep(dep)
	}
	for _, re := range meta.reExports {
		dep, err := mr.fetch(m.id, re.specifier)
		if err != nil {
			return err
		}
		if err := mr.instantiateDFS(dep, visiting, done); err != nil {
			return err
		}
		addDep(dep)
	}

	// Pass 2: link cells. Dependency environments exist by now; cycle edges
	// point at ancestors whose environments were hoisted above.
	for _, req := range meta.imports {
		dep, _ := mr.fetch(m.id, req.specifier)
		for _, b := range req.bindings {
			cell, err := mr.resolveExport(dep, string(b.imported))
			if err != nil {
				return err
			}
			m.env.cells[m.cellIndex(string(b.local))] = cell
		}
	}
	for _, re := range meta.reExports {
		dep, _ := mr.fetch(m.id, re.specifier)
		for _, b := range re.bindings {
			cell, err := mr.resolveExport(dep, string(b.local))
			if err != nil {
				return err
			}
			idx := mr.reExportCell(m, string(b.exported))
			m.env.cells[idx] = cell
		}
	}

	visiting[m] = false
	done[m] = true
	m.deps = orderedDeps
	return nil
}

// cellIndex returns the cell index of a local binding by name. It relies
// on the compiler's binding order (scope.bindings), which is exposed
// through moduleMeta via a name->index table.
func (m *moduleInstance) cellIndex(name string) int {
	return m.prg.moduleMeta.cellIndex[unistring.String(name)]
}

// reExportCell allocates (once) a dedicated cell slot used to hold an
// aliased re-export. Re-export specifiers do not correspond to local
// bindings, so extra slots are appended beyond the declared cells.
func (mr *ModuleRunner) reExportCell(m *moduleInstance, exported string) int {
	meta := m.prg.moduleMeta
	if idx, ok := meta.cellIndex[unistring.String(exported)]; ok {
		return idx
	}
	idx := len(m.env.cells)
	m.env.cells = append(m.env.cells, &moduleCell{})
	meta.cellIndex[unistring.String(exported)] = idx
	return idx
}

// resolveExport follows own exports and explicit re-export links to the
// live cell of exportedName in m.
func (mr *ModuleRunner) resolveExport(m *moduleInstance, exportedName string) (*moduleCell, error) {
	meta := m.prg.moduleMeta
	if cell, ok := meta.ownExports[unistring.String(exportedName)]; ok {
		if m.env == nil {
			return nil, fmt.Errorf("module %q not instantiated", m.id)
		}
		return m.env.cells[cell], nil
	}
	for _, re := range meta.reExports {
		for _, b := range re.bindings {
			if string(b.exported) == exportedName {
				dep, err := mr.fetch(m.id, re.specifier)
				if err != nil {
					return nil, err
				}
				return mr.resolveExport(dep, string(b.local))
			}
		}
	}
	return nil, fmt.Errorf("module %q does not export %q", m.id, exportedName)
}

func (mr *ModuleRunner) evaluate(m *moduleInstance) error {
	switch m.state {
	case moduleEvaluated:
		return nil
	case moduleErrored:
		return &stickyModuleError{id: m.id, cause: m.err}
	case moduleEvaluating:
		// circular evaluation: let the outer invocation finish it
		return nil
	}
	m.state = moduleEvaluating

	for _, dep := range m.deps {
		if err := mr.evaluate(dep); err != nil {
			m.state = moduleErrored
			if ex, ok := err.(*stickyModuleError); ok {
				m.err = ex.cause
			}
			return err
		}
	}

	err := mr.r.runModule(m)
	if err != nil {
		if ex, ok := err.(*Exception); ok {
			m.state = moduleErrored
			m.err = ex
			return &stickyModuleError{id: m.id, cause: m.err}
		}
		// Non-JavaScript errors (interrupts, stack overflow, host panics)
		// do not poison the module graph: leave the module evaluable again.
		m.state = moduleLinked
		return err
	}
	m.state = moduleEvaluated
	return nil
}

// buildNamespace creates the module namespace object (frozen, with live
// getters) returned to Go callers.
func (mr *ModuleRunner) buildNamespace(m *moduleInstance) *Object {
	o := mr.r.NewObject()
	for name := range m.prg.moduleMeta.ownExports {
		name := name
		cell, _ := mr.resolveExport(m, string(name))
		getter := mr.r.newNativeFunc(func(FunctionCall) Value {
			return cell.get(mr.r.vm)
		}, unistring.String("get "+name.String()), 0)
		_ = o.DefineAccessorProperty(name.String(), getter, nil, FLAG_TRUE, FLAG_TRUE)
	}
	_ = o.SetSymbol(SymToStringTag, newStringValue("Module"))
	r2 := mr.r
	r2.object_freeze(FunctionCall{Arguments: []Value{o}})
	return o
}

type stickyModuleError struct {
	id    string
	cause *Exception
}

func (e *stickyModuleError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("module %q has previously failed to evaluate: %v", e.id, e.cause)
	}
	return fmt.Sprintf("module %q has previously failed to evaluate", e.id)
}

func (e *stickyModuleError) Unwrap() error { return e.cause }

// Cause returns the original JavaScript evaluation error.
func (e *stickyModuleError) Cause() error { return e.cause }
