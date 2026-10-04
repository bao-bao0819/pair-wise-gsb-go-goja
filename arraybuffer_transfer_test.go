package goja

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unsafe"
)

func dataPtr(b []byte) uintptr {
	if len(b) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))
}

func requireTypeError(t *testing.T, err error, what string) {
	t.Helper()
	var ex *Exception
	if !errors.As(err, &ex) {
		t.Fatalf("%s: expected TypeError exception, got %T: %v", what, err, err)
	}
	obj, ok := ex.val.(*Object)
	if !ok {
		t.Fatalf("%s: expected JS error object, got %T", what, ex.val)
	}
	if name := obj.Get("name").String(); name != "TypeError" {
		t.Fatalf("%s: expected TypeError, got %s (%v)", what, name, err)
	}
}

func requireErrIs(t *testing.T, err, target error, what string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("%s: expected error wrapping %v, got %v", what, target, err)
	}
}

// TestTransferArrayBuffersZeroCopy is the direct evidence that no bytes are
// copied: the destination data pointer equals the source data pointer, while
// the source stops referencing the storage entirely.
func TestTransferArrayBuffersZeroCopy(t *testing.T) {
	src := New()
	dst := New()

	const size = 1 << 20 // 1 MiB
	payload := bytes.Repeat([]byte{0xAB}, size)
	srcBuf := src.NewArrayBuffer(payload)
	srcObj := srcBuf.buf
	beforePtr := dataPtr(srcObj.data)

	out, err := TransferArrayBuffers(dst, src, []ArrayBuffer{srcBuf}, ArrayBufferTransferOptions{})
	if err != nil {
		t.Fatalf("transfer failed: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 output, got %d", len(out))
	}

	dstObj := out[0].buf
	if dstObj.val.runtime != dst {
		t.Fatal("destination buffer belongs to the wrong Runtime")
	}
	if p := dataPtr(dstObj.data); p != beforePtr {
		t.Fatalf("storage was copied: before=%x after=%x", beforePtr, p)
	}
	if srcObj.data != nil {
		t.Fatalf("source still references storage after transfer: %x", dataPtr(srcObj.data))
	}
	if !srcObj.detached {
		t.Fatal("source not marked detached")
	}
	if len(dstObj.data) != size {
		t.Fatalf("destination length: got %d want %d", len(dstObj.data), size)
	}
	if dstObj.data[0] != 0xAB || dstObj.data[size-1] != 0xAB {
		t.Fatal("destination does not preserve original bytes")
	}
	if !srcBuf.Detached() {
		t.Fatal("source wrapper Detached() == false")
	}

	// The destination wrapper must be usable and must expose the exact storage.
	if p := dataPtr(out[0].Bytes()); p != beforePtr {
		t.Fatalf("destination Bytes() pointer %x != original %x", p, beforePtr)
	}

	// And usable from ECMAScript code in the destination Runtime.
	if err := dst.Set("dbuf", out[0]); err != nil {
		t.Fatal(err)
	}
	ok, err := dst.RunString(`
	var d = new Uint8Array(dbuf);
	var good = d.length === 1048576 && d[0] === 0xAB && d[1048575] === 0xAB;
	d[0] = 0x77;
	good && d[0] === 0x77;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if !ok.ToBoolean() {
		t.Fatal("destination ECMAScript view incorrect")
	}
}

// TestTransferBatchRelinksMultipleBuffers moves several buffers at once and
// checks every destination pointer matches its source pointer.
func TestTransferBatchRelinksMultipleBuffers(t *testing.T) {
	src := New()
	dst := New()

	sizes := []int{0, 1, 7, 4096, 65537}
	srcs := make([]ArrayBuffer, len(sizes))
	ptrs := make([]uintptr, len(sizes))
	for i, n := range sizes {
		b := bytes.Repeat([]byte{byte(i + 1)}, n)
		ab := src.NewArrayBuffer(b)
		srcs[i] = ab
		ptrs[i] = dataPtr(ab.buf.data)
	}

	out, err := TransferArrayBuffers(dst, src, srcs, ArrayBufferTransferOptions{})
	if err != nil {
		t.Fatalf("batch transfer failed: %v", err)
	}
	for i, got := range out {
		if got.buf.val.runtime != dst {
			t.Fatalf("entry %d in wrong runtime", i)
		}
		if len(got.buf.data) != sizes[i] {
			t.Fatalf("entry %d length: got %d want %d", i, len(got.buf.data), sizes[i])
		}
		if p := dataPtr(got.buf.data); p != ptrs[i] {
			t.Fatalf("entry %d copied: %x != %x", i, p, ptrs[i])
		}
		if !srcs[i].Detached() {
			t.Fatalf("entry %d source not detached", i)
		}
		for j := 0; j < sizes[i]; j++ {
			if got.buf.data[j] != byte(i+1) {
				t.Fatalf("entry %d byte %d corrupted", i, j)
			}
		}
	}
}

// TestTransferOverlappingViewsDetached creates many overlapping views over one
// buffer and asserts full ECMAScript detached semantics after transfer.
func TestTransferOverlappingViewsDetached(t *testing.T) {
	src := New()
	dst := New()

	v, err := src.RunString(`
	var buf = new ArrayBuffer(32);
	var u8 = new Uint8Array(buf);
	for (var i = 0; i < 32; i++) u8[i] = i + 1;
	var u16 = new Uint16Array(buf, 2, 10);
	var i32 = new Int32Array(buf, 8, 4);
	var sub = new Uint8Array(buf).subarray(5, 20);
	var f64 = new Float64Array(buf, 16, 2);
	var dv = new DataView(buf, 4, 12);
	var u8c = new Uint8ClampedArray(buf, 0, 32);
	buf;
	`)
	if err != nil {
		t.Fatal(err)
	}
	srcWrapper := extractBufferWrapper(t, v)

	// Views are live before transfer.
	pre, err := src.RunString(`
	u8.length === 32 && u16.length === 10 && i32.length === 4 &&
	sub.length === 15 && f64.length === 2 && u8c.length === 32 &&
	u8[0] === 1 && u16[0] === 0x0403 && dv.getUint8(0) === 5 &&
	buf.byteLength === 32;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if !pre.ToBoolean() {
		t.Fatal("pre-transfer view sanity failed")
	}

	transferred, err := TransferArrayBuffers(dst, src, []ArrayBuffer{srcWrapper}, ArrayBufferTransferOptions{})
	if err != nil {
		t.Fatalf("transfer failed: %v", err)
	}

	// Length / byteLength / byteOffset getters report the detached state.
	zeroes, err := src.RunString(`
	var nums = [buf.byteLength,
	           u8.byteLength, u8.length, u8.byteOffset,
	           u16.byteLength, u16.length, u16.byteOffset,
	           i32.byteLength, i32.length,
	           sub.byteLength, sub.length,
	           f64.byteLength, f64.length,
	           u8c.byteLength, u8c.length];
	nums.every(function (n) { return n === 0; });
	`)
	if err != nil {
		t.Fatal(err)
	}
	if !zeroes.ToBoolean() {
		t.Fatal("detached length getters did not all report 0")
	}

	// Indexed reads are undefined; indexed writes are ignored.
	idx, err := src.RunString(`
	var reads = [u8[0], u16[0], i32[0], sub[2], f64[1], u8c[31], u8[99999]];
	var allUndef = reads.every(function (x) { return x === undefined; });
	u8[0] = 99; u16[1] = 1; i32[0] = 2; f64[0] = 1.5; u8c[0] = 7;
	allUndef && u8[0] === undefined && u16[1] === undefined && i32[0] === undefined;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if !idx.ToBoolean() {
		t.Fatal("detached indexed access did not behave as undefined / ignored writes")
	}

	// DataView and TypedArray methods that touch storage throw TypeError.
	for _, script := range []string{
		`dv.byteLength`,
		`dv.byteOffset`,
		`dv.getUint8(0)`,
		`dv.getUint32(0, true)`,
		`dv.setUint8(0, 1)`,
		`u8.set([1,2,3])`,
		`u8.slice(0,1)`,
		`new Uint8Array(buf, 0, 1)`,
		`i32.sort()`,
	} {
		_, err := src.RunString(script)
		if err == nil {
			t.Fatalf("%s on detached buffer: expected TypeError, got nil", script)
		}
		requireTypeError(t, err, script)
	}

	// Go wrapper of the source reports detached and exposes nil bytes.
	if !srcWrapper.Detached() {
		t.Fatal("source Go wrapper not detached")
	}
	if b := srcWrapper.Bytes(); b != nil {
		t.Fatalf("source Go wrapper Bytes() = %d bytes, want nil", len(b))
	}

	// Destination preserves the byte pattern and is independently usable.
	if err := dst.Set("dbuf", transferred[0]); err != nil {
		t.Fatal(err)
	}
	ok, err := dst.RunString(`
	var d = new Uint8Array(dbuf);
	var good = d.length === 32;
	for (var i = 0; i < 32; i++) if (d[i] !== i + 1) good = false;
	var view16 = new Uint16Array(dbuf, 2, 10);
	good && view16[0] === 0x0403;
	`)
	if err != nil {
		t.Fatal(err)
	}
	if !ok.ToBoolean() {
		t.Fatal("destination content not preserved")
	}
}

func extractBufferWrapper(t *testing.T, v Value) ArrayBuffer {
	t.Helper()
	obj, ok := v.(*Object)
	if !ok {
		t.Fatalf("expected *Object, got %T", v)
	}
	buf, ok := obj.self.(*arrayBufferObject)
	if !ok {
		t.Fatalf("expected *arrayBufferObject, got %T", obj.self)
	}
	return buf.export(nil).(ArrayBuffer)
}

// makeFreshBuffers returns n non-detached source buffers with known content
// plus their underlying objects.
func makeFreshBuffers(t *testing.T, r *Runtime, n int) ([]ArrayBuffer, []*arrayBufferObject) {
	t.Helper()
	bufs := make([]ArrayBuffer, n)
	objs := make([]*arrayBufferObject, n)
	for i := 0; i < n; i++ {
		ab := r.NewArrayBuffer([]byte{byte(i + 1), byte(i + 2), byte(i + 3)})
		bufs[i] = ab
		objs[i] = ab.buf
	}
	return bufs, objs
}

// TestTransferAtomicityOnLastBadEntry puts an illegal entry at the end of an
// otherwise valid batch and verifies that no earlier source gets detached.
func TestTransferAtomicityOnLastBadEntry(t *testing.T) {
	src := New()
	dst := New()

	bufs, objs := makeFreshBuffers(t, src, 3)

	t.Run("already detached entry at end", func(t *testing.T) {
		// Detach the last buffer itself, then send [good0, good1, detachedLast].
		last := src.NewArrayBuffer([]byte{9, 9, 9})
		if !last.Detach() {
			t.Fatal("pre-detach failed")
		}
		batch := []ArrayBuffer{bufs[0], bufs[1], last}
		out, err := TransferArrayBuffers(dst, src, batch, ArrayBufferTransferOptions{})
		requireErrIs(t, err, ErrArrayBufferDetached, "detached last entry")
		if out != nil {
			t.Fatalf("expected nil result on failure, got %d", len(out))
		}
		for i, obj := range objs[:2] {
			if obj.detached {
				t.Fatalf("valid buffer %d detached despite failed batch", i)
			}
			if len(obj.data) != 3 {
				t.Fatalf("valid buffer %d lost its data", i)
			}
		}
	})

	t.Run("wrong-runtime entry at end", func(t *testing.T) {
		other := New().NewArrayBuffer([]byte{5, 5, 5})
		batch := []ArrayBuffer{bufs[0], bufs[1], bufs[2], other}
		_, err := TransferArrayBuffers(dst, src, batch, ArrayBufferTransferOptions{})
		requireErrIs(t, err, ErrWrongSourceRuntime, "foreign last entry")
		for i, obj := range objs {
			if obj.detached {
				t.Fatalf("valid buffer %d detached despite failed batch", i)
			}
		}
		// The foreign buffer is untouched and usable in its own runtime.
		if other.Detached() {
			t.Fatal("foreign buffer was detached by a failed transfer")
		}
	})

	t.Run("duplicate underlying buffer at end", func(t *testing.T) {
		batch := []ArrayBuffer{bufs[0], bufs[1], bufs[2], bufs[0]}
		_, err := TransferArrayBuffers(dst, src, batch, ArrayBufferTransferOptions{})
		requireErrIs(t, err, ErrDuplicateTransferEntry, "duplicate last entry")
		for i, obj := range objs {
			if obj.detached {
				t.Fatalf("valid buffer %d detached despite failed batch", i)
			}
		}
	})

	t.Run("two views of same buffer", func(t *testing.T) {
		// Two Go wrappers obtained over the same underlying engine buffer
		// (e.g. via JS) must be treated as duplicates, not two buffers.
		v, err := src.RunString(`
		var b = new ArrayBuffer(8);
		var x = new Uint8Array(b);
		var y = new Uint16Array(b);
		[b, x.buffer, y.buffer];
		`)
		if err != nil {
			t.Fatal(err)
		}
		wrappers := exportArrayWrappers(t, v)
		if len(wrappers) != 3 {
			t.Fatalf("want 3 wrappers, got %d", len(wrappers))
		}
		_, err = TransferArrayBuffers(dst, src, wrappers, ArrayBufferTransferOptions{})
		requireErrIs(t, err, ErrDuplicateTransferEntry, "three views one buffer")
		if wrappers[0].Detached() {
			t.Fatal("buffer detached despite duplicate rejection")
		}
	})

	t.Run("nil entry in middle", func(t *testing.T) {
		batch := []ArrayBuffer{bufs[0], {}, bufs[2]}
		_, err := TransferArrayBuffers(dst, src, batch, ArrayBufferTransferOptions{})
		requireErrIs(t, err, ErrInvalidTransferArgument, "nil entry")
		for i, obj := range []*arrayBufferObject{objs[0], objs[2]} {
			if obj.detached {
				t.Fatalf("valid buffer %d detached despite failed batch", i)
			}
		}
	})

	// After all the failed attempts, a clean batch still works.
	out, err := TransferArrayBuffers(dst, src, bufs, ArrayBufferTransferOptions{})
	if err != nil {
		t.Fatalf("clean batch after failures failed: %v", err)
	}
	for i, obj := range objs {
		if !obj.detached {
			t.Fatalf("buffer %d not detached after successful retry", i)
		}
		if !bufs[i].Detached() {
			t.Fatalf("wrapper %d not detached after successful retry", i)
		}
		if out[i].buf.val.runtime != dst {
			t.Fatalf("output %d in wrong runtime", i)
		}
	}
}

func exportArrayWrappers(t *testing.T, v Value) []ArrayBuffer {
	t.Helper()
	obj, ok := v.(*Object)
	if !ok {
		t.Fatalf("expected array object, got %T", v)
	}
	lengthVal := obj.Get("length")
	n := int(lengthVal.ToInteger())
	out := make([]ArrayBuffer, n)
	for i := 0; i < n; i++ {
		el := obj.Get(strconv.Itoa(i))
		elObj, ok := el.(*Object)
		if !ok {
			t.Fatalf("element %d not object: %T", i, el)
		}
		buf, ok := elObj.self.(*arrayBufferObject)
		if !ok {
			t.Fatalf("element %d not arraybuffer: %T", i, elObj.self)
		}
		out[i] = buf.export(nil).(ArrayBuffer)
	}
	return out
}

// TestTransferValidationErrors covers the remaining standalone validation
// failures.
func TestTransferValidationErrors(t *testing.T) {
	src := New()
	dst := New()

	if _, err := TransferArrayBuffers(dst, src, nil, ArrayBufferTransferOptions{}); err != nil {
		t.Fatalf("empty batch should succeed, got %v", err)
	}
	if _, err := TransferArrayBuffers(nil, src, nil, ArrayBufferTransferOptions{}); !errors.Is(err, ErrInvalidTransferArgument) {
		t.Fatalf("nil dst: %v", err)
	}
	if _, err := TransferArrayBuffers(dst, nil, nil, ArrayBufferTransferOptions{}); !errors.Is(err, ErrInvalidTransferArgument) {
		t.Fatalf("nil src: %v", err)
	}
	if _, err := TransferArrayBuffers(src, src, []ArrayBuffer{src.NewArrayBuffer(nil)}, ArrayBufferTransferOptions{}); !errors.Is(err, ErrWrongSourceRuntime) {
		t.Fatalf("same runtime: %v", err)
	}

	// Buffer that belongs to the destination cannot be "transferred" into it.
	dstOwned := dst.NewArrayBuffer([]byte{1})
	if _, err := TransferArrayBuffers(dst, src, []ArrayBuffer{dstOwned}, ArrayBufferTransferOptions{}); !errors.Is(err, ErrWrongSourceRuntime) {
		t.Fatalf("destination-owned buffer: %v", err)
	}
	if dstOwned.Detached() {
		t.Fatal("destination-owned buffer got detached by rejected transfer")
	}
}

// TestOwnershipModes verifies the exclusive/external contract.
func TestOwnershipModes(t *testing.T) {
	src := New()
	dst := New()

	// Host-provided storage is external by default.
	payload := make([]byte, 16)
	external := src.NewArrayBuffer(payload)
	if external.Ownership() != OwnershipExternal {
		t.Fatal("NewArrayBuffer must be OwnershipExternal")
	}

	// Engine-allocated storage starts exclusive.
	v, err := src.RunString(`new ArrayBuffer(16)`)
	if err != nil {
		t.Fatal(err)
	}
	engineBuf := extractBufferWrapper(t, v)
	if engineBuf.Ownership() != OwnershipExclusive {
		t.Fatalf("engine ArrayBuffer should be exclusive, got %v", engineBuf.Ownership())
	}

	// AllocateManagedArrayBuffer starts exclusive.
	managedAlloc := src.AllocateManagedArrayBuffer(16)
	if managedAlloc.Ownership() != OwnershipExclusive {
		t.Fatal("AllocateManagedArrayBuffer should be exclusive")
	}

	// Strict exclusive transfer must refuse the external buffer.
	_, err = TransferArrayBuffers(dst, src,
		[]ArrayBuffer{engineBuf, external},
		ArrayBufferTransferOptions{RequireExclusiveOwnership: true})
	requireErrIs(t, err, ErrOwnershipNotAllowed, "external in exclusive batch")
	if external.Detached() {
		t.Fatal("external buffer changed state on rejected batch")
	}
	if engineBuf.Detached() {
		t.Fatal("exclusive buffer detached on rejected batch")
	}

	// Once Go takes bytes, the engine buffer downgrades to external.
	if b := engineBuf.Bytes(); len(b) != 16 {
		t.Fatalf("Bytes() len: %d", len(b))
	}
	if engineBuf.Ownership() != OwnershipExternal {
		t.Fatal("Bytes() must downgrade to OwnershipExternal")
	}
	_, err = TransferArrayBuffers(dst, src, []ArrayBuffer{engineBuf},
		ArrayBufferTransferOptions{RequireExclusiveOwnership: true})
	requireErrIs(t, err, ErrOwnershipNotAllowed, "downgraded buffer in exclusive batch")

	// Export to []byte also downgrades the underlying ArrayBuffer.
	v2, err := src.RunString(`new Uint8Array(8)`)
	if err != nil {
		t.Fatal(err)
	}
	var exported []byte
	if err := src.ExportTo(v2, &exported); err != nil {
		t.Fatal(err)
	}
	taBuf := extractBufferWrapper(t, v2.(*Object).Get("buffer"))
	if taBuf.Ownership() != OwnershipExternal {
		t.Fatal("exporting typed array to []byte must downgrade underlying buffer")
	}

	// Strict exclusive batch with fresh managed buffers moves the sole owner.
	fresh := src.AllocateManagedArrayBuffer(24)
	ptr := dataPtr(fresh.buf.data)
	out, err := TransferArrayBuffers(dst, src, []ArrayBuffer{managedAlloc, fresh},
		ArrayBufferTransferOptions{RequireExclusiveOwnership: true})
	if err != nil {
		t.Fatalf("exclusive batch failed: %v", err)
	}
	if !managedAlloc.Detached() || !fresh.Detached() {
		t.Fatal("sources not detached")
	}
	if dataPtr(out[1].buf.data) != ptr {
		t.Fatal("exclusive transfer copied bytes")
	}
	if out[0].Ownership() != OwnershipExclusive || out[1].Ownership() != OwnershipExclusive {
		t.Fatal("ownership mode not carried to destination")
	}

	// Documented limitation: host aliases from NewArrayBuffer stay valid and
	// share the storage with the destination. We explicitly do not promise
	// those aliases are invalidated.
	payload[0] = 0x42
	ext2 := src.NewArrayBuffer(payload)
	extDst := New()
	out2, err := TransferArrayBuffers(extDst, src, []ArrayBuffer{ext2}, ArrayBufferTransferOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out2[0].buf.data[0] != 0x42 {
		t.Fatal("destination does not see host-written byte")
	}
	if dataPtr(out2[0].buf.data) != dataPtr(payload) {
		t.Fatal("external transfer did not relink same storage")
	}
}

// TestManagedBytesEntryPoints covers the managed-buffer construction contract.
func TestManagedBytesEntryPoints(t *testing.T) {
	src := New()

	mb := NewManagedBytes(12)
	if mb.Len() != 12 || mb.Consumed() {
		t.Fatal("NewManagedBytes basic state wrong")
	}
	ab, err := src.NewManagedArrayBuffer(mb)
	if err != nil {
		t.Fatal(err)
	}
	if ab.Ownership() != OwnershipExclusive {
		t.Fatal("managed buffer must be exclusive")
	}
	if !mb.Consumed() {
		t.Fatal("ManagedBytes not marked consumed")
	}
	if _, err := src.NewManagedArrayBuffer(mb); !errors.Is(err, ErrManagedBytesConsumed) {
		t.Fatalf("double use: %v", err)
	}

	// Fill callback writes directly into owned storage without a second alloc.
	ab2, err := src.NewManagedArrayBufferFunc(100, func(dst []byte) error {
		for i := range dst {
			dst[i] = byte(i)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ab2.buf.data) != 100 || ab2.buf.data[99] != 99 {
		t.Fatal("fill callback did not populate owned storage")
	}

	if _, err := NewManagedBytesFunc(-1, nil); !errors.Is(err, ErrManagedBytesInvalid) {
		t.Fatalf("negative size: %v", err)
	}
	if _, err := NewManagedBytesFunc(4, nil); !errors.Is(err, ErrManagedBytesInvalid) {
		t.Fatalf("nil fill: %v", err)
	}
	fillErr := errors.New("boom")
	if _, err := NewManagedBytesFunc(4, func([]byte) error { return fillErr }); !errors.Is(err, fillErr) {
		t.Fatalf("fill error propagation: %v", err)
	}

	// Reader entry point streams directly into owned storage.
	reader := bytes.NewReader(bytes.Repeat([]byte{0x5A}, 57))
	mbr, err := NewManagedBytesFromReader(reader, 57)
	if err != nil {
		t.Fatal(err)
	}
	ab3, err := src.NewManagedArrayBuffer(mbr)
	if err != nil {
		t.Fatal(err)
	}
	if len(ab3.buf.data) != 57 || ab3.buf.data[56] != 0x5A {
		t.Fatal("reader buffer content wrong")
	}
	if _, err := NewManagedBytesFromReader(nil, 1); !errors.Is(err, ErrManagedBytesInvalid) {
		t.Fatalf("nil reader: %v", err)
	}
	if _, err := NewManagedBytesFromReader(strings.NewReader("x"), 10); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short read: %v", err)
	}

	// Zero-size managed buffers are valid and exclusive.
	empty, err := src.NewManagedArrayBuffer(NewManagedBytes(0))
	if err != nil {
		t.Fatal(err)
	}
	if empty.Ownership() != OwnershipExclusive || len(empty.buf.data) != 0 {
		t.Fatal("empty managed buffer wrong")
	}
}

// TestSharedArrayBufferRejected verifies shared memory is never transferred.
// The engine never creates shared buffers itself; the internal flag is set
// directly to prove the gate exists.
func TestSharedArrayBufferRejected(t *testing.T) {
	src := New()
	dst := New()

	bufObj := src._newArrayBuffer(src.getArrayBufferPrototype(), nil)
	bufObj.data = make([]byte, 16)
	bufObj.shared = true
	shared := ArrayBuffer{buf: bufObj}

	good := src.AllocateManagedArrayBuffer(8)
	_, err := TransferArrayBuffers(dst, src, []ArrayBuffer{good, shared}, ArrayBufferTransferOptions{})
	requireErrIs(t, err, ErrSharedArrayBuffer, "shared buffer")
	if good.Detached() {
		t.Fatal("good buffer detached despite shared-buffer rejection")
	}
	if bufObj.detached {
		t.Fatal("shared buffer itself got detached by rejected transfer")
	}
}

// TestTransferRejectsRuntimeRunningScript blocks src inside a host callback on
// another goroutine and proves a concurrent transfer returns ErrRuntimeBusy
// without detaching anything.
func TestTransferRejectsRuntimeRunningScript(t *testing.T) {
	src := New()
	dst := New()

	entered := make(chan struct{})
	release := make(chan struct{})
	if err := src.Set("block", func() {
		close(entered)
		<-release
	}); err != nil {
		t.Fatal(err)
	}

	runDone := make(chan error, 1)
	go func() {
		_, err := src.RunString(`block();`)
		runDone <- err
	}()

	<-entered

	ab := src.AllocateManagedArrayBuffer(4)
	if _, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab},
		ArrayBufferTransferOptions{RequireExclusiveOwnership: true}); !errors.Is(err, ErrRuntimeBusy) {
		t.Fatalf("expected ErrRuntimeBusy, got %v", err)
	}
	if ab.Detached() {
		t.Fatal("source detached despite busy rejection")
	}

	close(release)
	if err := <-runDone; err != nil {
		t.Fatalf("running script returned error: %v", err)
	}

	// Once the script finished, the same transfer succeeds.
	ab2 := src.AllocateManagedArrayBuffer(4)
	out, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab2},
		ArrayBufferTransferOptions{RequireExclusiveOwnership: true})
	if err != nil {
		t.Fatalf("transfer after completion should succeed: %v", err)
	}
	if !ab2.Detached() || out[0].buf.val.runtime != dst {
		t.Fatal("post-completion transfer state wrong")
	}
}

// TestTransferAndExecutionMutualExclusion hammers transfers into dst while dst
// executes scripts on its own goroutine. Accepted transfers must always carry
// intact bytes; contention may only be reported as ErrRuntimeBusy.
func TestTransferAndExecutionMutualExclusion(t *testing.T) {
	src := New()
	dst := New()

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			ab := src.NewArrayBuffer([]byte{byte(i), byte(i + 1)})
			out, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab}, ArrayBufferTransferOptions{})
			if err != nil {
				if !errors.Is(err, ErrRuntimeBusy) {
					t.Errorf("transfer %d: %v", i, err)
					return
				}
				continue
			}
			if len(out[0].buf.data) != 2 ||
				out[0].buf.data[0] != byte(i) ||
				out[0].buf.data[1] != byte(i+1) {
				t.Errorf("transfer %d corrupted output", i)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if _, err := dst.RunString(`1+1`); err != nil &&
				!errors.Is(err, ErrRuntimeBusy) {
				t.Errorf("dst run %d: %v", i, err)
				return
			}
		}
	}()

	wg.Wait()
}

// TestTransferZeroCopySentinelPattern is a stronger no-copy check: a
// non-repeating sentinel pattern is written into the source storage, the
// buffer is transferred, and the destination storage is asserted to be the
// same allocation via pointer and to contain the identical bytes with no
// intermediate buffer ever allocated by the transfer.
func TestTransferZeroCopySentinelPattern(t *testing.T) {
	src := New()
	dst := New()

	const size = 1 << 16
	payload := make([]byte, size)
	for i := range payload {
		// Pseudo-random non-repeating pattern so a truncated or zeroed copy
		// cannot accidentally match.
		payload[i] = byte(i*31 + 17)
	}

	srcBuf := src.NewArrayBuffer(payload)
	sourceObj := srcBuf.buf

	// A copy-based implementation would create a new backing array whose
	// pointer differs from the source. Capture the exact pointer and length
	// before transfer, then require pointer identity afterwards.
	before := dataPtr(sourceObj.data)

	out, err := TransferArrayBuffers(dst, src, []ArrayBuffer{srcBuf}, ArrayBufferTransferOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if dataPtr(out[0].buf.data) != dataPtr(payload) {
		t.Fatal("destination is not the source allocation")
	}
	if dataPtr(out[0].buf.data) != before {
		t.Fatal("destination pointer differs from pre-transfer source pointer")
	}
	if !bytes.Equal(out[0].buf.data, payload) {
		t.Fatal("sentinel pattern corrupted during zero-copy move")
	}
	if sourceObj.data != nil {
		t.Fatal("source engine object still references moved storage")
	}

	// Mutations on one side are visible through the single allocation: the
	// destination writes hit the array the host originally supplied.
	out[0].buf.data[size-1] = 0x01
	if payload[size-1] != 0x01 {
		t.Fatal("destination and host payload do not share one backing array")
	}
}

// TestDeepCopyIsNotTransfer documents the distinction explicitly: a host-side
// copy (append/bytes.Clone) leaves both sides independently mutable and does not
// detach the source — that is exactly what TransferArrayBuffers does not do.
func TestDeepCopyIsNotTransfer(t *testing.T) {
	src := New()

	v, err := src.RunString(`
	var buf = new ArrayBuffer(4);
	new Uint8Array(buf).set([1, 2, 3, 4]);
	buf;
	`)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := extractBufferWrapper(t, v)

	// Simulate what a naive serializer would do: deep-copy the bytes.
	original := wrapper.buf.data
	clone := append([]byte(nil), original...)
	clone[0] = 99

	if wrapper.Detached() {
		t.Fatal("deep copy must not detach the source buffer")
	}
	if original[0] != 1 {
		t.Fatal("source storage was modified by writing to the clone")
	}
	if dataPtr(clone) == dataPtr(original) {
		t.Fatal("clone unexpectedly aliases source storage")
	}

	// The source JS buffer is still fully usable.
	ok, err := src.RunString(`new Uint8Array(buf)[0] === 1;`)
	if err != nil {
		t.Fatal(err)
	}
	if !ok.ToBoolean() {
		t.Fatal("source view not usable after deep copy")
	}

	// Contrast with a real transfer of the same buffer.
	dst := New()
	out, terr := TransferArrayBuffers(dst, src, []ArrayBuffer{wrapper}, ArrayBufferTransferOptions{})
	if terr != nil {
		t.Fatal(terr)
	}
	if !wrapper.Detached() {
		t.Fatal("real transfer must detach the source")
	}
	if dataPtr(out[0].buf.data) != dataPtr(original) {
		t.Fatal("real transfer must reuse source storage")
	}
}
