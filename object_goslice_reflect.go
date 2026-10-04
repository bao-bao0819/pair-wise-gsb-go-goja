package goja

import (
	"math"
	"math/bits"
	"reflect"
	"strconv"

	"github.com/dop251/goja/unistring"
)

type objectGoSliceReflect struct {
	objectGoArrayReflect
}

func (o *objectGoSliceReflect) init() {
	o.objectGoArrayReflect._init()
	o.lengthProp.writable = true
	o.putIdx = o._putIdx
}

func (o *objectGoSliceReflect) enableReadOnly(ctx *readOnlyContext) {
	o.objectGoReflect.enableReadOnly(ctx)
	o.updateLen()
	o.lengthProp.writable = false
	o.lengthProp.configurable = false
	o.putIdx = func(idx int, v Value, throw bool) bool {
		return ctx.rejectReadOnlyWrite(throw, unistring.String("assign to property "+strconv.Itoa(idx)))
	}
}

func (o *objectGoSliceReflect) _putIdx(idx int, v Value, throw bool) bool {
	if idx >= o.fieldsValue.Len() {
		o.grow(idx + 1)
	}
	return o.objectGoArrayReflect._putIdx(idx, v, throw)
}

func (o *objectGoSliceReflect) grow(size int) {
	oldcap := o.fieldsValue.Cap()
	if oldcap < size {
		n := reflect.MakeSlice(o.fieldsValue.Type(), size, growCap(size, o.fieldsValue.Len(), oldcap))
		reflect.Copy(n, o.fieldsValue)
		o.fieldsValue.Set(n)
		l := len(o.valueCache)
		if l > size {
			l = size
		}
		for i, w := range o.valueCache[:l] {
			if w != nil {
				w.setReflectValue(o.fieldsValue.Index(i))
			}
		}
	} else {
		tail := o.fieldsValue.Slice(o.fieldsValue.Len(), size)
		zero := reflect.Zero(o.fieldsValue.Type().Elem())
		for i := 0; i < tail.Len(); i++ {
			tail.Index(i).Set(zero)
		}
		o.fieldsValue.SetLen(size)
	}
}

func (o *objectGoSliceReflect) shrink(size int) {
	o.valueCache.shrink(size)
	tail := o.fieldsValue.Slice(size, o.fieldsValue.Len())
	zero := reflect.Zero(o.fieldsValue.Type().Elem())
	for i := 0; i < tail.Len(); i++ {
		tail.Index(i).Set(zero)
	}
	o.fieldsValue.SetLen(size)
}

func (o *objectGoSliceReflect) putLength(v uint32, throw bool) bool {
	if o.readOnly != nil {
		return o.readOnly.rejectReadOnlyWrite(throw, "set length")
	}
	if bits.UintSize == 32 && v > math.MaxInt32 {
		panic(rangeError("Integer value overflows 32-bit int"))
	}
	newLen := int(v)
	curLen := o.fieldsValue.Len()
	if newLen > curLen {
		o.grow(newLen)
	} else if newLen < curLen {
		o.shrink(newLen)
	}
	return true
}

func (o *objectGoSliceReflect) setOwnStr(name unistring.String, val Value, throw bool) bool {
	if name == "length" {
		if o.readOnly != nil {
			return o.readOnly.rejectReadOnlyWrite(throw, "set length")
		}
		return o.putLength(o.val.runtime.toLengthUint32(val), throw)
	}
	return o.objectGoArrayReflect.setOwnStr(name, val, throw)
}

func (o *objectGoSliceReflect) defineOwnPropertyStr(name unistring.String, descr PropertyDescriptor, throw bool) bool {
	if name == "length" {
		return o.val.runtime.defineArrayLength(&o.lengthProp, descr, o.putLength, throw)
	}
	return o.objectGoArrayReflect.defineOwnPropertyStr(name, descr, throw)
}
