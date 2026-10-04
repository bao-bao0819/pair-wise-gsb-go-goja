package goja

import "github.com/dop251/goja/unistring"

// moduleImportRequest is a single import edge discovered while compiling a
// module.
type moduleImportRequest struct {
	specifier string
	// bindings maps the local name to the exported name in the requested
	// module. Local "default" entries hold the imported default binding.
	bindings []moduleImportBinding
	// sideEffectOnly is true for 'import "mod";'.
	sideEffectOnly bool
}

type moduleImportBinding struct {
	local    unistring.String
	imported unistring.String
}

// moduleExportEntry describes one local binding exported by a module.
type moduleExportEntry struct {
	// local is the name of the local (cell) binding. For re-exported
	// bindings it is empty.
	local unistring.String
	// cell is the module cell index for locally defined exports.
	cell int
	// exported is the public name.
	exported unistring.String
}

// moduleReExport is an 'export { ... } from "mod";' edge.
type moduleReExport struct {
	specifier string
	bindings  []moduleReExportBinding
}

type moduleReExportBinding struct {
	local    unistring.String // name in the source module
	exported unistring.String // public name in this module
}

// moduleMeta is the compile-time module description attached to a Program.
// It is consumed by the Runtime's module linker to wire up live bindings.
type moduleMeta struct {
	// numCells is the number of module cells (own exports plus imports).
	numCells int

	// cellIndex maps every local binding name to its cell index.
	cellIndex map[unistring.String]int

	// ownExports maps exported name -> local cell index. The "default"
	// key holds the default export.
	ownExports map[unistring.String]int

	imports   []moduleImportRequest
	reExports []moduleReExport

	// varCells are initialised to undefined during instantiation and are
	// not subject to the temporal dead zone.
	varCells map[int]bool
}
