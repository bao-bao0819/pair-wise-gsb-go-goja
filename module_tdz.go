package goja

import (
	"github.com/dop251/goja/unistring"
)

// tdz is the per-module backing storage for exported lexical bindings. It
// implements the ECMAScript temporal dead zone: reading a binding before its
// initialiser has run throws a ReferenceError, exactly like a module-level let/
// const/class accessed too early through a cycle.
type tdz struct {
	initialized map[unistring.String]bool
	values      map[unistring.String]Value
	// frozen marks const-style bindings that reject reassignment.
	frozen map[unistring.String]bool
}

func newTdz() *tdz {
	return &tdz{
		initialized: make(map[unistring.String]bool),
		values:      make(map[unistring.String]Value),
		frozen:      make(map[unistring.String]bool),
	}
}

// predeclare creates the slot. Hoisted kinds (var/function) start out
// initialised; lexical kinds (let/const/class/default-expression) stay in the
// TDZ until Init is called as their initialiser executes.
func (t *tdz) predeclare(name unistring.String, kind bindingKind) {
	switch kind {
	case bindVar, bindFunc:
		t.initialized[name] = true
		t.values[name] = _undefined
	default:
		t.initialized[name] = false
	}
	if kind == bindConst || kind == bindDefault {
		t.frozen[name] = true
	}
}

func (t *tdz) get(name unistring.String) (Value, bool) {
	if !t.initialized[name] {
		return nil, false
	}
	return t.values[name], true
}

// init runs a lexical initialiser, taking the binding out of the TDZ.
func (t *tdz) init(name unistring.String, v Value) {
	t.initialized[name] = true
	t.values[name] = v
}

// set updates a mutable binding. It panics with a *TypeError (caught by the
// runtime) for const bindings after initialisation.
func (t *tdz) set(r *Runtime, name unistring.String, v Value) {
	runtimeSetTdz(t, r, name, v)
}

// runtimeSetTdz is split out so the rule is easy to read: const and
// default-expression slots accept exactly one write (the initialiser) while
// in the TDZ; once initialised they reject every further assignment.
func runtimeSetTdz(t *tdz, r *Runtime, name unistring.String, v Value) {
	if t.frozen[name] && t.initialized[name] {
		panic(r.NewTypeError("Assignment to constant exported variable."))
	}
	t.initialized[name] = true
	t.values[name] = v
}
