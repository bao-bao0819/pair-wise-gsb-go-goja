package goja

import (
	"reflect"
)

// readOnlyView marks a Go reflect-backed host object (struct, map, slice or array and their
// nested combinations) as a read-only view. It shares the roContext with all nested objects
// so that the same Go value reached through multiple paths maps to a single stable JavaScript
// object.
type readOnlyView struct {
	ctx *roContext
}

type roIdentity struct {
	kind reflect.Kind
	typ  reflect.Type
	ptr  uintptr
}

// roContext is shared by all objects produced by a single ToReadOnlyValue call (including nested
// ones). It holds the identity cache and the explicit method whitelist.
type roContext struct {
	// cache maps a Go container identity to the read-only object that represents it. It only
	// contains values whose identity is stable (maps, slices, addressable arrays and addressable
	// structs).
	cache map[roIdentity]*Object

	// methods is the set of Go method names explicitly exposed by the caller. A nil value means
	// no methods are exposed.
	methods map[string]struct{}
}

func (c *roContext) methodAllowed(name string) bool {
	if c.methods == nil {
		return false
	}
	_, ok := c.methods[name]
	return ok
}

/*
ToReadOnlyValue converts i into a read-only Value.

It supports Go structs, maps, slices and arrays and any nested combinations of these types. The
conversion preserves the usual Go-to-JavaScript value mapping performed by ToValue (field names,
map keys, values and Export()), but the resulting object and every nested container object it
reaches are read-only:

- property and element assignments fail,
- the 'delete' operator cannot remove anything,
- Object.defineProperty cannot add or reconfigure properties,
- Object.setPrototypeOf and __proto__ assignment fail,
- Object.preventExtensions, Object.seal and Object.freeze are idempotent no-ops,
- array mutator methods such as push, pop, splice, fill, sort and assigning 'length' fail.

Failures follow the usual ECMAScript strict/non-strict rules: they throw a TypeError in strict
mode code and are silently ignored (but without modifying anything) in sloppy mode. There is no
case where an operation appears to succeed while the underlying Go value has changed.

Reads remain live: properties and elements reflect the current state of the underlying Go value,
and the same Go object reached through different fields, keys or indexes resolves to the very
same JavaScript object.

Go methods are not exposed on read-only values by default. A Go method is free to mutate its
receiver (or other package-level state), so exposing it through a "read-only" view would be
misleading. Use the methods argument to explicitly whitelist method names that are safe to expose.
Being whitelisted does not make any purity guarantee: a whitelisted method is called on the
underlying Go value and may still mutate it; only expose methods you trust.

Values that are not containers (numbers, strings, booleans, nil and so on) are converted the same
way ToValue converts them. Plain ToValue is not affected by this method and values it produces
remain fully writable.
*/
func (r *Runtime) ToReadOnlyValue(i interface{}, methods ...string) Value {
	if i == nil {
		return _null
	}
	ctx := &roContext{}
	if len(methods) > 0 {
		ctx.methods = make(map[string]struct{}, len(methods))
		for _, name := range methods {
			ctx.methods[name] = struct{}{}
		}
	}
	return r.roValue(reflect.ValueOf(i), ctx)
}

// roValue converts a reflect.Value into a read-only Value. Non-container values are converted
// normally using toValue, containers are routed through roWrap.
func (r *Runtime) roValue(v reflect.Value, ctx *roContext) Value {
	if !v.IsValid() {
		return _null
	}

	value := v
	for value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	if !value.IsValid() {
		return _null
	}

	switch value.Kind() {
	case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
		return r.roWrap(v, ctx)
	default:
		if v.Kind() == reflect.Interface {
			return r.roValue(v.Elem(), ctx)
		}
		return r.toValue(v.Interface(), v)
	}
}

// roIdentityFor returns a stable identity for a container value. The boolean is false for values
// that do not have a stable identity (unaddressable struct and array values): every access to
// such a value goes through an independent copy, so it must not be deduplicated.
func roIdentityFor(v reflect.Value) (roIdentity, bool) {
	switch v.Kind() {
	case reflect.Map, reflect.Ptr:
		return roIdentity{kind: v.Kind(), typ: v.Type(), ptr: v.Pointer()}, v.Pointer() != 0
	case reflect.Slice:
		if v.IsNil() {
			return roIdentity{}, false
		}
		return roIdentity{kind: reflect.Slice, typ: v.Type(), ptr: v.Pointer()}, true
	case reflect.Array, reflect.Struct:
		if !v.CanAddr() {
			return roIdentity{}, false
		}
		return roIdentity{kind: v.Kind(), typ: v.Type(), ptr: v.Addr().Pointer()}, true
	}
	return roIdentity{}, false
}

