package goja

import (
	"errors"
	"fmt"
	"go/ast"
	"math"
	"math/big"
	"reflect"

	"github.com/dop251/goja/unistring"
)

// ExportPathSegment describes a single step of the traversal path captured by an
// ExportError. Concrete types are ExportPropertyPathSegment, ExportIndexPathSegment
// and ExportKeyPathSegment.
type ExportPathSegment interface {
	exportPathSegment()
}

// ExportPropertyPathSegment is an object property access on the path of an
// export operation.
type ExportPropertyPathSegment struct {
	Name string
}

func (ExportPropertyPathSegment) exportPathSegment() {}

// ExportIndexPathSegment is an array (or array-like) index access on the path of
// an export operation.
type ExportIndexPathSegment struct {
	Index int
}

func (ExportIndexPathSegment) exportPathSegment() {}

// ExportKeyPathSegment is a Map entry key or a Set element on the path of an
// export operation. Unlike properties, keys are not identifiers and may have
// arbitrary Go or JavaScript types, therefore they are kept as a Value rather
// than rendered into a string.
type ExportKeyPathSegment struct {
	// Key is the Map key or Set element value. Note that its String() method may
	// throw (e.g. for a Proxy with a throwing toString); use
	// (ExportError).Error() which renders such values as "<unprintable>".
	Key Value
}

func (ExportKeyPathSegment) exportPathSegment() {}

// ExportErrorKind categorises the failure reported by ExportToWithDiagnostics.
type ExportErrorKind int

const (
	// ExportErrorKindUnspecified is used for failures that do not fall into any
	// of the other categories (e.g. mismatched array lengths).
	ExportErrorKindUnspecified ExportErrorKind = iota
	// ExportErrorKindIntegerOverflow indicates a numeric value (including a
	// BigInt) cannot fit into the target integer type.
	ExportErrorKindIntegerOverflow
	// ExportErrorKindUnrepresentableNumber indicates a numeric value cannot be
	// represented by the target numeric type. This only covers BigInts whose
	// magnitude overflows the target floating-point type; ECMAScript numbers
	// (including NaN and infinity) follow the standard ToInt/ToFloat
	// conversions and never trigger it.
	ExportErrorKindUnrepresentableNumber
	// ExportErrorKindTypeMismatch indicates the JavaScript value is not
	// convertible to the target Go type.
	ExportErrorKindTypeMismatch
	// ExportErrorKindGetterThrow indicates a property getter (or another piece
	// of JavaScript code invoked during traversal) threw an exception. The
	// original exception is available via errors.As / Unwrap.
	ExportErrorKindGetterThrow
	// ExportErrorKindCycle indicates the current traversal chain reached an
	// object again, but the target Go type cannot represent the resulting
	// reference cycle. Aliases that the target type is able to preserve are not
	// reported as errors.
	ExportErrorKindCycle
)

// String returns a human-readable description of the kind.
func (k ExportErrorKind) String() string {
	switch k {
	case ExportErrorKindIntegerOverflow:
		return "integer overflow"
	case ExportErrorKindUnrepresentableNumber:
		return "unrepresentable number"
	case ExportErrorKindTypeMismatch:
		return "type mismatch"
	case ExportErrorKindGetterThrow:
		return "getter threw an exception"
	case ExportErrorKindCycle:
		return "cycle not representable by target type"
	default:
		return "conversion error"
	}
}

// ExportError describes why an ExportToWithDiagnostics call failed.
//
// Path contains the structured traversal steps from the exported value to the
// value that could not be converted. Property names are returned verbatim and
// never rendered, so names containing dots or brackets cannot be confused with
// index segments.
//
// TargetType is the Go type the failing value was being converted to.
//
// The original reason (including a thrown *Exception) is returned by Unwrap
// and can be inspected with errors.As and errors.Is.
type ExportError struct {
	Kind       ExportErrorKind
	Path       []ExportPathSegment
	TargetType reflect.Type
	Cause      error
	// Message is an optional extra detail. It is empty for failures that are
	// fully described by Kind and Cause.
	Message string
}

