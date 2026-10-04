package goja

import (
	js_ast "github.com/dop251/goja/ast"
	js_parser "github.com/dop251/goja/parser"
)

func parseModule(name, src string, opts ...js_parser.Option) (*js_ast.Program, error) {
	return js_parser.ParseModule(name, src, opts...)
}

// runModule executes an instantiated module body once with its linked
// environment installed. The compiled program's first instruction is an
// enterModule carrying the environment.
func (r *Runtime) runModule(m *moduleInstance) error {
	vm := r.vm
	vm.pendingModuleEnv = m.env
	defer func() { vm.pendingModuleEnv = nil }()

	recursive := len(vm.callStack) > 0
	var runErr error
	cleaned := false
	cleanup := func() {
		if cleaned {
			return
		}
		cleaned = true
		if recursive {
			if vm.sp >= 2 {
				vm.sp -= 2
			}
			if len(vm.callStack) > 0 {
				vm.popCtx()
			}
		} else if len(vm.callStack) > 0 {
			vm.callStack = vm.callStack[:len(vm.callStack)-1]
		}
	}
	defer func() {
		cleanup()
		if x := recover(); x != nil {
			if ex := asUncatchableException(x); ex != nil {
				runErr = ex
				if len(vm.callStack) == 0 {
					r.leaveAbrupt()
				}
			} else {
				panic(x)
			}
		}
	}()

	if recursive {
		vm.pushCtx()
		vm.stash = &r.global.stash
		vm.privEnv = nil
		vm.moduleEnv = nil
		vm.newTarget = nil
		vm.args = 0
	} else {
		vm.callStack = append(vm.callStack, context{})
	}
	vm.prg = m.prg
	vm.pc = 0
	vm.result = _undefined
	ex := vm.runTry()
	if ex != nil {
		vm.clearStack()
		if !recursive {
			vm.prg = nil
			vm.sb = -1
			r.leave()
		}
		runErr = ex
		cleanup()
		return runErr
	}
	if !recursive {
		vm.prg = nil
		vm.sb = -1
		r.leave()
	}
	cleanup()
	return runErr
}
