package goja

import (
	"testing"
)

type roChild struct {
	V int
}

type roParent struct {
	Name  string
	Tags  []string
	Items map[string]*roChild
	A     *roChild
	B     *roChild
}

// TestReadOnlyValue covers struct fields, map keys (including integer keys), nested slice
// push/splice and the Reflect/property descriptor surface, in both strict and sloppy mode.
func TestReadOnlyValue(t *testing.T) {
	vm := New()
	child := &roChild{V: 7}
	p := &roParent{
		Name:  "cfg",
		Tags:  []string{"a", "b"},
		Items: map[string]*roChild{"x": child},
		A:     child,
		B:     child,
	}
	vm.Set("p", vm.ToReadOnlyValue(p))

	cases := []struct{ script, want string }{
		// Reads preserve the usual Go value mapping.
		{`p.Name`, "cfg"},
		{`p.Tags[0]`, "a"},
		{`p.Items.x.V`, "7"},

		// Stable identity for the same Go object reached through multiple paths.
		{`p.A === p.B`, "true"},
		{`p.A === p.Items.x`, "true"},
		{`p.Tags === p.Tags`, "true"},

		// Assignment: throws in strict mode, silent no-op in sloppy mode, host unchanged.
		{`(function(){"use strict"; try { p.Name = 1; return "changed" } catch(e){ return e instanceof TypeError } })()`, "true"},
		{`(function(){ p.Name = 1; return p.Name })()`, "cfg"},
		{`(function(){"use strict"; try { p.newProp = 1; return "changed" } catch(e){ return e instanceof TypeError } })()`, "true"},
		{`(function(){ p.newProp = 1; return typeof p.newProp })()`, "undefined"},

		// Nested slice mutator methods throw regardless of strictness and leave the host intact.
		{`(function(){"use strict"; try { p.Tags.push("z"); return "changed" } catch(e){ return e instanceof TypeError } })()`, "true"},
		{`(function(){ try { p.Tags.push("z") } catch(e){ if(!(e instanceof TypeError)) throw e }; return p.Tags.length })()`, "2"},
		{`(function(){ try { p.Tags.splice(0,1) } catch(e){ if(!(e instanceof TypeError)) throw e }; return p.Tags.length })()`, "2"},
		{`(function(){ try { p.Tags.length = 0 } catch(e){}; return p.Tags.length })()`, "2"},
		{`(function(){"use strict"; try { p.Tags.length = 0; return "changed" } catch(e){ return e instanceof TypeError } })()`, "true"},

		// delete cannot remove struct fields, elements or map entries.
		{`(function(){"use strict"; try { delete p.Name; return "changed" } catch(e){ return e instanceof TypeError } })()`, "true"},
		{`(function(){ delete p.Name; return p.Name })()`, "cfg"},
		{`(function(){"use strict"; try { delete p.Tags[0]; return "changed" } catch(e){ return e instanceof TypeError } })()`, "true"},
		{`p.Tags[0]`, "a"},

		// Object.defineProperty cannot change anything; idempotent frozen descriptors are allowed.
		{`(function(){"use strict"; try { Object.defineProperty(p,"Name",{value:"x"}); return "changed" } catch(e){ return e instanceof TypeError } })()`, "true"},
		{`(function(){"use strict"; try { Object.defineProperty(p,"Name",{value:"x",writable:true}); return "changed" } catch(e){ return e instanceof TypeError } })()`, "true"},
		{`(function(){"use strict"; try { Object.defineProperty(p,"New",{value:1}); return "changed" } catch(e){ return e instanceof TypeError } })()`, "true"},
		{`(function(){ try { Object.defineProperty(p,"Name",{value:"cfg"}); return "ok" } catch(e){ return e instanceof TypeError ? "te" : "err" } })()`, "ok"},

		// Prototype changes and integrity levels.
		{`(function(){"use strict"; try { Object.setPrototypeOf(p, {z:1}); return "changed" } catch(e){ return e instanceof TypeError } })()`, "true"},
		{`Object.freeze(p) === p`, "true"},
		{`Object.preventExtensions(p) === p`, "true"},
		{`Object.seal(p) === p`, "true"},
		{`Object.isExtensible(p)`, "false"},
		{`Object.isFrozen(p)`, "true"},
		{`Object.isFrozen(p.Tags)`, "true"},

		// Reflection descriptors report frozen data properties.
		{`Object.getOwnPropertyDescriptor(p,"Name").writable`, "false"},
		{`Object.getOwnPropertyDescriptor(p,"Name").configurable`, "false"},
		{`Object.getOwnPropertyDescriptor(p,"Name").enumerable`, "true"},
		{`Object.getOwnPropertyDescriptor(p.Tags,"length").writable`, "false"},
		{`(function(){"use strict"; try { Object.defineProperty(p.Tags,"length",{value:1}); return "x" } catch(e){ return e instanceof TypeError ? "te":"e" } })()`, "te"},
		{`(function(){ try { Object.defineProperty(p.Tags,"length",{value:2}); return "ok" } catch(e){ return e instanceof TypeError ? "te":"e" } })()`, "ok"},
		{`(function(){"use strict"; try { Object.defineProperty(p.Tags,"length",{writable:true}); return "x" } catch(e){ return e instanceof TypeError ? "te":"e" } })()`, "te"},
		{`Reflect.has(p,"Name")`, "true"},
		{`Reflect.set(p,"Name",1)`, "false"},
		{`Reflect.deleteProperty(p,"Name")`, "false"},
		{`Reflect.defineProperty(p,"Name",{value:1})`, "false"},
		{`Reflect.isExtensible(p)`, "false"},

		// Enumeration lists fields but no methods (none whitelisted here).
		{`Object.keys(p).sort().join(",")`, "A,B,Items,Name,Tags"},

		// JSON.stringify reads through the view.
		{`JSON.stringify({n:p.Name})`, `{"n":"cfg"}`},
	}
	for _, c := range cases {
		v, err := vm.RunString(c.script)
		if err != nil {
			t.Fatalf("%s: %v", c.script, err)
		}
		if got := v.String(); got != c.want {
			t.Errorf("%s = %s, want %s", c.script, got, c.want)
		}
	}

	if p.Name != "cfg" || child.V != 7 || len(p.Tags) != 2 || p.Tags[0] != "a" {
		t.Fatalf("host value mutated: %+v / %+v", p, child)
	}

	if exp := vm.Get("p").Export(); exp != p {
		t.Fatalf("Export did not return the original Go value: %T", exp)
	}
}

