package goja

import (
	"reflect"
	"testing"
)

type roInner struct {
	N int
}

func (i *roInner) Inc() {
	i.N++
}

func (i roInner) Value() int {
	return i.N
}

type roConfig struct {
	Name   string
	Inner  *roInner
	Items  []*roInner
	Labels []string
	Pairs  map[string]*roInner
	Nums   map[int]string
	Fixed  [2]int
}

func (c *roConfig) Greet() string {
	return "hello " + c.Name
}

func TestReadOnlyValueStruct(t *testing.T) {
	r := New()
	inner := &roInner{N: 7}
	cfg := &roConfig{
		Name:  "cfg",
		Inner: inner,
		Items: []*roInner{inner, {N: 9}},
		Pairs: map[string]*roInner{"a": inner},
		Nums:  map[int]string{1: "one"},
		Fixed: [2]int{1, 2},
	}
	r.Set("cfg", r.ToReadOnlyValue(cfg))

	v, err := r.RunString(`
		({
			name: cfg.Name,
			inner: cfg.Inner.N,
			item: cfg.Items[1].N,
			pair: cfg.Pairs.a.N,
			num: cfg.Nums[1],
			fixed: cfg.Fixed[0] + "/" + cfg.Fixed[1],
		})
	`)
	if err != nil {
		t.Fatal(err)
	}
	exp := map[string]interface{}{
		"name": "cfg", "inner": int64(7), "item": int64(9),
		"pair": int64(7), "num": "one", "fixed": "1/2",
	}
	if !reflect.DeepEqual(v.Export(), exp) {
		t.Fatalf("unexpected read result: %#v", v.Export())
	}
}

func TestReadOnlyValueStableIdentity(t *testing.T) {
	r := New()
	inner := &roInner{N: 3}
	cfg := &roConfig{
		Inner: inner,
		Items: []*roInner{inner},
		Pairs: map[string]*roInner{"a": inner},
	}
	r.Set("cfg", r.ToReadOnlyValue(cfg))

	v, err := r.RunString(`
		cfg.Inner === cfg.Items[0] &&
		cfg.Inner === cfg.Pairs.a &&
		cfg.Inner === cfg.Inner
	`)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ToBoolean() {
		t.Fatal("nested paths to the same Go object do not share identity")
	}

	// The same Go object reached through different containers in one conversion.
	m := map[string]interface{}{"x": inner}
	r.Set("m", r.ToReadOnlyValue(m))
	v, err = r.RunString(`(function(){
		var a = m.x;
		var b = cfg.Inner;
		return a === b;
})()`)
	if err == nil {
		// separate conversions are not required to share identity
	}

	// A Go value repeated inside a single conversion shares identity, even
	// when stored under different key types and through interface{} values.
	shared := &roInner{N: 42}
	root := map[string]interface{}{
		"ptr":   shared,
		"slice": []interface{}{shared},
		"map":   map[int]*roInner{9: shared},
	}
	r.Set("root", r.ToReadOnlyValue(root))
	v, err = r.RunString(`root.ptr === root.slice[0] && root.ptr === root.map[9]`)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ToBoolean() {
		t.Fatal("shared Go value across map/slice/int-key did not keep identity")
	}
}

