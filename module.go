// ES module support for goja (static imports/exports, v1).
//
// This file implements a deliberately scoped subset of ECMAScript modules:
// named import/export, default export and explicit re-export. Module source is
// provided by the host through callbacks; nothing is read from disk or the
// network by this package. Parsing, instantiation (linking) and evaluation are
// separate, explicit phases. See module.md for the full v1 syntax boundary.
package goja

import (
	"errors"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/file"
	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/unistring"
)

// ModuleResolveFunc maps a module specifier as it appears in an import or
// re-export statement to a canonical module ID. Referrer is the canonical ID of
// the module containing the statement (empty string for the entry module).
//
// The canonical ID is the only identity a module has: two specifiers (aliases)
// that resolve to the same ID share one module instance, one set of live
// bindings and one evaluation result.
type ModuleResolveFunc func(referrer, specifier string) (id string, err error)

// ModuleLoadFunc returns the source text of a module given its canonical ID.
// The host decides where the source comes from; the runtime never touches the
// file system or network itself.
type ModuleLoadFunc func(id string) (src string, err error)

// ModuleLoader parses, links and evaluates a graph of ES modules inside a
// single Runtime. A loader is safe to reuse for multiple independent entry
// modules; every canonical ID is evaluated at most once per loader.
type ModuleLoader struct {
	runtime *Runtime
	resolve ModuleResolveFunc
	load    ModuleLoadFunc

	modules map[string]*moduleRecord
}

// NewModuleLoader creates a ModuleLoader bound to rt. resolve maps specifiers
// to canonical IDs (if nil, the specifier itself is used as the ID, which still
// gives aliasing whenever the same string is imported). load returns the
// module source for a canonical ID.
func NewModuleLoader(rt *Runtime, resolve ModuleResolveFunc, load ModuleLoadFunc) *ModuleLoader {
	if load == nil {
		panic("goja: NewModuleLoader requires a non-nil load callback")
	}
	return &ModuleLoader{
		runtime: rt,
		resolve: resolve,
		load:    load,
		modules: make(map[string]*moduleRecord),
	}
}

// ErrModuleNotFound is returned (wrapped) when the host load callback reports
// that a canonical module ID has no source.
var ErrModuleNotFound = errors.New("module not found")

type moduleStatus byte

const (
	statusNew moduleStatus = iota
	statusParsed
	statusInstantiated
	statusEvaluating
	statusEvaluated
	statusErrored
)

// bindingKind mirrors the declaration kind of an exported local binding. It
// controls the temporal dead zone and whether reassignment is allowed.
type bindingKind byte

const (
	bindVar     bindingKind = iota // hoisted, initialised to undefined
	bindLet                        // TDZ until initialised, reassignable
	bindConst                      // TDZ until initialised, read-only afterwards
	bindFunc                       // hoisted function, reassignable
	bindClass                      // TDZ until the class statement runs
	bindDefault                    // export default <expression>
	bindImport                     // imported binding (read-only, TDZ owned by exporter)
)

// localExport describes a binding produced by this module's own code and
// exposed under an exported name. The exported name "default" holds the
// default export.
type localExport struct {
	name unistring.String // exported name
	kind bindingKind
}

// indirectExport describes a single name re-exported from another module:
// export { imported as exported } from "spec".
type indirectExport struct {
	exported unistring.String // name exposed by this module
	imported unistring.String // name read from the target module ("default" ok)
	target   *moduleRecord
}

// moduleRecord is the parsed and, later, linked/evaluated state of one module.
type moduleRecord struct {
	id        string
	loader    *ModuleLoader
	program   *ast.Program
	status    moduleStatus
	evalError error // sticky once status == statusErrored
	linked    bool

	// importView is the object the module body reads its imported bindings
	// from. Every property is a getter-only accessor into the exporter's cell.
	importView *Object

	// cell is this module's own exports object while the body runs. The body
	// writes exported locals here; importers receive accessors over it.
	cell *Object

	// backing storage implementing the temporal dead zone for lexically
	// exported bindings.
	tz *tdz

	indirectExports []indirectExport
	dependencies    []*moduleRecord
	meta            *collectedMeta

	// instantiateProgram, when run, returns the evaluator function. It is
	// produced by the transform step during instantiation.
	instantiateProgram *Program

	evaluator Callable

	// namespace object lazily built for host consumption.
	namespace *Object
}

// ParseModule parses the entry module and every module reachable from it via
// static imports and explicit re-exports. No module code is executed; only the
// host resolve/load callbacks are invoked. ParseModule is idempotent for IDs
// that have already been parsed.
func (l *ModuleLoader) ParseModule(entrySpecifier string) (string, error) {
	return l.parseEntry(entrySpecifier)
}

func (l *ModuleLoader) parseEntry(entrySpecifier string) (string, error) {
	id, err := l.canonical("", entrySpecifier)
	if err != nil {
		return "", err
	}
	if _, err := l.getOrParse(id); err != nil {
		return "", err
	}
	return id, nil
}

func (l *ModuleLoader) canonical(referrer, specifier string) (string, error) {
	if l.resolve != nil {
		id, err := l.resolve(referrer, specifier)
		if err != nil {
			return "", err
		}
		return id, nil
	}
	return specifier, nil
}

// getOrParse fetches and parses the module identified by id, recursively
// parsing its dependencies. A partially parsed graph is left in place but the
// returned error prevents instantiation.
func (l *ModuleLoader) getOrParse(id string) (*moduleRecord, error) {
	if m, ok := l.modules[id]; ok {
		return m, nil
	}

	src, err := l.load(id)
	if err != nil {
		return nil, err
	}

	prg, err := parser.ParseModule(nil, id, src, l.runtime.parserOptions...)
	if err != nil {
		return nil, &ModuleSyntaxError{ID: id, Err: err}
	}

	m := &moduleRecord{
		id:      id,
		loader:  l,
		program: prg,
		status:  statusNew,
		tz:      newTdz(),
	}
	l.modules[id] = m // reserve before recursion so cycles terminate

	if err := l.collectModuleMetaData(m); err != nil {
		return nil, err
	}
	if err := validateModuleScopes(m); err != nil {
		return nil, err
	}

	m.status = statusParsed
	return m, nil
}

// ModuleSyntaxError wraps a parse error together with the canonical module ID.
type ModuleSyntaxError struct {
	ID  string
	Err error
}

func (e *ModuleSyntaxError) Error() string {
	return "SyntaxError in module " + e.ID + ": " + e.Err.Error()
}

func (e *ModuleSyntaxError) Unwrap() error { return e.Err }

// LinkError reports a failure during instantiation (e.g. a missing export).
type LinkError struct {
	ID  string
	Msg string
}

func (e *LinkError) Error() string {
	if e.ID == "" {
		return "LinkError: " + e.Msg
	}
	return "LinkError in module " + e.ID + ": " + e.Msg
}

var _ = file.Idx(0)
