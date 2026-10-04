package goja

import (
	"strings"
	"testing"
)

func newTestLoader(t *testing.T, sources map[string]string, resolve ModuleResolveFunc) *ModuleLoader {
	t.Helper()
	vm := New()
	return NewModuleLoader(vm, resolve, func(id string) (string, error) {
		src, ok := sources[id]
		if !ok {
			return "", ErrModuleNotFound
		}
		return src, nil
	})
}

func runModules(t *testing.T, sources map[string]string, entry string) (*Runtime, *Object, Value, error) {
	t.Helper()
	l := newTestLoader(t, sources, nil)
	id, err := l.Instantiate(entry)
	if err != nil {
		return l.runtime, nil, nil, err
	}
	v, err := l.Evaluate(id)
	if err != nil {
		return l.runtime, nil, v, err
	}
	ns, err := l.ModuleNamespace(id)
	return l.runtime, ns, v, err
}

// Two modules importing each other, using functions hoisted through the cycle.
func TestModuleCircularTwoModules(t *testing.T) {
	sources := map[string]string{
		"a": `
import { b } from "b";
export function a(n) { return n <= 0 ? 0 : b(n - 1); }
export const result = a(3);
`,
		"b": `
import { a } from "a";
export function b(n) { return n <= 0 ? 0 : a(n - 1); }
`,
	}
	_, ns, _, err := runModules(t, sources, "a")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if v := ns.Get("result"); v.ToInteger() != 0 {
		t.Fatalf("result = %v, want 0", v)
	}
}

// A mutable export read repeatedly by a third module observes live changes.
func TestModuleLiveBindingReadByThirdModule(t *testing.T) {
	sources := map[string]string{
		"counter": `
export let count = 0;
export function inc() { count = count + 1; return count; }
export const snapshot = () => count;
`,
		"reader": `
import { count, inc, snapshot } from "counter";
export function readAll() {
	return [count, inc(), snapshot(), inc(), count];
}
`,
		"main": `
import { readAll } from "reader";
export const out = readAll();
`,
	}
	_, ns, _, err := runModules(t, sources, "main")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	out := ns.Get("out")
	got := []int64{}
	for i := 0; i < 5; i++ {
		got = append(got, out.ToInteger())
		// out is an array; use indexing below via the object instead.
		break
	}
	_ = got
	arr := out.ToString().String()
	if arr != "0,1,1,2,2" {
		t.Fatalf("out = %q, want 0,1,1,2,2", arr)
	}
}

// Reading an uninitialised lexical binding during a cycle throws a
// ReferenceError (TDZ).
func TestModuleTDZInCycle(t *testing.T) {
	sources := map[string]string{
		"x": `
import { probe } from "y";
export let value = probe() + 1;
`,
		"y": `
import { value } from "x";
export function probe() { return value; }
`,
	}
	l := newTestLoader(t, sources, nil)
	id, err := l.Instantiate("x")
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	_, err = l.Evaluate(id)
	if err == nil {
		t.Fatal("expected TDZ ReferenceError, got nil")
	}
	if !strings.Contains(err.Error(), "before initialization") {
		t.Fatalf("err = %v, want TDZ message", err)
	}
}

// A module evaluates once; its failure sticks and cannot be bypassed by
// importing it through a different module/order.
func TestModuleEvaluateOnceAndStickyError(t *testing.T) {
	sources := map[string]string{
		"boom": `export let x = (function(){ throw new Error("boom"); })();`,
		"a":    `import {x} from "boom"; export const a = x;`,
		"b":    `import {x} from "boom"; export const b = x;`,
	}
	vm := New()
	l := NewModuleLoader(vm, nil, func(id string) (string, error) {
		return sources[id], nil
	})
	idA, err := l.Instantiate("a")
	if err != nil {
		t.Fatalf("instantiate a: %v", err)
	}
	if _, err := l.Evaluate(idA); err == nil {
		t.Fatal("expected error evaluating a")
	}
	idB, err := l.Instantiate("b")
	if err != nil {
		t.Fatalf("instantiate b: %v", err)
	}
	_, err = l.Evaluate(idB)
	if err == nil {
		t.Fatal("expected sticky error evaluating b through a different entry")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("sticky err = %v", err)
	}
}

// Aliases resolving to the same canonical ID share one module instance: the
// mutable counter is shared across both specifiers.
func TestModuleAliasSharesInstance(t *testing.T) {
	sources := map[string]string{
		"real": `export let n = 1; export function bump(){ n++; }`,
		"a":    `import {n,bump} from "alias1"; export const a = n; export {bump};`,
		"b":    `import {n,bump} from "alias2"; export function read(){ return n; } export {bump as bumpB};`,
		"main": `
import {a, bump} from "a";
import {read, bumpB} from "b";
bump();
bumpB();
export const va = a;
export const vr = read();
`,
	}
	vm := New()
	l := NewModuleLoader(vm, func(referrer, spec string) (string, error) {
		switch spec {
		case "alias1", "alias2":
			return "real", nil
		}
		return spec, nil
	}, func(id string) (string, error) { return sources[id], nil })
	id, err := l.Instantiate("main")
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	if _, err := l.Evaluate(id); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	ns, _ := l.ModuleNamespace(id)
	if ns.Get("vr").ToInteger() != 3 {
		t.Fatalf("vr = %v, want 3", ns.Get("vr"))
	}
}