func TestReadOnlyValueSloppyAssignSilentlyFails(t *testing.T) {
	r := New()
	cfg := &roConfig{Name: "n", Inner: &roInner{N: 1}, Labels: []string{"a"}, Fixed: [2]int{1, 2}}
	r.Set("cfg", r.ToReadOnlyValue(cfg))

	v, err := r.RunString(`
		// Direct sloppy assignments silently fail (return the RHS, no throw).
		var r1 = (cfg.Name = "x");
		var r2 = (cfg.Inner.N = 5);
		var r3 = (cfg.Labels[0] = "b");
		var r5 = (cfg.NewField = 1);
		// Built-in mutating methods always use strict (throw=true) semantics
		// internally, even in sloppy mode, because 'length' is non-writable.
		var pushThrew = false;
		try { cfg.Labels.push("c"); } catch (e) { pushThrew = e instanceof TypeError; }
		cfg.Name + "|" + cfg.Inner.N + "|" + cfg.Labels[0] + "|" + cfg.Labels.length + "|" + cfg.NewField + "|" + pushThrew
	`)
	if err != nil {
		t.Fatalf("sloppy assignment must not throw: %v", err)
	}
	got := v.String()
	if got != "n|1|a|1|undefined|true" {
		t.Fatalf("unexpected values after sloppy writes: %q", got)
	}
	if cfg.Name != "n" || cfg.Inner.N != 1 || len(cfg.Labels) != 1 {
		t.Fatalf("Go value was mutated: %#v", cfg)
	}
}

func TestReadOnlyValueStrictAssignThrows(t *testing.T) {
	r := New()
	cfg := &roConfig{Name: "n", Inner: &roInner{N: 1}, Labels: []string{"a", "b"}, Fixed: [2]int{1, 2}}
	r.Set("cfg", r.ToReadOnlyValue(cfg))

	cases := []string{
		`cfg.Name = "x"`,
		`cfg.Inner.N = 5`,
		`cfg.Labels[0] = "b"`,
		`cfg.Labels.push("c")`,
		`cfg.Labels.length = 0`,
		`cfg.Labels.splice(0,1)`,
		`cfg.Labels.sort()`,
		`cfg.Labels.reverse()`,
		`cfg.Labels.fill("z")`,
		`cfg.Fixed[0] = 9`,
		`cfg.NewField = 1`,
		`Object.defineProperty(cfg, "Name", {value: "x"})`,
		`Object.defineProperty(cfg.Labels, "0", {value: "x"})`,
		`delete cfg.Name`,
		`delete cfg.Labels[0]`,
		`Object.setPrototypeOf(cfg, {})`,
		`cfg.__proto__ = {}`,
	}
	for _, script := range cases {
		_, err := r.RunString("'use strict';\n" + script)
		if err == nil {
			t.Fatalf("expected TypeError for %q", script)
		}
		if _, ok := err.(*Exception); !ok {
			t.Fatalf("expected JS exception for %q, got %T", script, err)
		}
	}

	if cfg.Name != "n" || cfg.Inner.N != 1 || len(cfg.Labels) != 2 || cfg.Labels[0] != "a" ||
		cfg.Labels[1] != "b" || cfg.Fixed[0] != 1 {
		t.Fatalf("Go value was mutated after strict writes: %#v", cfg)
	}
}

func TestReadOnlyValueMapWrites(t *testing.T) {
	r := New()
	data := map[string]interface{}{
		"nested": map[string]int{"v": 1},
		"list":   []interface{}{1, 2},
	}
	r.Set("d", r.ToReadOnlyValue(data))

	if _, err := r.RunString(`
		d.nested.v = 9;
		var pushThrew = false;
		try { d.list.push(3); } catch (e) { pushThrew = e instanceof TypeError; }
		if (!pushThrew) { throw new Error("push should throw"); }
		d.extra = true;
		delete d.nested;
	`); err != nil {
		t.Fatalf("sloppy writes must not throw: %v", err)
	}
	nested := data["nested"].(map[string]int)
	list := data["list"].([]interface{})
	if nested["v"] != 1 || len(list) != 2 {
		t.Fatalf("map/slice Go value mutated: %#v", data)
	}
	if _, err := r.RunString(`'use strict'; d.nested.v = 9`); err == nil {
		t.Fatal("expected TypeError writing nested map in strict mode")
	}
}