// TestReadOnlyValueArraysAndMaps covers fixed-size arrays, map keys of various types, sorting
// and nested writes reached through map and array element paths.
func TestReadOnlyValueArraysAndMaps(t *testing.T) {
	vm := New()

	type item struct{ X int }
	type root struct {
		Arr    [3]int
		Sli    []*item
		M      map[int]string
		PtrMap map[string]*item
	}

	it := &item{X: 2}
	r := &root{
		Arr:    [3]int{3, 1, 2},
		Sli:    []*item{{X: 1}, it},
		M:      map[int]string{1: "a", 2: "b"},
		PtrMap: map[string]*item{"p": it},
	}
	vm.Set("r", vm.ToReadOnlyValue(r))

	cases := []struct{ script, want string }{
		{`Array.isArray(r.Arr)`, "true"},
		{`Array.isArray(r.Sli)`, "true"},
		{`r.Arr.join(",")`, "3,1,2"},
		{`r.Arr.length`, "3"},

		// sort bypasses ordinary setters but is still blocked.
		{`(function(){ try { r.Arr.sort(); return "x" } catch(e){ return e instanceof TypeError ? "te" : "err" } })()`, "te"},
		{`r.Arr.join(",")`, "3,1,2"},

		// Fixed-size array elements cannot be assigned.
		{`(function(){"use strict"; try { r.Arr[0] = 9; return "x" } catch(e){ return e instanceof TypeError ? "te" : "err" } })()`, "te"},
		{`r.Arr[0]`, "3"},

		// Integer map keys map to numeric property keys.
		{`r.M[1]`, "a"},
		{`Object.keys(r.M).sort().join(",")`, "1,2"},
		{`(function(){"use strict"; try { r.M[3] = "c"; return "x" } catch(e){ return e instanceof TypeError ? "te" : "err" } })()`, "te"},
		{`(function(){ r.M[1] = "x"; return r.M[1] })()`, "a"},
		{`(function(){"use strict"; try { delete r.M[1]; return "x" } catch(e){ return e instanceof TypeError ? "te" : "err" } })()`, "te"},
		{`r.M[1]`, "a"},

		// Nested writes via slices, pointer map values and pointer siblings.
		{`r.Sli[0] === r.Sli[0]`, "true"},
		{`r.Sli[1] === r.PtrMap.p`, "true"},
		{`(function(){"use strict"; try { r.Sli[0].X = 5; return "x" } catch(e){ return e instanceof TypeError ? "te" : "err" } })()`, "te"},
		{`r.Sli[0].X`, "1"},
		{`(function(){"use strict"; try { r.PtrMap.p.X = 5; return "x" } catch(e){ return e instanceof TypeError ? "te" : "err" } })()`, "te"},
		{`r.PtrMap.p.X`, "2"},

		// Live read: changing the Go state is reflected on the next access.
		{`r.PtrMap.p.X`, "2"},
	}
	for _, c := range cases {
		v, err := vm.RunString(c.script)
		if err != nil {
			t.Fatalf("%s: %v", c.script, err)
		}
		if got := v.String(); got != c.want {
			t.Errorf("%s = %s, want %s", c.script, got, c.want)
		}
	}

	it.X = 42
	if v, err := vm.RunString(`r.PtrMap.p.X`); err != nil || v.String() != "42" {
		t.Fatalf("live read after Go mutation = %s (%v)", v, err)
	}

	if r.Arr != [3]int{3, 1, 2} || len(r.Sli) != 2 || r.Sli[0].X != 1 || it.X != 42 {
		t.Fatalf("host value mutated unexpectedly: %+v", r)
	}
}