func (e *ExportError) Error() string {
	msg := e.Kind.String()
	if len(e.Path) > 0 {
		msg = fmt.Sprintf("%s at %s", msg, formatExportPath(e.Path))
	}
	if e.TargetType != nil {
		msg = fmt.Sprintf("%s (target %s)", msg, e.TargetType)
	}
	if e.Message != "" {
		msg = msg + ": " + e.Message
	}
	if e.Cause != nil {
		msg = msg + ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap returns the original cause of the failure. It is usually a *Exception
// thrown by a getter or another piece of JavaScript code.
func (e *ExportError) Unwrap() error {
	return e.Cause
}

// formatExportPath renders the path for error messages only. The structured
// data (ExportError.Path) remains the unambiguous representation.
func formatExportPath(path []ExportPathSegment) string {
	var b []byte
	for _, seg := range path {
		switch s := seg.(type) {
		case ExportPropertyPathSegment:
			if len(b) == 0 {
				b = append(b, '$')
			}
			b = append(b, '.')
			b = append(b, s.Name...)
		case ExportIndexPathSegment:
			b = append(b, '[')
			b = fmt.Appendf(b, "%d", s.Index)
			b = append(b, ']')
		case ExportKeyPathSegment:
			if len(b) == 0 {
				b = append(b, '$')
			}
			b = append(b, '[')
			b = append(b, safeValueString(s.Key)...)
			b = append(b, ']')
		}
	}
	if len(b) == 0 {
		return "$"
	}
	return string(b)
}

// safeValueString renders a Value for an error message without letting
// exceptions thrown by its toString/valueOf escape.
func safeValueString(v Value) (s string) {
	defer func() {
		if r := recover(); r != nil {
			s = "<unprintable>"
		}
	}()
	return v.String()
}

// diagnosticExportCtx carries the structured diagnostics state on top of the
// plain alias cache used by every export.
type diagnosticExportCtx struct {
	path      []ExportPathSegment
	active    map[*Object]struct{}
	rollbacks []func()
}

func (ctx *objectExportCtx) pushSegment(seg ExportPathSegment) {
	if ctx.diag != nil {
		ctx.diag.path = append(ctx.diag.path, seg)
	}
}

func (ctx *objectExportCtx) popSegment() {
	if ctx.diag != nil {
		ctx.diag.path = ctx.diag.path[:len(ctx.diag.path)-1]
	}
}

// enterActive marks o as currently being traversed. It returns false when o is
// already on the current traversal chain (a back edge). The alias cache is
// consulted separately and takes precedence, so an object shared between two
// independent branches (a diamond) is never reported as a cycle.
func (ctx *objectExportCtx) enterActive(o *Object) bool {
	if ctx.diag == nil {
		return true
	}
	if _, exists := ctx.diag.active[o]; exists {
		return false
	}
	if ctx.diag.active == nil {
		ctx.diag.active = make(map[*Object]struct{})
	}
	ctx.diag.active[o] = struct{}{}
	return true
}

func (ctx *objectExportCtx) leaveActive(o *Object) {
	if ctx.diag == nil {
		return
	}
	delete(ctx.diag.active, o)
}

func (ctx *objectExportCtx) snapshotPath() []ExportPathSegment {
	if ctx.diag == nil {
		return nil
	}
	path := make([]ExportPathSegment, len(ctx.diag.path))
	copy(path, ctx.diag.path)
	return path
}

func (ctx *objectExportCtx) newExportError(kind ExportErrorKind, target reflect.Type, cause error, message string) *ExportError {
	return &ExportError{
		Kind:       kind,
		Path:       ctx.snapshotPath(),
		TargetType: target,
		Cause:      cause,
		Message:    message,
	}
}

// trap runs f, converting a thrown *Exception (or any other panic that is not
// already an ExportError) into an ExportError of the given kind. When
// diagnostics are disabled the original panic is re-raised to preserve the
// existing behaviour.
func (ctx *objectExportCtx) trap(kind ExportErrorKind, target reflect.Type, f func() error) error {
	if ctx.diag == nil {
		return f()
	}
	var ret error
	func() {
		defer func() {
			if r := recover(); r != nil {
				if e, ok := r.(*ExportError); ok {
					ret = e
					return
				}
				if err, ok := r.(error); ok {
					var ex *Exception
					if errors.As(err, &ex) {
						ret = ctx.newExportError(kind, target, ex, "")
						return
					}
					ret = ctx.newExportError(kind, target, err, "")
					return
				}
				ret = ctx.newExportError(kind, target, nil, fmt.Sprint(r))
			}
		}()
		ret = f()
	}()
	return ret
}

// at pushes seg, runs f and pops seg again. A returned error is used as-is: the
// error already carries a snapshot of the full path.
func (ctx *objectExportCtx) at(seg ExportPathSegment, f func() error) error {
	ctx.pushSegment(seg)
	err := f()
	ctx.popSegment()
	return err
}

// wrapFailure upgrades a plain conversion error returned by the legacy code
// paths into an ExportError when diagnostics are enabled. It leaves an
// existing *ExportError untouched.
func (ctx *objectExportCtx) wrapFailure(target reflect.Type, kind ExportErrorKind, err error) error {
	if err == nil || ctx.diag == nil {
		return err
	}
	if e, ok := err.(*ExportError); ok {
		return e
	}
	var ex *Exception
	if errors.As(err, &ex) {
		if kind == ExportErrorKindUnspecified {
			kind = ExportErrorKindGetterThrow
		}
		return ctx.newExportError(kind, target, ex, "")
	}
	return ctx.newExportError(kind, target, err, "")
}

// numericExportError validates a value before a numeric conversion in
// diagnostic mode. It mirrors the ECMAScript conversions used by ToInt32 and
// friends (e.g. NaN/infinity convert to 0 and out-of-range numbers wrap
// around), so ordinary truncation remains allowed. The only extra restrictions
// apply to BigInt values, which cannot be converted implicitly by ECMAScript:
// values that do not fit into the target integer type are reported as integer
// overflow, and values that cannot be represented as a float (overflow to
// +/-infinity) are reported as unrepresentable numbers.
func (ctx *objectExportCtx) numericExportError(v Value, typ reflect.Type) error {
	if bi, ok := v.(*valueBigInt); ok {
		if typ.Kind() == reflect.Float32 || typ.Kind() == reflect.Float64 {
			// Same semantics as BigInt.prototype toNumber: loss of precision is
			// allowed, but values that become +/-infinity are not representable.
			f, _ := new(big.Float).SetInt((*big.Int)(bi)).Float64()
			if math.IsInf(f, 0) {
				return ctx.newExportError(ExportErrorKindUnrepresentableNumber, typ, nil,
					"BigInt value is not representable by "+typ.String())
			}
			return nil
		}
		i := (*big.Int)(bi)
		lo, hi := bigIntRange(typ)
		if lo != nil && i.Cmp(lo) < 0 || hi != nil && i.Cmp(hi) > 0 {
			return ctx.newExportError(ExportErrorKindIntegerOverflow, typ, nil,
				"BigInt value "+i.String()+" does not fit into "+typ.String())
		}
		return nil
	}
	return nil
}

// bigIntRange returns the inclusive [min, max] BigInt values representable by
// the integer type typ.
func bigIntRange(typ reflect.Type) (min, max *big.Int) {
	bitSize := uint(typ.Bits())
	switch typ.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		max = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bitSize-1), big.NewInt(1))
		min = new(big.Int).Neg(new(big.Int).Add(max, big.NewInt(1)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		min = big.NewInt(0)
		max = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bitSize), big.NewInt(1))
	}
	return
}

