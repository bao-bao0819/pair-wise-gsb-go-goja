package goja

import (
	"reflect"
	"unsafe"

	"github.com/dop251/goja/unistring"
)

// ReadOnlyMethodSet restricts which Go methods are exposed on read-only host values
// created by Runtime.ToReadOnlyValue.
//
// A nil ReadOnlyMethodSet (or an Option that leaves it unset) hides every method.
// This is the default because a Go method is not necessarily side-effect free:
// it is invoked with the real receiver and may modify it or other shared state,
// even though the value is otherwise presented as read-only. Whitelisting a
// method therefore never establishes a purity guarantee, it only acknowledges
// that the caller accepts whatever the method does.
//
// Use ReadOnlyMethods to whitelist the same methods for every type, or
// ReadOnlyMethodsByType to whitelist methods only on specific types. A type entry
// overrides (rather than extends) the global list for that type.
type ReadOnlyMethodSet struct {
	// global lists method names exposed on every type.
	global map[string]struct{}

	// byType lists method names exposed on a concrete Go type.
	byType map[reflect.Type]map[string]struct{}
}

// ReadOnlyOption configures the behaviour of Runtime.ToReadOnlyValue.
type ReadOnlyOption func(*readOnlyOptions)

type readOnlyOptions struct {
	methods *ReadOnlyMethodSet
}

// ReadOnlyMethods returns an Option that exposes the named Go methods on every
// read-only host value. The names are the Go method names after applying the
// Runtime's FieldNameMapper (i.e. the same names used by ToValue).
//
// Methods are hidden by default. Exposing a method does not make it pure: the
// method runs against the real Go receiver and may mutate it or other state.
func ReadOnlyMethods(names ...string) ReadOnlyOption {
	return func(o *readOnlyOptions) {
		if o.methods == nil {
			o.methods = &ReadOnlyMethodSet{}
		}
		o.methods.global = stringSet(names)
	}
}

// ReadOnlyMethodsByType returns an Option that exposes the named Go methods
// only when the read-only value is of the given type, or contains a value of
// the given type. An entry for a type overrides the global ReadOnlyMethods
// list for that type.
//
// Methods are hidden by default. Exposing a method does not make it pure: the
// method runs against the real Go receiver and may mutate it or other state.
func ReadOnlyMethodsByType(methods map[reflect.Type][]string) ReadOnlyOption {
	return func(o *readOnlyOptions) {
		if o.methods == nil {
			o.methods = &ReadOnlyMethodSet{}
		}
		if o.methods.byType == nil {
			o.methods.byType = make(map[reflect.Type]map[string]struct{}, len(methods))
		}
		for typ, names := range methods {
			o.methods.byType[typ] = stringSet(names)
		}
	}
}

func stringSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	return set
}

// readOnlyContext carries the per-conversion state of a ToReadOnlyValue call.
// It is shared by every wrapper reachable from the root value so that:
//
//   - nested values are converted recursively as read-only;
//   - distinct access paths to the same Go object resolve to a stable wrapper;
//   - the method whitelist is applied consistently.
//
// The identity registry is deliberately per conversion (not per Runtime):
// it keeps existing ToValue semantics untouched and lets unreferenced Go
// values be garbage collected once the returned wrappers are gone.
type readOnlyContext struct {
	runtime *Runtime
	methods *ReadOnlyMethodSet

	wrappers map[readOnlyObjectKey]*Object
}

type readOnlyObjectKey struct {
	ptr unsafe.Pointer
	typ reflect.Type
}

// readOnlySortable is implemented by read-only host arrays. Sorting a frozen
// non-empty array must fail even when no swap is required (e.g. an already
// ordered array), which a swap hook alone cannot detect.
type readOnlySortable interface {
	readOnlyLength() int
}

func (o *objectGoArrayReflect) readOnlyLength() int {
	if o.readOnly == nil {
		return -1
	}
	return o.fieldsValue.Len()
}

func (o *objectGoSlice) readOnlyLength() int {
	if o.readOnly == nil {
		return -1
	}
	return len(*o.data)
}

func (c *readOnlyContext) child(i interface{}, origValue reflect.Value) Value {
	return c.runtime.toReadOnlyValue(i, origValue, c)
}