func TestReadOnlyValueDescriptors(t *testing.T) {
	r := New()
	cfg := &roConfig{Name: "n", Labels: []string{"a"}, Fixed: [2]int{1}}
	r.Set("cfg", r.ToReadOnlyValue(cfg))

	v, err := r.RunString(`
		var d = Object.getOwnPropertyDescriptor(cfg, "Name");
		var e = Object.getOwnPropertyDescriptor(cfg.Labels, "0");
		var len = Object.getOwnPropertyDescriptor(cfg.Labels, "length");
		var arr = Object.getOwnPropertyDescriptor(cfg.Fixed, "0");
		var ext = Object.isExtensible(cfg);
		var frozen = Object.isFrozen(cfg) && Object.isFrozen(cfg.Labels) && Object.isFrozen(cfg.Fixed);
		var freezeOk = (Object.freeze(cfg) === cfg);
		(!d.writable && !d.configurable && d.enumerable &&
		 !e.writable && !e.configurable &&
		 !len.writable && !len.configurable &&
		 !arr.writable && !arr.configurable &&
		 !ext && frozen && freezeOk)
	`)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ToBoolean() {
		t.Fatal("read-only descriptors/frozen state incorrect")
	}
}

func TestReadOnlyValueMethodsHiddenByDefault(t *testing.T) {
	r := New()
	cfg := &roConfig{Name: "n", Inner: &roInner{N: 1}}
	r.Set("cfg", r.ToReadOnlyValue(cfg))

	v, err := r.RunString(`
		({
			greet: typeof cfg.Greet,
			inc: typeof cfg.Inner.Inc,
			value: typeof cfg.Inner.Value,
			hasGreet: cfg.hasOwnProperty("Greet"),
			keys: Object.keys(cfg).indexOf("Greet") >= 0,
		})
	`)
	if err != nil {
		t.Fatal(err)
	}
	m := v.Export().(map[string]interface{})
	for name, got := range m {
		if name == "hasGreet" || name == "keys" {
			if got != false {
				t.Fatalf("%s should be false, got %v", name, got)
			}
		} else if got != "undefined" {
			t.Fatalf("%s should be hidden, got %v", name, got)
		}
	}
}

func TestReadOnlyValueMethodWhitelist(t *testing.T) {
	r := New()
	cfg := &roConfig{Name: "n", Inner: &roInner{N: 1}}

	// Global whitelist: Greet and Inner.Value exposed, mutating Inc still hidden.
	r.Set("cfg", r.ToReadOnlyValue(cfg, ReadOnlyMethods("Greet", "Value")))
	v, err := r.RunString(`
		cfg.Greet() === "hello n" &&
		cfg.Inner.Value() === 1 &&
		typeof cfg.Inner.Inc === "undefined"
	`)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ToBoolean() {
		t.Fatal("global method whitelist did not apply as expected")
	}

	// Per-type whitelist overrides the global one: a type with its own entry
	// ignores the global list, types without an entry fall back to it.
	r2 := New()
	r2.Set("cfg", r2.ToReadOnlyValue(cfg,
		ReadOnlyMethods("Greet"),
		ReadOnlyMethodsByType(map[reflect.Type][]string{
			reflect.TypeOf(&roConfig{}): {"Greet"},
			reflect.TypeOf(&roInner{}):  {"Value"},
		}),
	))
	v, err = r2.RunString(`
		cfg.Greet() === "hello n" &&
		cfg.Inner.Value() === 1
	`)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ToBoolean() {
		t.Fatal("per-type method whitelist did not override global list")
	}

	// A type with an entry does not inherit the global list.
	r3 := New()
	r3.Set("cfg", r3.ToReadOnlyValue(cfg,
		ReadOnlyMethods("Greet"),
		ReadOnlyMethodsByType(map[reflect.Type][]string{
			reflect.TypeOf(&roInner{}): {"Value"},
		}),
	))
	v, err = r3.RunString(`cfg.Inner.Value() === 1 && typeof cfg.Inner.Inc === "undefined"`)
	if err != nil {
		t.Fatal(err)
	}
	if !v.ToBoolean() {
		t.Fatal("per-type entry should expose Value but hide Inc")
	}
}