// getStrTrap reads a string property, turning a thrown exception into an
// ExportError when diagnostics are enabled.
func (ctx *objectExportCtx) getStrTrap(o *Object, name unistring.String, target reflect.Type) (Value, error) {
	if ctx.diag == nil {
		return o.self.getStr(name, nil), nil
	}
	var res Value
	err := ctx.trap(ExportErrorKindGetterThrow, target, func() error {
		res = o.self.getStr(name, nil)
		return nil
	})
	return res, err
}

// snapshotRoot records the original contents of the conversion destination,
// including values reachable through pre-existing pointers, slices, maps and
// interfaces, so a failed conversion can restore the whole reachable state.
func (ctx *objectExportCtx) snapshotRoot(dst reflect.Value) {
	if ctx.diag == nil {
		return
	}
	saved := reflect.New(dst.Type()).Elem()
	saved.Set(dst)
	ctx.diag.rollbacks = append(ctx.diag.rollbacks, func() { dst.Set(saved) })
	ctx.snapshotChildren(dst)
}

// snapshotChildren records the mutable state reachable through dst that
// toReflectValue modifies in place.
func (ctx *objectExportCtx) snapshotChildren(dst reflect.Value) {
	switch dst.Kind() {
	case reflect.Ptr:
		if dst.IsNil() {
			return
		}
		ctx.snapshotPoinee(dst.Elem())
	case reflect.Slice, reflect.Map:
		if dst.IsNil() {
			return
		}
		saved := reflect.New(dst.Type()).Elem()
		saved.Set(dst)
		ctx.diag.rollbacks = append(ctx.diag.rollbacks, func() { dst.Set(saved) })
	case reflect.Struct:
		typ := dst.Type()
		for i := 0; i < typ.NumField(); i++ {
			if ast.IsExported(typ.Field(i).Name) {
				ctx.snapshotChildren(dst.Field(i))
			}
		}
	}
}