type roMethodHolder struct {
	N int
}

// Double mutates its receiver; it is used to prove methods are hidden unless whitelisted and
// that whitelisting makes no purity guarantee.
func (s *roMethodHolder) Double() int {
	s.N *= 2
	return s.N
}

func (s roMethodHolder) Peek() int {
	return s.N
}

// TestReadOnlyValueMethods checks the default-hidden policy and the explicit whitelist.
func TestReadOnlyValueMethods(t *testing.T) {
	vm := New()

	m := &roMethodHolder{N: 3}
	vm.Set("ro", vm.ToReadOnlyValue(m))
	vm.Set("row", vm.ToReadOnlyValue(m, "Peek"))

	cases := []struct{ script, want string }{
		// Nothing exposed by default.
		{`typeof ro.Double`, "undefined"},
		{`typeof ro.Peek`, "undefined"},
		{`Object.keys(ro).join(",")`, "N"},
		// Only the explicitly whitelisted method is exposed and callable.
		{`typeof row.Peek`, "function"},
		{`typeof row.Double`, "undefined"},
		{`row.Peek()`, "3"},
		{`Object.keys(row).sort().join(",")`, "N,Peek"},
		// Exposed methods are still non-writable data properties on the frozen view.
		{`Object.getOwnPropertyDescriptor(row,"Peek").writable`, "false"},
		{`Object.getOwnPropertyDescriptor(row,"Peek").configurable`, "false"},
		{`(function(){"use strict"; try { row.Peek = 1; return "x" } catch(e){ return e instanceof TypeError ? "te" : "err" } })()`, "te"},
	}
	for _, c := range cases {
		v, err := vm.RunString(c.script)
		if err != nil {
			t.Fatalf("%s: %v", c.script, err)
		}
		if got := v.String(); got != c.want {
			t.Errorf("%s = %s, want %s", c.script, got, c.want)
		}
	}
}

// TestReadOnlyValuePrimitives ensures non-container values use the normal conversion.
func TestReadOnlyValuePrimitives(t *testing.T) {
	vm := New()
	if v := vm.ToReadOnlyValue(nil); v != _null {
		t.Fatalf("nil: %v", v)
	}
	if v := vm.ToReadOnlyValue(42); v.ToInteger() != 42 {
		t.Fatalf("int: %v", v)
	}
	if v := vm.ToReadOnlyValue("hi"); v.String() != "hi" {
		t.Fatalf("string: %v", v)
	}
	if v := vm.ToReadOnlyValue(true); !v.ToBoolean() {
		t.Fatalf("bool: %v", v)
	}
}

// TestToValueStillWritable proves the new entry point does not affect the ordinary ToValue path.
func TestToValueStillWritable(t *testing.T) {
	vm := New()

	m := &roMethodHolder{N: 5}
	vm.Set("rw", vm.ToValue(m))
	vm.Set("s", vm.ToValue([]int{1}))

	cases := []struct{ script, want string }{
		{`rw.N = 9; rw.N`, "9"},
		{`typeof rw.Double`, "function"},
		{`typeof rw.Peek`, "function"},
		{`Object.getOwnPropertyDescriptor(rw,"N").writable`, "true"},
		{`Object.isExtensible(rw)`, "true"},
		{`s.push(2); s.length`, "2"},
	}
	for _, c := range cases {
		v, err := vm.RunString(c.script)
		if err != nil {
			t.Fatalf("%s: %v", c.script, err)
		}
		if got := v.String(); got != c.want {
			t.Errorf("%s = %s, want %s", c.script, got, c.want)
		}
	}
	if m.N != 9 {
		t.Fatalf("expected writable host, got N=%d", m.N)
	}
}

// TestReadOnlyValueDescriptors ensures the descriptors returned for nested containers are
// frozen, and that integer-keyed map entries behave the same.
func TestReadOnlyValueDescriptors(t *testing.T) {
	vm := New()
	type v struct{ N int }
	data := map[string]*v{"k": {N: 1}}
	vm.Set("d", vm.ToReadOnlyValue(data))

	cases := []struct{ script, want string }{
		{`Object.getOwnPropertyDescriptor(d,"k").writable`, "false"},
		{`Object.getOwnPropertyDescriptor(d,"k").configurable`, "false"},
		{`Object.getOwnPropertyDescriptor(d.k,"N").writable`, "false"},
		{`(function(){"use strict"; try { d.k.N = 2; return "x" } catch(e){ return e instanceof TypeError ? "te":"e" } })()`, "te"},
		{`d.k.N`, "1"},
	}
	for _, c := range cases {
		val, err := vm.RunString(c.script)
		if err != nil {
			t.Fatalf("%s: %v", c.script, err)
		}
		if got := val.String(); got != c.want {
			t.Errorf("%s = %s, want %s", c.script, got, c.want)
		}
	}
}
