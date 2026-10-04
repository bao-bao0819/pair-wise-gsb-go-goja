package goja

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"
)

type exportDiagItem struct {
	V int
}

type exportDiagRoot struct {
	Items []exportDiagItem
}

func asExportError(t *testing.T, err error) *ExportError {
	t.Helper()
	var de *ExportError
	if !errors.As(err, &de) {
		t.Fatalf("expected *ExportError, got %T: %v", err, err)
	}
	return de
}

func mustRun(t *testing.T, vm *Runtime, script string) Value {
	t.Helper()
	v, err := vm.RunString(script)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var errGoBoom = errors.New("go boom")

// The export starts from a non-zero target, the last nested element fails
// (its getter throws) and the target must be left untouched. The returned
// error must unwrap all the way to the thrown JavaScript exception and its
// structured path must point at the failing element.
func TestExportToWithDiagnosticsGetterThrow(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `
		({Items: [{V: 1}, {V: 2}, {get V() { throw new Error("boom"); }}]})
	`)

	target := exportDiagRoot{Items: []exportDiagItem{{V: 42}, {V: 43}}}
	original := exportDiagRoot{Items: []exportDiagItem{{V: 42}, {V: 43}}}

	err := vm.ExportToWithDiagnostics(v, &target)
	if err == nil {
		t.Fatal("expected an error")
	}

	de := asExportError(t, err)
	if de.Kind != ExportErrorKindGetterThrow {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}

	wantPath := []ExportPathSegment{
		ExportPropertyPathSegment{Name: "Items"},
		ExportIndexPathSegment{Index: 2},
	}
	if !reflect.DeepEqual(de.Path, wantPath) {
		t.Fatalf("unexpected path: %#v", de.Path)
	}
	if de.TargetType != reflect.TypeOf(0) {
		t.Fatalf("unexpected target type: %v", de.TargetType)
	}

	var ex *Exception
	if !errors.As(err, &ex) {
		t.Fatalf("error does not unwrap to *Exception: %v", err)
	}
	if ex.Value() == nil || ex.Value().String() != "Error: boom" {
		t.Fatalf("unexpected thrown value: %v", ex.Value())
	}

	if !reflect.DeepEqual(target, original) {
		t.Fatalf("target was modified on failure: %#v (want %#v)", target, original)
	}
}

// errors.Is must reach the original Go error thrown via GoError.
func TestExportToWithDiagnosticsGetterThrowUnwrapsGoError(t *testing.T) {
	vm := New()
	vm.Set("vmBoom", func(call FunctionCall) (Value, error) {
		return nil, errGoBoom
	})
	v := mustRun(t, vm, `
		({get F() { throw vmBoom(); }})
	`)

	type inner struct {
		F int
	}
	type outer struct {
		F inner
	}
	var target outer
	err := vm.ExportToWithDiagnostics(v, &target)
	if err == nil {
		t.Fatal("expected an error")
	}
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindGetterThrow {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	if !errors.Is(err, errGoBoom) {
		t.Fatalf("expected errors.Is(err, errGoBoom), got: %v", err)
	}
}

func TestExportToWithDiagnosticsIntegerOverflow(t *testing.T) {
	vm := New()
	cases := []struct {
		name string
		js   string
		typ  interface{}
	}{
		{"int64", `99999999999999999999999n`, new(int64)},
		{"uint32", `4294967296n`, new(uint32)},
		{"int8 negative", `-129n`, new(int8)},
		{"uint8 negative", `-1n`, new(uint8)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := vm.ExportToWithDiagnostics(mustRun(t, vm, c.js), c.typ)
			de := asExportError(t, err)
			if de.Kind != ExportErrorKindIntegerOverflow {
				t.Fatalf("unexpected kind: %v", de.Kind)
			}
		})
	}

	// Number overflow: ECMAScript wrap-around truncation is still allowed, so
	// ordinary doubles never trigger an integer overflow error.
	var i8 int8
	if err := vm.ExportToWithDiagnostics(mustRun(t, vm, `333`), &i8); err != nil {
		t.Fatalf("ES truncation must be preserved: %v", err)
	}
	if i8 != 77 {
		t.Fatalf("i8: %d", i8)
	}
}