// roWrap creates (or reuses from the context cache) the read-only object for a Go container.
func (r *Runtime) roWrap(origValue reflect.Value, ctx *roContext) Value {
	if !origValue.IsValid() {
		return _null
	}

	value := origValue
	for value.Kind() == reflect.Ptr {
		value = value.Elem()
	}
	if !value.IsValid() {
		return _null
	}

	if id, ok := roIdentityFor(value); ok {
		if obj, exists := ctx.cache[id]; exists {
			return obj
		}
	}

	var obj *Object
	switch value.Kind() {
	case reflect.Map:
		if reflectMapSupported(value.Type()) {
			obj = &Object{runtime: r}
			m := &objectGoMapReflect{
				objectGoReflect: objectGoReflect{
					baseObject: baseObject{
						val:        obj,
						extensible: false,
					},
					origValue:   origValue,
					fieldsValue: value,
					readOnlyView: readOnlyView{
						ctx: ctx,
					},
				},
			}
			m.init()
			obj.self = m
			break
		}
		obj = r.roGeneric(origValue, value, ctx)
	case reflect.Array:
		obj = &Object{runtime: r}
		a := &objectGoArrayReflect{
			objectGoReflect: objectGoReflect{
				baseObject: baseObject{
					val:        obj,
					extensible: false,
				},
				origValue:   origValue,
				fieldsValue: value,
				readOnlyView: readOnlyView{
					ctx: ctx,
				},
			},
		}
		a.init()
		obj.self = a
	case reflect.Slice:
		obj = &Object{runtime: r}
		s := &objectGoSliceReflect{
			objectGoArrayReflect: objectGoArrayReflect{
				objectGoReflect: objectGoReflect{
					baseObject: baseObject{
						val:        obj,
						extensible: false,
					},
					origValue:   origValue,
					fieldsValue: value,
					readOnlyView: readOnlyView{
						ctx: ctx,
					},
				},
			},
		}
		s.init()
		obj.self = s
	default:
		obj = r.roGeneric(origValue, value, ctx)
	}

	if id, ok := roIdentityFor(value); ok {
		if ctx.cache == nil {
			ctx.cache = make(map[roIdentity]*Object)
		}
		ctx.cache[id] = obj
	}

	return obj
}

// roGeneric builds a read-only generic reflect host object (used for structs and for maps that
// toValue does not give the special map treatment).
func (r *Runtime) roGeneric(origValue, value reflect.Value, ctx *roContext) *Object {
	obj := &Object{runtime: r}
	o := &objectGoReflect{
		baseObject: baseObject{
			val:        obj,
			extensible: false,
		},
		origValue:   origValue,
		fieldsValue: value,
		readOnlyView: readOnlyView{
			ctx: ctx,
		},
	}
	obj.self = o
	o.init()
	return obj
}

// reflectMapSupported reports whether toValue would convert a map type into the special
// objectGoMapReflect (supported key kinds and no methods).
func reflectMapSupported(t reflect.Type) bool {
	if t.NumMethod() != 0 {
		return false
	}
	switch t.Key().Kind() {
	case reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float64, reflect.Float32:
		return true
	}
	return false
}

// roReject reports a read-only violation. In throw (strict) mode it panics with a TypeError;
// in sloppy mode it is a silent no-op.
func (o *objectGoReflect) roReject(throw bool, name string) bool {
	o.val.runtime.typeErrorResult(throw, "Cannot %s on a read-only host object", name)
	return false
}

// roGetMethod returns the method value only when it is exposed by the whitelist.
func (o *objectGoReflect) roGetMethod(jsName string) reflect.Value {
	if o.readOnlyView.ctx.methodAllowed(jsName) {
		if v := o._getMethod(jsName); v.IsValid() {
			return v
		}
	}
	return reflect.Value{}
}

// roValueProperty returns a frozen (non-writable, non-configurable, enumerable) data property.
func (o *objectGoReflect) roValueProperty(v Value) *valueProperty {
	return &valueProperty{
		value:        v,
		writable:     false,
		enumerable:   true,
		configurable: false,
	}
}

// roSameDataDescriptor reports whether descr is compatible with an existing frozen enumerable
// data property of the same value (an idempotent redefinition used by Object.freeze and friends).
func (o *objectGoReflect) roSameDataDescriptor(descr PropertyDescriptor, current Value) bool {
	if descr.Getter != nil || descr.Setter != nil {
		return false
	}
	if descr.Writable == FLAG_TRUE {
		return false
	}
	if descr.Configurable == FLAG_TRUE {
		return false
	}
	if descr.Enumerable == FLAG_TRUE {
		return false
	}
	if descr.Value != nil && !descr.Value.SameAs(current) {
		return false
	}
	return true
}
