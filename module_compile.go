package goja

// compile transforms the module AST and compiles it into a reusable Program.
// It runs during instantiation and never executes module statements.
func (m *moduleRecord) compile() error {
	prg, err := m.buildProgram()
	if err != nil {
		return err
	}
	compiled, err := compileAST(prg, true, true, nil)
	if err != nil {
		return &ModuleSyntaxError{ID: m.id, Err: err}
	}
	m.instantiateProgram = compiled
	return nil
}