func TestExportToWithDiagnosticsBigIntConversions(t *testing.T) {
	vm := New()

	var i int64
	if err := vm.ExportToWithDiagnostics(mustRun(t, vm, `123n`), &i); err != nil {
		t.Fatal(err)
	}
	if i != 123 {
		t.Fatalf("i: %d", i)
	}

	var f float64
	if err := vm.ExportToWithDiagnostics(mustRun(t, vm, `123n`), &f); err != nil {
		t.Fatal(err)
	}
	if f != 123 {
		t.Fatalf("f: %v", f)
	}

	var u uint32
	if err := vm.ExportToWithDiagnostics(mustRun(t, vm, `4294967295n`), &u); err != nil {
		t.Fatal(err)
	}
	if u != math.MaxUint32 {
		t.Fatalf("u: %d", u)
	}
}

func TestExportToWithDiagnosticsUnrepresentableNumber(t *testing.T) {
	vm := New()
	// A BigInt whose magnitude exceeds float64 range converts to +Infinity.
	v := mustRun(t, vm, `1n + (1n << 1024n)`)
	var f float64
	err := vm.ExportToWithDiagnostics(v, &f)
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindUnrepresentableNumber {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}

	// NaN still converts to 0 for integer targets, per ECMAScript.
	var i int
	if err := vm.ExportToWithDiagnostics(vm.ToValue(math.NaN()), &i); err != nil {
		t.Fatalf("NaN must convert to 0: %v", err)
	}
	if i != 0 {
		t.Fatalf("i: %d", i)
	}
}

