package goja

// finaliseModuleAlloc finalises variable allocation for functions nested
// inside a module. Module top-level bindings themselves use dedicated
// module-cell opcodes (see binding.emitGet/emitInit/emitSet) and therefore
// take no space on the value stack or in ordinary stashes.
func (s *scope) finaliseModuleAlloc() {
	for _, nested := range s.nested {
		nested.finaliseVarAlloc(0)
	}
}
