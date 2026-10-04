package goja

// moduleCell is one live ES module binding. A cell created by an exporter
// is shared with every importer: reads always observe the current value
// and the value starts in the temporal dead zone (v == nil).
type moduleCell struct {
	v Value
}

// moduleEnvironment holds the live bindings of an instantiated ES module.
// Own cells are created during instantiation; import cells are pointed at
// the exporter's cell during linking.
type moduleEnvironment struct {
	cells []*moduleCell
}

// get reads a cell, throwing a ReferenceError while it is in the TDZ.
func (c *moduleCell) get(vm *vm) Value {
	if c.v == nil {
		vm.throw(errAccessBeforeInit)
		return nil
	}
	return c.v
}

// pendingModuleEnv is the environment installed by the next enterModule.
// runModule sets it before executing a module program, so the compiled
// Program itself stays immutable and can be shared across runners.

// enterModule installs the module environment staged on the vm (cells are
// created during linking, before any evaluation).
type enterModule struct {
}

func (e *enterModule) exec(vm *vm) {
	vm.push(_undefined) // callee slot
	vm.push(nil)        // this
	vm.sb = vm.sp
	vm.moduleEnv = vm.pendingModuleEnv
	vm.pc++
}

// leaveModule ends module evaluation. The environment itself is kept on
// the module record and on functions that closed over it.
type leaveModule struct{}

func (leaveModule) exec(vm *vm) {
	vm.moduleEnv = nil
	vm.sp = vm.sb
	vm.pc = len(vm.prg.code) // halt the run loop
}

// loadCellLex reads a module cell (TDZ checked).
type loadCellLex struct {
	cell int
}

func (g loadCellLex) exec(vm *vm) {
	if vm.moduleEnv == nil {
		panic("Compiler bug: loadCellLex outside of a module environment")
	}
	vm.push(vm.moduleEnv.cells[g.cell].get(vm))
	vm.pc++
}

// loadCell reads a module cell without a TDZ check (used for module-level
// var bindings, which are initialised to undefined during instantiation).
type loadCell struct {
	cell int
}

func (g loadCell) exec(vm *vm) {
	if vm.moduleEnv == nil {
		panic("Compiler bug: loadCell outside of a module environment")
	}
	v := vm.moduleEnv.cells[g.cell].v
	if v == nil {
		v = _undefined
	}
	vm.push(v)
	vm.pc++
}

// storeCell writes a mutable module cell.
type storeCell struct {
	cell int
}

func (s storeCell) exec(vm *vm) {
	// Leave the value on the stack: this is the expression result variant
	// (used by '=' and ++/-- expressions that produce a value).
	vm.moduleEnv.cells[s.cell].v = vm.stack[vm.sp-1]
	vm.pc++
}

type storeCellP struct {
	cell int
}

func (s storeCellP) exec(vm *vm) {
	vm.moduleEnv.cells[s.cell].v = vm.stack[vm.sp-1]
	vm.pc++
}

// initCell initialises a module cell (consumes the value from the stack).
type initCell struct {
	cell int
}

func (s initCell) exec(vm *vm) {
	vm.moduleEnv.cells[s.cell].v = vm.stack[vm.sp-1]
	vm.pc++
}

type initCellP struct {
	cell int
}

func (s initCellP) exec(vm *vm) {
	vm.moduleEnv.cells[s.cell].v = vm.stack[vm.sp-1]
	vm.pc++
}
