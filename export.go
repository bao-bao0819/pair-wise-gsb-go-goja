package goja

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// ExportErrorKind describes the category of a failure reported by Runtime.ExportToWithDiagnostics.
type ExportErrorKind int

const (
	// ExportErrorTypeMismatch indicates that a JavaScript value cannot be converted to the target Go type.
	ExportErrorTypeMismatch ExportErrorKind = iota
	// ExportErrorIntOverflow indicates that a JavaScript number is an integer which does not fit into
	// the target Go integer type.
	ExportErrorIntOverflow
	// ExportErrorUnrepresentableNumber indicates that a JavaScript number cannot be represented in the
	// target Go type: NaN, +/-Inf, a fractional value for an integer type, or a finite value that
	// overflows float32.
	ExportErrorUnrepresentableNumber
	// ExportErrorGetterException indicates that a JavaScript exception was thrown while reading a property
	// or coercing a value (e.g. by a getter, valueOf or an iterator). The original *Exception is
	// available via Unwrap.
	ExportErrorGetterException
	// ExportErrorCycle indicates a circular reference that cannot be represented in the target Go type.
	ExportErrorCycle
)

func (k ExportErrorKind) String() string {
	switch k {
	case ExportErrorTypeMismatch:
		return "type mismatch"
	case ExportErrorIntOverflow:
		return "integer overflow"
	case ExportErrorUnrepresentableNumber:
		return "unrepresentable number"
	case ExportErrorGetterException:
		return "exception thrown by getter"
	case ExportErrorCycle:
		return "circular reference"
	}
	return "unknown"
}

// ExportPathSegment identifies a single step in the path from the root of the exported value to the
// value that caused the failure. Because segments are structured data, property names containing
// dots, brackets or any other special characters are represented unambiguously.
type ExportPathSegment struct {
	// Name is the object property name. It is only valid when IsIndex is false.
	Name string
	// Index is the array or slice element index. It is only valid when IsIndex is true.
	Index int
	// IsIndex reports whether this segment is an element index (true) or a property name (false).
	IsIndex bool
}

// ExportError is the error type returned by Runtime.ExportToWithDiagnostics. It describes where
// the conversion failed (Path), what the target Go type at that position was (Type), why it
// failed (Kind and Err).
type ExportError struct {
	Kind ExportErrorKind
	Path []ExportPathSegment
	Type reflect.Type
	Err  error
}

func (e *ExportError) Error() string {
	return fmt.Sprintf("cannot export value at %s to %v: %v", FormatExportPath(e.Path), e.Type, e.Err)
}

func (e *ExportError) Unwrap() error {
	return e.Err
}

// FormatExportPath renders a structured export path as a JavaScript-like accessor string
// (e.g. `$.server.ports[2]`). Property names that are not valid identifiers are quoted.
func FormatExportPath(path []ExportPathSegment) string {
	var sb strings.Builder
	sb.WriteByte('$')
	for _, seg := range path {
		if seg.IsIndex {
			fmt.Fprintf(&sb, "[%d]", seg.Index)
		} else if isSimplePropertyName(seg.Name) {
			sb.WriteByte('.')
			sb.WriteString(seg.Name)
		} else {
			fmt.Fprintf(&sb, "[%q]", seg.Name)
		}
	}
	return sb.String()
}

func isSimplePropertyName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

type exportChainKey struct {
	obj *Object
	typ reflect.Type
}

// exportDiag holds the diagnostics state of a single ExportToWithDiagnostics call: the path from
// the root to the value currently being converted and the chain of (object, type) pairs along the
// current traversal, used to detect circular references that cannot be satisfied by aliasing.
type exportDiag struct {
	path  []ExportPathSegment
	chain map[exportChainKey]struct{}
}

func (d *exportDiag) pathSnapshot() []ExportPathSegment {
	if len(d.path) == 0 {
		return nil
	}
	p := make([]ExportPathSegment, len(d.path))
	copy(p, d.path)
	return p
}

func (ctx *objectExportCtx) diagPush(seg ExportPathSegment) {
	if ctx.diag != nil {
		ctx.diag.path = append(ctx.diag.path, seg)
	}
}

func (ctx *objectExportCtx) diagPop() {
	if ctx.diag != nil {
		ctx.diag.path = ctx.diag.path[:len(ctx.diag.path)-1]
	}
}

// diagEnter records that o is about to be converted to typ. It reports an ExportErrorCycle if the
// same object is already being converted to the same type further up the traversal chain. Objects
// referenced from independent branches do not appear in the chain and therefore do not trigger a
// false positive; references that can be satisfied from the export cache (aliases) are resolved
// before this check is reached.
func (ctx *objectExportCtx) diagEnter(o *Object, typ reflect.Type) error {
	if ctx.diag == nil {
		return nil
	}
	key := exportChainKey{obj: o, typ: typ}
	if _, exists := ctx.diag.chain[key]; exists {
		return &ExportError{
			Kind: ExportErrorCycle,
			Path: ctx.diag.pathSnapshot(),
			Type: typ,
			Err:  fmt.Errorf("circular reference: the object is already being converted to %v", typ),
		}
	}
	if ctx.diag.chain == nil {
		ctx.diag.chain = make(map[exportChainKey]struct{})
	}
	ctx.diag.chain[key] = struct{}{}
	return nil
}

func (ctx *objectExportCtx) diagLeave(o *Object, typ reflect.Type) {
	if ctx.diag != nil {
		delete(ctx.diag.chain, exportChainKey{obj: o, typ: typ})
	}
}