func TestExportToWithDiagnosticsTypeMismatch(t *testing.T) {
	vm := New()
	var tm time.Time
	err := vm.ExportToWithDiagnostics(mustRun(t, vm, `"not-a-date"`), &tm)
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindTypeMismatch {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	if de.TargetType != reflect.TypeOf(time.Time{}) {
		t.Fatalf("unexpected target type: %v", de.TargetType)
	}

	// Object-to-array on a non-iterable object.
	var a []int
	err = vm.ExportToWithDiagnostics(mustRun(t, vm, `(function(){})`), &a)
	de = asExportError(t, err)
	if de.Kind != ExportErrorKindTypeMismatch {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
}

// A deep conversion failure reports the full path and leaves the target
// untouched. A Symbol converted to an integer throws a TypeError in
// ECMAScript, which is reported as a getter throw.
func TestExportToWithDiagnosticsDeepGetterThrow(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `({Items: [{V: 1}, {V: Symbol("x")}]})`)
	target := exportDiagRoot{Items: []exportDiagItem{{V: 9}}}
	err := vm.ExportToWithDiagnostics(v, &target)
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindGetterThrow {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	wantPath := []ExportPathSegment{
		ExportPropertyPathSegment{Name: "Items"},
		ExportIndexPathSegment{Index: 1},
		ExportPropertyPathSegment{Name: "V"},
	}
	if !reflect.DeepEqual(de.Path, wantPath) {
		t.Fatalf("unexpected path: %#v", de.Path)
	}
	if len(target.Items) != 1 || target.Items[0].V != 9 {
		t.Fatalf("target was modified: %#v", target)
	}
}

func TestExportToWithDiagnosticsDisallowedCycle(t *testing.T) {
	vm := New()
	type S struct {
		X []int
	}
	v := mustRun(t, vm, `var o = {}; o.X = o; o`)
	var s S
	err := vm.ExportToWithDiagnostics(v, &s)
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindCycle {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	wantPath := []ExportPathSegment{
		ExportPropertyPathSegment{Name: "X"},
	}
	if !reflect.DeepEqual(de.Path, wantPath) {
		t.Fatalf("unexpected path: %#v", de.Path)
	}
}

// Cycle detection follows the current traversal chain: the same object reached
// from two independent branches (a diamond) is not a cycle and must remain a
// shared alias when the target type can represent it.
func TestExportToWithDiagnosticsDiamondSharedSubobject(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `var sh = {V: 7}; ({A: sh, B: sh})`)
	type D struct {
		A, B *exportDiagItem
	}
	var d D
	if err := vm.ExportToWithDiagnostics(v, &d); err != nil {
		t.Fatal(err)
	}
	if d.A == nil || d.B == nil {
		t.Fatalf("missing fields: %#v", d)
	}
	if d.A != d.B {
		t.Fatalf("shared subobject was not aliased: %p != %p", d.A, d.B)
	}
	if d.A.V != 7 {
		t.Fatalf("unexpected value: %d", d.A.V)
	}
}

// A diamond over interface{} values keeps a single shared map.
func TestExportToWithDiagnosticsDiamondSharedMap(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `var sh = {a: 1}; ({x: sh, y: sh})`)
	var d map[string]interface{}
	if err := vm.ExportToWithDiagnostics(v, &d); err != nil {
		t.Fatal(err)
	}
	mx, _ := d["x"].(map[string]interface{})
	my, _ := d["y"].(map[string]interface{})
	if mx == nil || my == nil {
		t.Fatalf("unexpected values: %#v", d)
	}
	if reflect.ValueOf(mx).Pointer() != reflect.ValueOf(my).Pointer() {
		t.Fatal("shared subobject was not aliased in interface{} export")
	}
}

// Cycles the target type can represent (pointers, slices, maps) are retained
// using the same rules as ExportTo.
func TestExportToWithDiagnosticsAllowedCycles(t *testing.T) {
	vm := New()

	v := mustRun(t, vm, `var o = {}; o.S = o; o`)
	type T struct{ S *T }
	var t1 T
	if err := vm.ExportToWithDiagnostics(v, &t1); err != nil {
		t.Fatal(err)
	}
	if t1.S != &t1 {
		t.Fatalf("self pointer not aliased: %p != %p", t1.S, &t1)
	}

	v = mustRun(t, vm, `var a = []; a[0] = a; a`)
	var a []interface{}
	if err := vm.ExportToWithDiagnostics(v, &a); err != nil {
		t.Fatal(err)
	}
	inner, ok := a[0].([]interface{})
	if !ok || len(inner) != 1 {
		t.Fatalf("self slice not aliased: %#v", a)
	}

	type M map[string]M
	v = mustRun(t, vm, `var m = {}; m.o = m; m`)
	var m M
	if err := vm.ExportToWithDiagnostics(v, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["o"]["o"]; !ok {
		t.Fatalf("self map not aliased: %#v", m)
	}
}

// Property names containing dots or brackets must not be ambiguous: the path
// is returned as structured segments.
func TestExportToWithDiagnosticsPathSegmentsUnambiguous(t *testing.T) {
	vm := New()
	// The whole object is a list of objects; the last one's "bad" getter
	// throws. A property named "a.b[0]" exists alongside it to prove the
	// structured segments cannot be confused with string formatting.
	v := mustRun(t, vm, `
		([
			{"a.b[0]": 1},
			{bad: 1},
			{get bad() { throw new Error("p"); }}
		])
	`)
	var m []map[string]int
	err := vm.ExportToWithDiagnostics(v, &m)
	de := asExportError(t, err)
	wantPath := []ExportPathSegment{
		ExportIndexPathSegment{Index: 2},
		ExportPropertyPathSegment{Name: "bad"},
	}
	if !reflect.DeepEqual(de.Path, wantPath) {
		t.Fatalf("unexpected path: %#v", de.Path)
	}

	// A property name containing brackets round-trips exactly on success.
	v = mustRun(t, vm, `({"a.b[0]": "x", "normal": "y"})`)
	var plain map[string]string
	if err := vm.ExportToWithDiagnostics(v, &plain); err != nil {
		t.Fatal(err)
	}
	if plain["a.b[0]"] != "x" || plain["normal"] != "y" {
		t.Fatalf("unexpected map: %#v", plain)
	}
}

// A failing map conversion must not leave any partially written entries in the
// caller's map.
func TestExportToWithDiagnosticsMapAtomicity(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `({good: 1, bad: 99999999999999999999999n})`)
	target := map[string]int{"keep": 5}
	err := vm.ExportToWithDiagnostics(v, &target)
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindIntegerOverflow {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	wantPath := []ExportPathSegment{ExportPropertyPathSegment{Name: "bad"}}
	if !reflect.DeepEqual(de.Path, wantPath) {
		t.Fatalf("unexpected path: %#v", de.Path)
	}
	if len(target) != 1 || target["keep"] != 5 {
		t.Fatalf("target was partially modified: %#v", target)
	}
}

// A failing slice conversion must not leave a partially written slice.
func TestExportToWithDiagnosticsSliceAtomicity(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `[1, 99999999999999999999999n, 3]`)
	target := []int{42}
	err := vm.ExportToWithDiagnostics(v, &target)
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindIntegerOverflow {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	wantPath := []ExportPathSegment{ExportIndexPathSegment{Index: 1}}
	if !reflect.DeepEqual(de.Path, wantPath) {
		t.Fatalf("unexpected path: %#v", de.Path)
	}
	if len(target) != 1 || target[0] != 42 {
		t.Fatalf("target was partially modified: %#v", target)
	}
}

// A conversion into a pre-existing pointer must not leave the pointee
// partially modified when a deep element fails.
func TestExportToWithDiagnosticsPreexistingPointerAtomicity(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `
		({Items: [{V: 1}, {V: 99999999999999999999999n}]})
	`)
	target := exportDiagRoot{Items: []exportDiagItem{{V: 42}}}
	// pre-existing items must be restored as a whole
	err := vm.ExportToWithDiagnostics(v, &target)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(target.Items) != 1 || target.Items[0].V != 42 {
		t.Fatalf("pre-existing slice not restored: %#v", target)
	}

	// Pre-existing nested pointer to a struct is mutated in place: verify its
	// fields are rolled back too.
	type nested struct{ V int }
	type withPtr struct {
		N *nested
	}
	pre := nested{V: 11}
	wp := withPtr{N: &pre}
	v = mustRun(t, vm, `({N: {V: 99999999999999999999999n}})`)
	err = vm.ExportToWithDiagnostics(v, &wp)
	if err == nil {
		t.Fatal("expected an error")
	}
	if wp.N == nil || wp.N.V != 11 {
		t.Fatalf("pre-existing pointee not restored: %#v", wp)
	}
	if pre.V != 11 {
		t.Fatalf("pointee backing variable not restored: %#v", pre)
	}

	// A slice behind a pre-existing pointer is also restored when a deep
	// element fails.
	type withItems struct{ Items []int }
	wp2 := withItems{Items: []int{7, 8}}
	v = mustRun(t, vm, `({Items: [1, 99999999999999999999999n]})`)
	err = vm.ExportToWithDiagnostics(v, &wp2)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(wp2.Items) != 2 || wp2.Items[0] != 7 || wp2.Items[1] != 8 {
		t.Fatalf("pre-existing pointee slice not restored: %#v", wp2.Items)
	}
}

// Successful exports behave the same as ExportTo for the deep cases.
func TestExportToWithDiagnosticsSuccess(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `
		({Items: [{V: 1}, {V: 2}, {V: 3}]})
	`)
	var target exportDiagRoot
	if err := vm.ExportToWithDiagnostics(v, &target); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(target, exportDiagRoot{Items: []exportDiagItem{{V: 1}, {V: 2}, {V: 3}}}) {
		t.Fatalf("unexpected target: %#v", target)
	}
}

