package goja

import (
	"reflect"

	"github.com/dop251/goja/unistring"
)

type objectGoMapSimple struct {
	baseObject
	data     map[string]interface{}
	readOnly *readOnlyContext
}

func (o *objectGoMapSimple) init() {
	o.baseObject.init()
	o.prototype = o.val.runtime.global.ObjectPrototype
	o.class = classObject
	o.extensible = true
}

func (o *objectGoMapSimple) _getStr(name string) Value {
	v, exists := o.data[name]
	if !exists {
		return nil
	}
	if o.readOnly != nil {
		return o.readOnly.child(v, reflect.Value{})
	}
	return o.val.runtime.ToValue(v)
}

func (o *objectGoMapSimple) getStr(name unistring.String, receiver Value) Value {
	if v := o._getStr(name.String()); v != nil {
		return v
	}
	return o.baseObject.getStr(name, receiver)
}

func (o *objectGoMapSimple) getOwnPropStr(name unistring.String) Value {
	if v := o._getStr(name.String()); v != nil {
		return &valueProperty{
			value:      v,
			writable:   o.readOnly == nil,
			enumerable: true,
		}
	}
	return nil
}

func (o *objectGoMapSimple) setOwnStr(name unistring.String, val Value, throw bool) bool {
	if o.readOnly != nil {
		return o.readOnly.rejectReadOnlyWrite(throw, "assign to property "+name)
	}
	n := name.String()
	if _, exists := o.data[n]; exists {
		o.data[n] = val.Export()
		return true
	}
	if proto := o.prototype; proto != nil {
		// we know it's foreign because prototype loops are not allowed
		if res, ok := proto.self.setForeignStr(name, val, o.val, throw); ok {
			return res
		}
	}
	// new property
	if !o.extensible {
		o.val.runtime.typeErrorResult(throw, "Cannot add property %s, object is not extensible", name)
		return false
	} else {
		o.data[n] = val.Export()
	}
	return true
}

func trueValIfPresent(present bool) Value {
	if present {
		return valueTrue
	}
	return nil
}

func (o *objectGoMapSimple) setForeignStr(name unistring.String, val, receiver Value, throw bool) (bool, bool) {
	if o.readOnly != nil {
		o.readOnly.rejectReadOnlyWrite(throw, "assign to property "+name)
		return false, true
	}
	return o._setForeignStr(name, trueValIfPresent(o._hasStr(name.String())), val, receiver, throw)
}

func (o *objectGoMapSimple) _hasStr(name string) bool {
	_, exists := o.data[name]
	return exists
}

func (o *objectGoMapSimple) hasOwnPropertyStr(name unistring.String) bool {
	return o._hasStr(name.String())
}

func (o *objectGoMapSimple) defineOwnPropertyStr(name unistring.String, descr PropertyDescriptor, throw bool) bool {
	if o.readOnly != nil {
		if checkFrozenCompatibleDescriptor(o.getOwnPropStr(name), descr) {
			return true
		}
		return o.readOnly.rejectReadOnlyWrite(throw, "define property "+name)
	}
	if !o.val.runtime.checkHostObjectPropertyDescr(name, descr, throw) {
		return false
	}

	n := name.String()
	if o.extensible || o._hasStr(n) {
		o.data[n] = descr.Value.Export()
		return true
	}

	o.val.runtime.typeErrorResult(throw, "Cannot define property %s, object is not extensible", n)
	return false
}

func (o *objectGoMapSimple) deleteStr(name unistring.String, throw bool) bool {
	if o.readOnly != nil {
		return o.readOnly.rejectReadOnlyWrite(throw, "delete property "+name)
	}
	delete(o.data, name.String())
	return true
}

func (o *objectGoMapSimple) setOwnIdx(idx valueInt, val Value, throw bool) bool {
	if o.readOnly != nil {
		return o.readOnly.rejectReadOnlyWrite(throw, "assign to property "+idx.string())
	}
	return o.baseObject.setOwnIdx(idx, val, throw)
}

