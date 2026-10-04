package goja

import "testing"

// Explicit re-export 'export { x } from' preserves live bindings.
func TestModuleReExport(t *testing.T) {
	sources := map[string]string{
		"dep.js": `export let v = 1; export function bump(){ return ++v; }`,
		"mid.js": `export { v, bump as next } from "dep.js";`,
		"main.js": `import { v, next } from "mid.js";
export function run(){ return [v, next(), v]; }`,
	}
	r, mr := newTestRunner(t, sources)
	ns, err := mr.RunModule("main.js")
	if err != nil {
		t.Fatal(err)
	}
	r.Set("ns", ns)
	if _, err := r.RunString(`
		if ("" + ns.run() !== "1,2,2") throw new Error("reexport live binding broken");
	`); err != nil {
		t.Fatal(err)
	}
}

// Default import and named default export interop.
func TestModuleDefaultImport(t *testing.T) {
	sources := map[string]string{
		"greet.js": `export default function greet(name){ return "hi " + name; }`,
		"main.js": `import greet, { default as g2 } from "greet.js";
export default greet("a") + "/" + g2("b");`,
	}
	_, mr := newTestRunner(t, sources)
	ns, err := mr.RunModule("main.js")
	if err != nil {
		t.Fatal(err)
	}
	if ns.Get("default").String() != "hi a/hi b" {
		t.Fatalf("got %v", ns.Get("default"))
	}
}

// A missing export is reported during Link, before evaluation.
func TestModuleMissingExportRejectedAtLink(t *testing.T) {
	sources := map[string]string{
		"dep.js":  `export const a = 1;`,
		"main.js": `import { nope } from "dep.js"; export default nope;`,
	}
	evaluated := false
	r := New()
	mr := r.NewModuleRunner(func(referrer, specifier string) (string, string, error) {
		return specifier, sources[specifier], nil
	})
	rm, err := mr.Link("main.js")
	if err == nil {
		_ = rm
		evaluated = true
		t.Fatal("expected link error for missing export")
	}
	if evaluated {
		t.Fatal("modules must not be evaluated on link failure")
	}
}

// Duplicate exports are rejected before execution.
func TestModuleDuplicateExport(t *testing.T) {
	r := New()
	if _, err := r.CompileModule("m.js", `let a=1,b=2; export { a, b as a };`); err == nil {
		t.Fatal("expected duplicate export error")
	}
}

// Module code runs in strict mode.
func TestModuleStrictMode(t *testing.T) {
	sources := map[string]string{
		"main.js": `export default function(){ undeclared = 1; return typeof undeclared; }`,
	}
	_, mr := newTestRunner(t, sources)
	ns, err := mr.RunModule("main.js")
	if err != nil {
		t.Fatal(err)
	}
	fn, _ := AssertFunction(ns.Get("default"))
	if _, err := fn(nil); err == nil {
		t.Fatal("expected strict-mode ReferenceError")
	}
}

// Three-phase separation: Link does not run code; Evaluate runs it once.
func TestModuleLinkDoesNotEvaluate(t *testing.T) {
	runs := 0
	sources := map[string]string{
		"main.js": `export const v = 42;`,
	}
	r := New()
	_ = runs
	mr := r.NewModuleRunner(func(referrer, specifier string) (string, string, error) {
		return specifier, sources[specifier], nil
	})
	rm, err := mr.Link("main.js")
	if err != nil {
		t.Fatal(err)
	}
	if rm.entry.env == nil || rm.entry.env.cells[0].v != nil {
		t.Fatal("binding should exist but be uninitialised after Link")
	}
	ns, err := rm.Evaluate()
	if err != nil {
		t.Fatal(err)
	}
	if ns.Get("v").ToInteger() != 42 {
		t.Fatalf("got %v", ns.Get("v"))
	}
}

// Default export of anonymous and named classes.
func TestModuleDefaultClass(t *testing.T) {
	sources := map[string]string{
		"anon.js":  `export default class { hi(){ return 5; } }`,
		"named.js": `export default class CC { static z(){ return 9; } }`,
		"m.js": `import A from "anon.js";
import C from "named.js";
export default new A().hi() + C.z();`,
	}
	_, mr := newTestRunner(t, sources)
	ns, err := mr.RunModule("m.js")
	if err != nil {
		t.Fatal(err)
	}
	if ns.Get("default").ToInteger() != 14 {
		t.Fatalf("got %v", ns.Get("default"))
	}
}

// Top-level 'this' in a module is undefined (ES semantics).
func TestModuleTopLevelThisUndefined(t *testing.T) {
	sources := map[string]string{"m.js": `export const t = this;`}
	_, mr := newTestRunner(t, sources)
	ns, err := mr.RunModule("m.js")
	if err != nil {
		t.Fatal(err)
	}
	if v := ns.Get("t"); v != nil && v != _undefined {
		t.Fatalf("top-level this should be undefined, got %v", v)
	}
}

// Module-level vars do not leak onto the global object.
func TestModuleVarScoped(t *testing.T) {
	sources := map[string]string{"m.js": `var secret = 42;
export const s = secret;
export var e = 1;`}
	r, mr := newTestRunner(t, sources)
	ns, err := mr.RunModule("m.js")
	if err != nil {
		t.Fatal(err)
	}
	if ns.Get("s").ToInteger() != 42 || ns.Get("e").ToInteger() != 1 {
		t.Fatalf("unexpected exports: %v %v", ns.Get("s"), ns.Get("e"))
	}
	if g, _ := r.RunString(`typeof secret`); g.String() != "undefined" {
		t.Fatalf("module var leaked onto global: %v", g)
	}
}

// Async and generator functions declared in a module close over its cells.
func TestModuleAsyncAndGenerator(t *testing.T) {
	sources := map[string]string{
		"m.js": `export let v = 10;
export async function av() { return v + 1; }
export function* gen() { yield v; yield v + 1; }`,
	}
	r, mr := newTestRunner(t, sources)
	ns, err := mr.RunModule("m.js")
	if err != nil {
		t.Fatal(err)
	}
	r.Set("ns", ns)
	if _, err := r.RunString(`
		const g = ns.gen();
		const a = g.next().value, b = g.next().value;
		if (a !== 10 || b !== 11) throw new Error(a + "," + b);
		const p = ns.av();
		if (!(p instanceof Promise)) throw new Error("expected Promise");
	`); err != nil {
		t.Fatal(err)
	}
}

// The module namespace is frozen and reports @@toStringTag "Module".
func TestModuleNamespaceFrozen(t *testing.T) {
	sources := map[string]string{"m.js": `export const a = 1;`}
	r, mr := newTestRunner(t, sources)
	ns, err := mr.RunModule("m.js")
	if err != nil {
		t.Fatal(err)
	}
	r.Set("ns", ns)
	if _, err := r.RunString(`
		if (!Object.isFrozen(ns)) throw new Error("namespace not frozen");
		if (ns[Symbol.toStringTag] !== "Module") throw new Error("toStringTag");
	`); err != nil {
		t.Fatal(err)
	}
}