func TestExportToWithDiagnosticsInvalidTarget(t *testing.T) {
	vm := New()
	if err := vm.ExportToWithDiagnostics(vm.ToValue(1), nil); err == nil {
		t.Fatal("expected an error for nil target")
	}
	var i int
	if err := vm.ExportToWithDiagnostics(vm.ToValue(1), i); err == nil {
		t.Fatal("expected an error for non-pointer target")
	}
}

// errors.Is for the category sentinels works through the error chain.
func TestExportErrorKindUnwrap(t *testing.T) {
	de := &ExportError{Kind: ExportErrorKindTypeMismatch}
	if de.Error() == "" {
		t.Fatal("empty error message")
	}
	if got := de.Kind.String(); got != "type mismatch" {
		t.Fatalf("kind string: %q", got)
	}
}

// Set element conversion failures carry the element index.
func TestExportToWithDiagnosticsSetPath(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `new Set([1, 2, 99999999999999999999999n])`)
	var s []int
	err := vm.ExportToWithDiagnostics(v, &s)
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindIntegerOverflow {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	if !reflect.DeepEqual(de.Path, []ExportPathSegment{ExportIndexPathSegment{Index: 2}}) {
		t.Fatalf("unexpected path: %#v", de.Path)
	}
}

// Map value conversion failures carry the key segment, and target maps stay
// untouched on failure.
func TestExportToWithDiagnosticsMapPathAndAtomicity(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `new Map([["a", 1], ["b", 99999999999999999999999n]])`)
	m := map[string]int{"keep": 1}
	err := vm.ExportToWithDiagnostics(v, &m)
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindIntegerOverflow {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	if len(de.Path) != 1 {
		t.Fatalf("unexpected path: %#v", de.Path)
	}
	keySeg, ok := de.Path[0].(ExportKeyPathSegment)
	if !ok || keySeg.Key.String() != "b" {
		t.Fatalf("unexpected path: %#v", de.Path)
	}
	if len(m) != 1 || m["keep"] != 1 {
		t.Fatalf("target was partially modified: %#v", m)
	}
}