func (o *objectGoMapSimple) setOwnSym(s *Symbol, val Value, throw bool) bool {
	if o.readOnly != nil {
		return o.readOnly.rejectReadOnlyWrite(throw, "assign to symbol property")
	}
	return o.baseObject.setOwnSym(s, val, throw)
}

func (o *objectGoMapSimple) defineOwnPropertyIdx(idx valueInt, descr PropertyDescriptor, throw bool) bool {
	if o.readOnly != nil {
		if checkFrozenCompatibleDescriptor(o.getOwnPropIdx(idx), descr) {
			return true
		}
		return o.readOnly.rejectReadOnlyWrite(throw, "define property "+idx.string())
	}
	return o.baseObject.defineOwnPropertyIdx(idx, descr, throw)
}

func (o *objectGoMapSimple) defineOwnPropertySym(s *Symbol, descr PropertyDescriptor, throw bool) bool {
	if o.readOnly != nil {
		if checkFrozenCompatibleDescriptor(o.getOwnPropSym(s), descr) {
			return true
		}
		return o.readOnly.rejectReadOnlyWrite(throw, "define symbol property")
	}
	return o.baseObject.defineOwnPropertySym(s, descr, throw)
}

func (o *objectGoMapSimple) deleteIdx(idx valueInt, throw bool) bool {
	if o.readOnly != nil {
		return o.readOnly.rejectReadOnlyWrite(throw, "delete property "+idx.string())
	}
	return o.baseObject.deleteIdx(idx, throw)
}

func (o *objectGoMapSimple) deleteSym(s *Symbol, throw bool) bool {
	if o.readOnly != nil {
		return o.readOnly.rejectReadOnlyWrite(throw, "delete symbol property")
	}
	return o.baseObject.deleteSym(s, throw)
}

func (o *objectGoMapSimple) setProto(proto *Object, throw bool) bool {
	if o.readOnly != nil {
		return o.readOnly.rejectReadOnlyWrite(throw, "set prototype")
	}
	return o.baseObject.setProto(proto, throw)
}

func (o *objectGoMapSimple) isExtensible() bool {
	if o.readOnly != nil {
		return false
	}
	return o.baseObject.isExtensible()
}

func (o *objectGoMapSimple) preventExtensions(throw bool) bool {
	if o.readOnly != nil {
		return true
	}
	return o.baseObject.preventExtensions(throw)
}

func (o *objectGoMapSimple) enableReadOnly(ctx *readOnlyContext) {
	o.readOnly = ctx
	o.extensible = false
}

type gomapPropIter struct {
	o         *objectGoMapSimple
	propNames []string
	idx       int
}

func (i *gomapPropIter) next() (propIterItem, iterNextFunc) {
	for i.idx < len(i.propNames) {
		name := i.propNames[i.idx]
		i.idx++
		if _, exists := i.o.data[name]; exists {
			return propIterItem{name: newStringValue(name), enumerable: _ENUM_TRUE}, i.next
		}
	}

	return propIterItem{}, nil
}

func (o *objectGoMapSimple) iterateStringKeys() iterNextFunc {
	propNames := make([]string, len(o.data))
	i := 0
	for key := range o.data {
		propNames[i] = key
		i++
	}

	return (&gomapPropIter{
		o:         o,
		propNames: propNames,
	}).next
}

func (o *objectGoMapSimple) stringKeys(_ bool, accum []Value) []Value {
	// all own keys are enumerable
	for key := range o.data {
		accum = append(accum, newStringValue(key))
	}
	return accum
}

func (o *objectGoMapSimple) export(*objectExportCtx) interface{} {
	return o.data
}

func (o *objectGoMapSimple) exportType() reflect.Type {
	return reflectTypeMap
}

func (o *objectGoMapSimple) equal(other objectImpl) bool {
	if other, ok := other.(*objectGoMapSimple); ok {
		return o == other
	}
	return false
}