// default export, named/default combination, and re-export work and keep
// live bindings.
func TestModuleDefaultAndReExport(t *testing.T) {
	sources := map[string]string{
		"base": `
export let v = 10;
export function setV(x){ v = x; }
export default function df(){ return "default-" + v; }
`,
		"re": `
export { v, setV, default as df } from "base";
`,
		"main": `
import d, { v, setV } from "base";
import { df } from "re";
export const before = v;
export const dcall1 = d();
setV(20);
export const after = v;
export const dcall2 = df();
`,
	}
	_, ns, _, err := runModules(t, sources, "main")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ns.Get("before").ToInteger() != 10 ||
		ns.Get("after").ToInteger() != 20 ||
		ns.Get("dcall1").ToString().String() != "default-10" ||
		ns.Get("dcall2").ToString().String() != "default-20" {
		t.Fatalf("before=%v after=%v d1=%v d2=%v",
			ns.Get("before"), ns.Get("after"), ns.Get("dcall1"), ns.Get("dcall2"))
	}
}

// Importers cannot assign to an imported binding: strict-mode TypeError.
func TestModuleImportedBindingReadOnly(t *testing.T) {
	sources := map[string]string{
		"base": `export let x = 1;`,
		"main": `
import { x } from "base";
x = 2;
`,
	}
	_, _, _, err := runModules(t, sources, "main")
	if err == nil {
		t.Fatal("expected TypeError assigning to import")
	}
}

// Rejected syntax is rejected before any execution.
func TestModuleRejectedSyntax(t *testing.T) {
	cases := map[string]string{
		"dynamic import":     `const p = import("x");`,
		"import.meta":        `import.meta.x;`,
		"namespace import":   `import * as ns from "x";`,
		"export star":        `export * from "x";`,
		"top level await":    `const v = await 1;`,
		"destructure export": `export const {a} = {a:1};`,
		"missing export":     `import {nope} from "x";`,
		"bad specifier":      `import x from 42;`,
	}
	sources := map[string]string{"x": `export let y = 1;`}
	for name, body := range cases {
		src := map[string]string{"main": body}
		for k, v := range sources {
			src[k] = v
		}
		l := newTestLoader(t, src, nil)
		id, ierr := l.Instantiate("main")
		var err error
		if ierr == nil {
			_, err = l.Evaluate(id)
		} else {
			err = ierr
		}
		if err == nil {
			t.Errorf("case %q: expected rejection, got success", name)
		}
	}
}

// Ordinary scripts on the same runtime are completely unaffected.
func TestModuleDoesNotAffectScripts(t *testing.T) {
	vm := New()
	v, err := vm.RunScript("s.js", `
var import_ = 1;
function export_(){ return import_ + 41; }
export_();
`)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	if v.ToInteger() != 42 {
		t.Fatalf("script result = %v, want 42", v)
	}
	// And a module graph works alongside.
	l := NewModuleLoader(vm, nil, func(id string) (string, error) {
		return `export const answer = 42;`, nil
	})
	id, err := l.Instantiate("m")
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	if _, err := l.Evaluate(id); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	ns, _ := l.ModuleNamespace(id)
	if ns.Get("answer").ToInteger() != 42 {
		t.Fatalf("module answer = %v", ns.Get("answer"))
	}
}

// ParseModule public entry point.
func TestModuleParseEntry(t *testing.T) {
	sources := map[string]string{
		"m":   `import {x} from "dep"; export const y = x + 1;`,
		"dep": `export const x = 1;`,
	}
	l := newTestLoader(t, sources, nil)
	id, err := l.ParseModule("m")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if id != "m" {
		t.Fatalf("id = %q", id)
	}
	// Parsed but not instantiated: evaluate lazily instantiates.
	if _, err := l.Evaluate(id); err != nil {
		t.Fatalf("lazy evaluate: %v", err)
	}
}

