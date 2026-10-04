package goja

import (
	"github.com/dop251/goja/unistring"
)

// validateModuleScopes checks single-module static rules that do not depend on
// other modules, turning them into pre-execution LinkErrors:
//
//   - an imported local name must not be imported twice in the same module;
//   - an imported local name must not collide with a var/let/const/function/
//     class declared at module top level.
//
// Duplicate exports and missing exports are handled elsewhere.
func validateModuleScopes(m *moduleRecord) error {
	bindings := make(map[unistring.String]bool)

	bindImport := func(name unistring.String) error {
		if name == "" {
			return nil
		}
		if bindings[name] {
			return &LinkError{
				ID:  m.id,
				Msg: "Identifier '" + name.String() + "' has already been declared",
			}
		}
		bindings[name] = true
		return nil
	}

	for _, imp := range m.meta.imports {
		if err := bindImport(imp.defaults); err != nil {
			return err
		}
		for _, n := range imp.named {
			if err := bindImport(n.local); err != nil {
				return err
			}
		}
	}

	for name := range collectLocalNonImportNames(m.program) {
		if bindings[name] {
			return &LinkError{
				ID:  m.id,
				Msg: "Identifier '" + name.String() + "' has already been declared",
			}
		}
	}

	return nil
}