func TestReadOnlyValueNestedWritesStayReadOnly(t *testing.T) {
	r := New()
	cfg := &roConfig{
		Inner:  &roInner{N: 1},
		Items:  []*roInner{{N: 2}},
		Pairs:  map[string]*roInner{"k": {N: 3}},
		Labels: []string{"a"},
		Fixed:  [2]int{4, 5},
	}
	r.Set("cfg", r.ToReadOnlyValue(cfg))

	v, err := r.RunString(`
		var strict = false;
		function tryStrict(code){
			try { (0,eval)("'use strict';" + code); return false; }
			catch(e){ return e instanceof TypeError; }
		}
		({
			nestedStruct: tryStrict("cfg.Inner.N = 9"),
			sliceElem:   tryStrict("cfg.Items[0].N = 9"),
			mapValue:    tryStrict("cfg.Pairs.k.N = 9"),
			strSlice:    tryStrict("cfg.Labels[0] = 'z'"),
			arrayElem:   tryStrict("cfg.Fixed[0] = 9"),
		})
	`)
	if err != nil {
		t.Fatal(err)
	}
	m := v.Export().(map[string]interface{})
	for name, ok := range m {
		if ok != true {
			t.Fatalf("expected TypeError for %s", name)
		}
	}
}

func TestReadOnlyValueArrayLengthAndSorted(t *testing.T) {
	r := New()
	cfg := &roConfig{Labels: []string{"a", "b"}, Fixed: [2]int{1, 2}}
	r.Set("cfg", r.ToReadOnlyValue(cfg))

	// Idempotent frozen-compatible length definitions succeed; resizing fails.
	if _, err := r.RunString(`'use strict';
		Object.defineProperty(cfg.Labels, "length",
			{value: 2, writable: false, configurable: false});
		Object.defineProperty(cfg.Fixed, "length",
			{value: 2, writable: false, configurable: false});
	`); err != nil {
		t.Fatalf("idempotent length definition should succeed: %v", err)
	}

	for _, script := range []string{
		`Object.defineProperty(cfg.Labels, "length", {value: 1})`,
		`Object.defineProperty(cfg.Fixed, "length", {value: 1})`,
	} {
		if _, err := r.RunString("'use strict';\n" + script); err == nil {
			t.Fatalf("expected TypeError resizing length: %s", script)
		}
	}

	// Sorting an already-ordered frozen array must still fail (it would be a
	// no-op by swapping, but the spec requires writing the sorted list back).
	if _, err := r.RunString(`'use strict'; cfg.Labels.sort()`); err == nil {
		t.Fatal("sort of an ordered frozen array must throw")
	}
	if cfg.Labels[0] != "a" || cfg.Labels[1] != "b" {
		t.Fatalf("Go slice mutated after failed sort: %v", cfg.Labels)
	}

	// A zero/one-element frozen array sort succeeds (there is nothing to write).
	one := &roConfig{Labels: []string{"only"}}
	r.Set("one", r.ToReadOnlyValue(one))
	if _, err := r.RunString(`one.Labels.sort()`); err != nil {
		t.Fatalf("sort of a one-element read-only array should succeed: %v", err)
	}
}

func TestToValueStillWritable(t *testing.T) {
	r := New()
	cfg := &roConfig{Name: "n", Inner: &roInner{N: 1}, Labels: []string{"a"}}
	r.Set("cfg", r.ToValue(cfg))

	if _, err := r.RunString(`
		'use strict';
		cfg.Name = "x";
		cfg.Inner.N = 5;
		cfg.Labels[0] = "b";
		cfg.Labels.push("c");
	`); err != nil {
		t.Fatalf("ordinary ToValue must stay writable: %v", err)
	}
	if cfg.Name != "x" || cfg.Inner.N != 5 || len(cfg.Labels) != 2 || cfg.Labels[0] != "b" {
		t.Fatalf("ordinary ToValue writes did not propagate: %#v", cfg)
	}
}