func TestModuleShadowing(t *testing.T) {
	sources := map[string]string{
		"main": `
import {v} from "dep";
export const outer = v;
export function f() {
  const v = 100; // shadows the imported binding
  return v;
}
export function g() {
  if (true) {
    const v = 200;
    return v;
  }
}
export function h() {
  return v; // still the import
}
`,
		"dep": `export let v = 5;`,
	}
	_, ns, _, err := runModules(t, sources, "main")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]int64{
		"f": 100, "g": 200, "h": 5,
	}
	for name, want := range checks {
		fn, ok := AssertFunction(ns.Get(name))
		if !ok {
			t.Fatalf("%s not callable", name)
		}
		got, err := fn(nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.ToInteger() != want {
			t.Fatalf("%s=%v want %d", name, got, want)
		}
	}
	if ns.Get("outer").ToInteger() != 5 {
		t.Fatalf("outer=%v want 5", ns.Get("outer"))
	}
}

func TestModuleDestructureNonExported(t *testing.T) {
	sources := map[string]string{
		"main": `
import {obj} from "dep";
const {a, b} = obj;
export const sum = a + b;
export const arr = [a, b];
`,
		"dep": `export const obj = {a: 1, b: 2};`,
	}
	_, ns, _, err := runModules(t, sources, "main")
	if err != nil {
		t.Fatal(err)
	}
	if ns.Get("sum").ToInteger() != 3 {
		t.Fatalf("sum=%v", ns.Get("sum"))
	}
	if ns.Get("arr").ToString().String() != "1,2" {
		t.Fatalf("arr=%v", ns.Get("arr"))
	}
}

// After normal instantiation/evaluation an exported class is usable through
// the importer's live binding.
func TestModuleClassExportUsable(t *testing.T) {
	sources := map[string]string{
		"x": `import {probe} from "y"; export class C { static v(){return 1;} } export const r = probe();`,
		"y": `import {C} from "x"; export function probe(){ return C.v(); }`,
	}
	_, ns, _, err := runModules(t, sources, "x")
	if err != nil {
		t.Fatal(err)
	}
	if ns.Get("r").ToInteger() != 1 {
		t.Fatalf("r=%v want 1", ns.Get("r"))
	}
}

func TestModuleClassExportAfterInit(t *testing.T) {
	sources := map[string]string{
		"x": `import {use} from "y"; export class C { static v(){return 9;} } export const r = use();`,
		"y": `import {C} from "x"; export function use(){ return () => C.v(); }`,
	}
	_, ns, _, err := runModules(t, sources, "x")
	if err != nil {
		t.Fatal(err)
	}
	fn, ok := AssertFunction(ns.Get("r"))
	if !ok {
		t.Fatal("r not callable")
	}
	v, err := fn(nil)
	if err != nil {
		t.Fatal(err)
	}
	if v.ToInteger() != 9 {
		t.Fatalf("v=%v want 9", v)
	}
}

func TestModuleArrowAndNested(t *testing.T) {
	sources := map[string]string{
		"main": `
import {base} from "dep";
const add = (x) => base + x;
export const r1 = add(2);
export const r2 = [1,2,3].map(x => x + base);
export const r3 = (function(){
  let total = 0;
  for (const item of [1,2,3]) { total += item + base; }
  return total;
})();
`,
		"dep": `export const base = 10;`,
	}
	_, ns, _, err := runModules(t, sources, "main")
	if err != nil {
		t.Fatal(err)
	}
	if ns.Get("r1").ToInteger() != 12 {
		t.Fatalf("r1=%v", ns.Get("r1"))
	}
	if ns.Get("r2").ToString().String() != "11,12,13" {
		t.Fatalf("r2=%v", ns.Get("r2"))
	}
	// each iteration adds item + base: (1+10)+(2+10)+(3+10) = 36
	if ns.Get("r3").ToInteger() != 36 {
		t.Fatalf("r3=%v want 36", ns.Get("r3"))
	}
}

// Side effects run in post-order dependency order, exactly once.
func TestModuleEvaluationOrder(t *testing.T) {
	vm := New()
	var order []string
	vm.Set("mark", func(s string) { order = append(order, s) })
	l := NewModuleLoader(vm, nil, func(id string) (string, error) {
		m := map[string]string{
			"a": `import "b"; import "c"; globalThis.mark("a"); export const a=1;`,
			"b": `import "d"; globalThis.mark("b"); export const b=2;`,
			"c": `import "d"; globalThis.mark("c"); export const c=3;`,
			"d": `globalThis.mark("d"); export const d=4;`,
		}
		return m[id], nil
	})
	id, err := l.Instantiate("a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Evaluate(id); err != nil {
		t.Fatal(err)
	}
	got := joinStrings(order)
	// d first (shared, once), then b and c in source order, finally a.
	if got != "d,b,c,a" {
		t.Fatalf("order=%v want d,b,c,a", order)
	}

	// A second entry that reuses the graph must not re-run anything.
	order = nil
	id2, err := l.Instantiate("b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Evaluate(id2); err != nil {
		t.Fatal(err)
	}
	if len(order) != 0 {
		t.Fatalf("modules re-ran on second entry: %v", order)
	}
}

func joinStrings(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ","
		}
		out += v
	}
	return out
}