// snapshotPoinee records the state of the value behind a pointer that is about
// to be converted in place.
func (ctx *objectExportCtx) snapshotPoinee(dst reflect.Value) {
	if ctx.diag == nil {
		return
	}
	switch dst.Kind() {
	case reflect.Struct:
		saved := reflect.New(dst.Type()).Elem()
		saved.Set(dst)
		ctx.diag.rollbacks = append(ctx.diag.rollbacks, func() { dst.Set(saved) })
		ctx.snapshotChildren(dst)
	case reflect.Slice, reflect.Map:
		if dst.IsNil() {
			return
		}
		saved := reflect.New(dst.Type()).Elem()
		saved.Set(dst)
		ctx.diag.rollbacks = append(ctx.diag.rollbacks, func() { dst.Set(saved) })
	}
}

func (ctx *objectExportCtx) restoreSnapshots() {
	if ctx.diag == nil {
		return
	}
	for i := len(ctx.diag.rollbacks) - 1; i >= 0; i-- {
		ctx.diag.rollbacks[i]()
	}
	ctx.diag.rollbacks = nil
}

func (r *Runtime) setReflectString(v Value, dst reflect.Value, typ reflect.Type, ctx *objectExportCtx) error {
	var s string
	err := ctx.trap(ExportErrorKindGetterThrow, typ, func() error {
		s = v.String()
		return nil
	})
	if err != nil {
		return err
	}
	dst.Set(reflect.ValueOf(s).Convert(typ))
	return nil
}

func (r *Runtime) setReflectBool(v Value, dst reflect.Value, typ reflect.Type, ctx *objectExportCtx) error {
	var b bool
	err := ctx.trap(ExportErrorKindGetterThrow, typ, func() error {
		b = v.ToBoolean()
		return nil
	})
	if err != nil {
		return err
	}
	dst.Set(reflect.ValueOf(b).Convert(typ))
	return nil
}

func (r *Runtime) setReflectInt(v Value, dst reflect.Value, typ reflect.Type, ctx *objectExportCtx, conv func(Value) int64) error {
	if ctx.diag != nil {
		if err := ctx.numericExportError(v, typ); err != nil {
			return err
		}
	}
	var i int64
	if bi, ok := v.(*valueBigInt); ok {
		i = (*big.Int)(bi).Int64()
	} else {
		err := ctx.trap(ExportErrorKindGetterThrow, typ, func() error {
			i = conv(v)
			return nil
		})
		if err != nil {
			return err
		}
	}
	dst.Set(reflect.ValueOf(i).Convert(typ))
	return nil
}

func (r *Runtime) setReflectUint(v Value, dst reflect.Value, typ reflect.Type, ctx *objectExportCtx, conv func(Value) uint64) error {
	if ctx.diag != nil {
		if err := ctx.numericExportError(v, typ); err != nil {
			return err
		}
	}
	var u uint64
	if bi, ok := v.(*valueBigInt); ok {
		u = (*big.Int)(bi).Uint64()
	} else {
		err := ctx.trap(ExportErrorKindGetterThrow, typ, func() error {
			u = conv(v)
			return nil
		})
		if err != nil {
			return err
		}
	}
	dst.Set(reflect.ValueOf(u).Convert(typ))
	return nil
}

func (r *Runtime) setReflectFloat(v Value, dst reflect.Value, typ reflect.Type, ctx *objectExportCtx, bits int) error {
	if ctx.diag != nil {
		if err := ctx.numericExportError(v, typ); err != nil {
			return err
		}
	}
	var f float64
	if bi, ok := v.(*valueBigInt); ok {
		f, _ = new(big.Float).SetInt((*big.Int)(bi)).Float64()
	} else {
		err := ctx.trap(ExportErrorKindGetterThrow, typ, func() error {
			f = v.ToFloat()
			return nil
		})
		if err != nil {
			return err
		}
	}
	if bits == 32 {
		f = float64(float32(f))
	}
	dst.Set(reflect.ValueOf(f).Convert(typ))
	return nil
}