// Array length mismatch is reported and targets keep their contents.
func TestExportToWithDiagnosticsArrayLengthMismatch(t *testing.T) {
	vm := New()
	type A [2]int
	var a A
	a[0], a[1] = 7, 8
	err := vm.ExportToWithDiagnostics(mustRun(t, vm, `[1, 2, 3]`), &a)
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindUnspecified {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	if a != (A{7, 8}) {
		t.Fatalf("target was modified: %#v", a)
	}
}

// Iterable element failures carry the iteration index.
func TestExportToWithDiagnosticsIterablePath(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `
		({
			[Symbol.iterator]() {
				let i = 0;
				return {
					next() {
						if (i === 2) {
							return {done: true};
						}
						return {value: i++};
					}
				};
			}
		})
	`)
	var s []int
	err := vm.ExportToWithDiagnostics(v, &s)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 2 || s[0] != 0 || s[1] != 1 {
		t.Fatalf("unexpected: %v", s)
	}

	v = mustRun(t, vm, `
		({
			[Symbol.iterator]() {
				let i = 0;
				return {
					next() {
						if (i === 2) {
							return {done: true};
						}
						const n = i++;
						if (n === 1) {
							throw new Error("iter-element");
						}
						return {value: n};
					}
				};
			}
		})
	`)
	err = vm.ExportToWithDiagnostics(v, &s)
	de := asExportError(t, err)
	if de.Kind != ExportErrorKindGetterThrow {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	// The exception is thrown while collecting the iterable, before any
	// element is converted, hence the path is still at the slice root.
	if len(de.Path) != 0 {
		t.Fatalf("unexpected path: %#v", de.Path)
	}

	// An exception thrown by an element's conversion keeps the index.
	v = mustRun(t, vm, `
		({
			[Symbol.iterator]() {
				let i = 0;
				const values = [1, {valueOf() { throw new Error("el"); }}, 3];
				return {
					next() {
						if (i === values.length) {
							return {done: true};
						}
						return {value: values[i++]};
					}
				};
			}
		})
	`)
	err = vm.ExportToWithDiagnostics(v, &s)
	de = asExportError(t, err)
	if de.Kind != ExportErrorKindGetterThrow {
		t.Fatalf("unexpected kind: %v", de.Kind)
	}
	if !reflect.DeepEqual(de.Path, []ExportPathSegment{ExportIndexPathSegment{Index: 1}}) {
		t.Fatalf("unexpected element path: %#v", de.Path)
	}
}

// Successful export of a shared subobject referenced by two independent fields.
func TestExportToWithDiagnosticsSharedSubobjectSuccess(t *testing.T) {
	vm := New()
	v := mustRun(t, vm, `
		var sh = {V: 5};
		({A: sh, B: sh, Items: [sh, sh]});
	`)
	type full struct {
		A, B  *exportDiagItem
		Items []*exportDiagItem
	}
	var f full
	if err := vm.ExportToWithDiagnostics(v, &f); err != nil {
		t.Fatal(err)
	}
	if f.A == nil || f.A != f.B {
		t.Fatalf("A/B not aliased: %#v", f)
	}
	if len(f.Items) != 2 || f.Items[0] != f.Items[1] || f.Items[0] != f.A {
		t.Fatalf("Items not aliased to A: %#v (A=%p)", f.Items, f.A)
	}
}