// diagError wraps err into an *ExportError capturing the current path, unless diagnostics are
// disabled (in which case err is returned unchanged) or err already contains an *ExportError.
func (ctx *objectExportCtx) diagError(kind ExportErrorKind, typ reflect.Type, err error) error {
	if ctx.diag == nil || err == nil {
		return err
	}
	var exportErr *ExportError
	if errors.As(err, &exportErr) {
		return err
	}
	if _, ok := err.(*Exception); ok {
		kind = ExportErrorGetterException
	}
	return &ExportError{
		Kind: kind,
		Path: ctx.diag.pathSnapshot(),
		Type: typ,
		Err:  err,
	}
}

// exportCheckedNumber performs a diagnostics-aware conversion of v into a numeric Go type.
// Unlike the standard ExportTo conversion, which applies the ECMAScript ToInt32/ToUint32/...
// semantics and silently wraps or truncates, this conversion reports out-of-range and
// unrepresentable values as errors. It returns handled=false for non-numeric kinds.
func (r *Runtime) exportCheckedNumber(v Value, dst reflect.Value, typ reflect.Type, kind reflect.Kind, ctx *objectExportCtx) (handled bool, err error) {
	var bits int
	var unsigned bool
	switch kind {
	case reflect.Int:
		bits = strconv.IntSize
	case reflect.Int8:
		bits = 8
	case reflect.Int16:
		bits = 16
	case reflect.Int32:
		bits = 32
	case reflect.Int64:
		bits = 64
	case reflect.Uint:
		bits, unsigned = strconv.IntSize, true
	case reflect.Uint8:
		bits, unsigned = 8, true
	case reflect.Uint16:
		bits, unsigned = 16, true
	case reflect.Uint32:
		bits, unsigned = 32, true
	case reflect.Uint64:
		bits, unsigned = 64, true
	case reflect.Float32:
		var f float64
		ex := r.vm.try(func() {
			f = v.ToFloat()
		})
		if ex != nil {
			return true, ctx.diagError(ExportErrorGetterException, typ, ex)
		}
		f32 := float32(f)
		if math.IsInf(float64(f32), 0) && !math.IsInf(f, 0) {
			return true, ctx.diagError(ExportErrorUnrepresentableNumber, typ, fmt.Errorf("%v cannot be represented as %v", v, typ))
		}
		dst.Set(reflect.ValueOf(f32).Convert(typ))
		return true, nil
	default:
		return false, nil
	}

	var num Value
	ex := r.vm.try(func() {
		num = v.ToNumber()
	})
	if ex != nil {
		return true, ctx.diagError(ExportErrorGetterException, typ, ex)
	}
	f := num.ToFloat()
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return true, ctx.diagError(ExportErrorUnrepresentableNumber, typ, fmt.Errorf("%v cannot be represented as %v", v, typ))
	}
	if f != math.Trunc(f) {
		return true, ctx.diagError(ExportErrorUnrepresentableNumber, typ, fmt.Errorf("%v cannot be represented as %v without losing the fractional part", v, typ))
	}
	if unsigned {
		if f < 0 || f >= math.Ldexp(1, bits) {
			return true, ctx.diagError(ExportErrorIntOverflow, typ, fmt.Errorf("%v overflows %v", v, typ))
		}
		dst.Set(reflect.ValueOf(uint64(f)).Convert(typ))
	} else {
		if f < -math.Ldexp(1, bits-1) || f >= math.Ldexp(1, bits-1) {
			return true, ctx.diagError(ExportErrorIntOverflow, typ, fmt.Errorf("%v overflows %v", v, typ))
		}
		dst.Set(reflect.ValueOf(int64(f)).Convert(typ))
	}
	return true, nil
}

// ExportToWithDiagnostics converts a JavaScript value into the specified Go value. The second
// parameter must be a non-nil pointer.
//
// It follows the same conversion rules as ExportTo with the following differences:
//
//   - Conversions to Go integer types are checked: a fractional value, NaN or +/-Inf results in
//     ExportErrorUnrepresentableNumber and an integer that does not fit into the target type
//     results in ExportErrorIntOverflow, instead of being silently truncated or wrapped around.
//   - JavaScript exceptions thrown by getters, valueOf or iterators are caught and returned as
//     errors with kind ExportErrorGetterException instead of being propagated as panics.
//   - Circular references that cannot be represented in the target Go type are reported as
//     ExportErrorCycle. Circular references that the target type can represent (e.g. through
//     pointer, map or slice types) are preserved as aliases, same as in ExportTo.
//   - The conversion is atomic: if it fails, the target retains the value it had before the call.
//
// Failures are returned as *ExportError which records the kind of the failure, the path to the
// offending value as a slice of ExportPathSegment, the target Go type at that position and the
// original cause (available via Unwrap).
func (r *Runtime) ExportToWithDiagnostics(v Value, target interface{}) (err error) {
	tval := reflect.ValueOf(target)
	if tval.Kind() != reflect.Ptr || tval.IsNil() {
		return errors.New("target must be a non-nil pointer")
	}
	dst := tval.Elem()
	// Convert into a temporary value so that the target is left untouched if the conversion fails.
	tmp := reflect.New(dst.Type()).Elem()
	ctx := &objectExportCtx{diag: &exportDiag{}}
	ex := r.vm.try(func() {
		err = r.toReflectValue(v, tmp, ctx)
	})
	if ex != nil {
		return &ExportError{
			Kind: ExportErrorGetterException,
			Path: ctx.diag.pathSnapshot(),
			Type: dst.Type(),
			Err:  ex,
		}
	}
	if err != nil {
		return err
	}
	dst.Set(tmp)
	return nil
}