// methodAllowed reports whether the method with the JS-exposed name is
// whitelisted for values of methodsType.
func (c *readOnlyContext) methodAllowed(methodsType reflect.Type, jsName string) bool {
	if c == nil || c.methods == nil {
		return false
	}
	if set, ok := c.methods.byType[methodsType]; ok {
		_, allowed := set[jsName]
		return allowed
	}
	if c.methods.global != nil {
		_, allowed := c.methods.global[jsName]
		return allowed
	}
	return false
}

// markReadOnly marks an existing host object wrapper produced by ToValue as
// read-only and registers it for stable nested identity.
//
// Primitives, functions and ordinary (non Go-container) values are returned
// unchanged: read-only enforcement only applies to host data objects.
func (r *Runtime) markReadOnly(v Value, ctx *readOnlyContext) Value {
	obj, ok := v.(*Object)
	if !ok {
		return v
	}
	w, ok := obj.self.(readOnlyWrapper)
	if !ok {
		return v
	}

	key, hasKey := r.readOnlyObjectKey(w)
	if hasKey {
		if existing := ctx.wrappers[key]; existing != nil {
			return existing
		}
	}

	w.enableReadOnly(ctx)
	if hasKey {
		ctx.wrappers[key] = obj
	}
	return obj
}

// readOnlyWrapper is implemented by every host object wrapper that can be
// presented as a read-only value.
type readOnlyWrapper interface {
	enableReadOnly(ctx *readOnlyContext)
}

// readOnlyObjectKey computes an identity key for a Go container (struct, map,
// slice or array). The key is only valid for the lifetime of the corresponding
// wrapper, which keeps the underlying Go value alive.
func (r *Runtime) readOnlyObjectKey(w readOnlyWrapper) (readOnlyObjectKey, bool) {
	switch o := w.(type) {
	case *objectGoMapReflect:
		rv := o.fieldsValue
		return readOnlyObjectKey{
			ptr: rv.UnsafePointer(),
			typ: rv.Type(),
		}, true
	case *objectGoArrayReflect:
		rv := o.fieldsValue
		if rv.Kind() == reflect.Array {
			if !rv.CanAddr() {
				return readOnlyObjectKey{}, false
			}
			rv = rv.Addr()
		}
		return readOnlyObjectKey{
			ptr: rv.UnsafePointer(),
			typ: rv.Type(),
		}, true
	case *objectGoReflect:
		if o.fieldsValue.Kind() != reflect.Struct {
			return readOnlyObjectKey{}, false
		}
		rv := o.origValue
		for rv.Kind() == reflect.Ptr {
			rv = rv.Elem()
		}
		if !rv.CanAddr() {
			return readOnlyObjectKey{}, false
		}
		return readOnlyObjectKey{
			ptr: rv.Addr().UnsafePointer(),
			typ: rv.Type(),
		}, true
	case *objectGoMapSimple:
		return readOnlyObjectKey{
			ptr: reflect.ValueOf(o.data).UnsafePointer(),
			typ: reflect.TypeOf(o.data),
		}, true
	case *objectGoSlice:
		return readOnlyObjectKey{
			ptr: reflect.ValueOf(o.data).Elem().UnsafePointer(),
			typ: reflect.TypeOf(*o.data),
		}, true
	}
	return readOnlyObjectKey{}, false
}

// rejectReadOnlyWrite reports an attempt to mutate a read-only host value,
// following the ECMAScript convention: strict operations throw a TypeError,
// sloppy operations silently fail. It returns false so callers can use it as
// their own return value.
func (c *readOnlyContext) rejectReadOnlyWrite(throw bool, what unistring.String) bool {
	c.runtime.typeErrorResult(throw, "Cannot %s on a read-only host value", what)
	return false
}

// checkFrozenCompatibleDescriptor reports whether descr merely describes the
// state the property already has on a read-only object (non-writable,
// non-configurable, no accessors, no value change). Such a definition is a
// no-op and lets Object.freeze()/Object.seal() applied to an already read-only
// host value succeed idempotently. Anything else is a mutation attempt.
//
// current is the property's current Value, or nil when the property does not
// exist.
func checkFrozenCompatibleDescriptor(current Value, descr PropertyDescriptor) bool {
	if descr.Getter != nil || descr.Setter != nil {
		return false
	}
	if descr.Configurable == FLAG_TRUE || descr.Writable == FLAG_TRUE {
		return false
	}
	if descr.Enumerable == FLAG_TRUE {
		return false
	}
	if descr.Value != nil {
		if prop, ok := current.(*valueProperty); ok {
			current = prop.value
		}
		if current == nil {
			return false
		}
		return current.StrictEquals(descr.Value)
	}
	return true
}
