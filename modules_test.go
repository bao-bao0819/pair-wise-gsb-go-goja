package goja

import (
	"errors"
	"strings"
	"testing"
)

func newTestRunner(t *testing.T, sources map[string]string) (*Runtime, *ModuleRunner) {
	t.Helper()
	r := New()
	mr := r.NewModuleRunner(func(referrer, specifier string) (string, string, error) {
		return specifier, sources[specifier], nil
	})
	return r, mr
}

// Two modules importing each other; both can observe the other's live
// bindings once evaluation completes.
func TestModuleCircularImports(t *testing.T) {
	sources := map[string]string{
		"a.js": `import { b } from "b.js";
export let a = 1;
export function getB() { return b; }
export function setA(v) { a = v; }
`,
		"b.js": `import { a, setA } from "a.js";
export let b = 2;
export function sum() { return a + b; }
export function bump() { setA(a + 10); return a; }
`,
		"main.js": `import { getB } from "a.js";
import { sum, bump } from "b.js";
export function results() {
	return [getB(), sum(), bump(), sum()];
}
`,
	}
	r, mr := newTestRunner(t, sources)
	ns, err := mr.RunModule("main.js")
	if err != nil {
		t.Fatal(err)
	}
	r.Set("ns", ns)
	if _, err := r.RunString(`
		const got = "" + ns.results();
		if (got !== "2,3,11,13") {
			throw new Error("unexpected circular results: " + got);
		}
	`); err != nil {
		t.Fatal(err)
	}
}

// A mutable exported variable is observed changing by a third module.
func TestModuleMutableExportReadRepeatedly(t *testing.T) {
	sources := map[string]string{
		"counter.js": `export let count = 0;
export function inc() { count = count + 1; return count; }
export function get() { return count; }
`,
		"reader.js": `import { count, inc, get } from "counter.js";
export function reads() {
	return [count, inc(), count, inc(), get(), count];
}
`,
		"main.js": `import { reads } from "reader.js";
export default reads;
`,
	}
	r, mr := newTestRunner(t, sources)
	ns, err := mr.RunModule("main.js")
	if err != nil {
		t.Fatal(err)
	}
	r.Set("ns", ns)
	_, err = r.RunString(`
		const arr = ns.default();
		if ("" + arr !== "0,1,1,2,2,2") {
			throw new Error("unexpected reads: " + arr);
		}
	`)
	if err != nil {
		t.Fatal(err)
	}
}

// Reading an imported binding before it is initialised in a cycle throws.
// During evaluation 'a' calls a function from 'b' that reads 'a.value'
// before its initialiser has run.
func TestModuleCycleTDZ(t *testing.T) {
	sources := map[string]string{
		"a.js": `import { b } from "b.js";
export let a = b + 1;
export function readB() { return b; }
`,
		"b.js": `import { readB } from "a.js";
export let b = readB();
`,
		"main.js": `import "b.js";
`,
	}
	_, mr := newTestRunner(t, sources)
	_, err := mr.RunModule("main.js")
	if err == nil {
		t.Fatal("expected TDZ error")
	}
	msg := err.Error()
	if u := errors.Unwrap(err); u != nil {
		msg += " " + u.Error()
	}
	if !strings.Contains(msg, "initializ") && !strings.Contains(msg, "ReferenceError") {
		t.Fatalf("expected a TDZ reference error, got: %v", err)
	}
}

// Importers cannot assign to an imported binding (compile-time rejection).
func TestModuleImportBindingReadOnly(t *testing.T) {
	sources := map[string]string{
		"dep.js": `export const v = 1;`,
		"main.js": `import { v } from "dep.js";
v = 2;
export default v;
`,
	}
	_, mr := newTestRunner(t, sources)
	if _, err := mr.RunModule("main.js"); err == nil {
		t.Fatal("expected error assigning to import")
	}
}

// Aliases that map to the same canonical ID share one module instance.
func TestModuleAliasSharesInstance(t *testing.T) {
	sources := map[string]string{
		"shared.js": `export let n = 0;
export function tick() { return ++n; }
`,
		"a.js": `import { tick } from "./alias-shared";
export const fromA = tick();
`,
		"b.js": `import { tick } from "./alias-shared";
export const fromB = tick();
`,
		"main.js": `import { fromA } from "a.js";
import { fromB } from "b.js";
export default fromA + "," + fromB;
`,
	}
	r := New()
	mr := r.NewModuleRunner(func(referrer, specifier string) (string, string, error) {
		if specifier == "./alias-shared" {
			return "shared.js", sources["shared.js"], nil
		}
		return specifier, sources[specifier], nil
	})
	ns, err := mr.RunModule("main.js")
	if err != nil {
		t.Fatal(err)
	}
	if v := ns.Get("default").String(); v != "1,2" {
		t.Fatalf("expected shared state 1,2 got %q", v)
	}
}

// A module that throws during evaluation is sticky: changing import order
// must not let it re-run.
func TestModuleFailedStateSticky(t *testing.T) {
	sources := map[string]string{
		"boom.js": `export let v = 0;
v = 1;
throw new Error("boom");
`,
		"use1.js": `import { v } from "boom.js";
export const a = v;
`,
		"use2.js": `import { v } from "boom.js";
export const b = v;
`,
		"main.js": `import { a } from "use1.js";
import { b } from "use2.js";
export default a + b;
`,
	}
	_, mr := newTestRunner(t, sources)
	if _, err := mr.RunModule("main.js"); err == nil {
		t.Fatal("expected first failure")
	}
	// Re-run from a different entry reaching the same failed module.
	if _, err := mr.RunModule("use2.js"); err == nil {
		t.Fatal("expected sticky failure on second run")
	}
}

// Ordinary script parsing and execution are unaffected.
func TestScriptUnaffected(t *testing.T) {
	r := New()
	v, err := r.RunString(`
		var a = 1;
		function add(x) { return x + a; }
		add(41);
	`)
	if err != nil {
		t.Fatal(err)
	}
	if v.ToInteger() != 42 {
		t.Fatalf("got %v", v)
	}
	// Module-only syntax stays rejected in ordinary scripts.
	if _, err := r.RunString(`export let x = 1;`); err == nil {
		t.Fatal("expected export to be rejected in a script")
	}
}

// Unsupported module syntax is rejected before any execution.
func TestModuleUnsupportedSyntax(t *testing.T) {
	r := New()
	mr := r.NewModuleRunner(func(referrer, specifier string) (string, string, error) {
		return specifier, "", nil
	})
	bad := map[string]string{
		"dyn":  `const p = import("x"); export default p;`,
		"tla":  `const x = await 1; export default x;`,
		"star": `export * from "x";`,
		"meta": `const u = import.meta.url; export default u;`,
	}
	srcs := map[string]string{
		"x": ``,
	}
	_ = srcs
	for id, src := range bad {
		sources := map[string]string{id: src, "x": `export default 1`}
		lmr := r.NewModuleRunner(func(referrer, specifier string) (string, string, error) {
			return specifier, sources[specifier], nil
		})
		if _, err := lmr.RunModule(id); err == nil {
			t.Errorf("%s: expected syntax rejection", id)
		}
	}
	_ = mr
}
