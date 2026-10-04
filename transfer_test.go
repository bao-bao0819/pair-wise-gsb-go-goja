package goja

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

func mustRun(t *testing.T, r *Runtime, src string) Value {
	t.Helper()
	v, err := r.RunString(src)
	if err != nil {
		t.Fatalf("script error: %v", err)
	}
	return v
}

func evalInt(t *testing.T, r *Runtime, src string) int64 {
	t.Helper()
	return mustRun(t, r, src).ToInteger()
}

func backingPtr(b []byte) uintptr {
	if len(b) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))
}

// buildOverlapping creates a managed 32-byte buffer in r and several overlapping views,
// filling it with recognisable markers.
func buildOverlapping(t *testing.T, r *Runtime) (*ManagedBuffer, ArrayBuffer) {
	t.Helper()
	mb, ab := NewManagedBuffer(r, 32)
	r.Set("buf", r.ToValue(ab))
	mustRun(t, r, `
		var u8  = new Uint8Array(buf);
		var u32 = new Uint32Array(buf, 4, 4); // overlaps u8[4..19]
		var dv  = new DataView(buf, 8, 16);    // overlaps both
		var u8tail = new Uint8Array(buf, 28, 4);
		for (var i = 0; i < 32; i++) u8[i] = i + 1;
		u32[0] = 0xDEADBEEF;
		dv.setUint32(0, 0x01020304);
	`)
	return mb, ab
}

func assertDetachedViews(t *testing.T, r *Runtime) {
	t.Helper()
	checks := map[string]int64{
		"buf.byteLength":            0,
		"u8.length":                 0,
		"u8.byteLength":             0,
		"u32.length":                0,
		"u32.byteLength":            0,
		"u8tail.length":             0,
		"(u8[5] === undefined?1:0)": 1,
	}
	for expr, want := range checks {
		if got := evalInt(t, r, expr); got != want {
			t.Errorf("%s: want %d, got %d", expr, want, got)
		}
	}
	// DataView getters and element access reject a detached buffer.
	for _, expr := range []string{`dv.byteLength`, `dv.byteOffset`, `dv.getInt32(0)`} {
		if _, err := r.RunString(expr); err == nil {
			t.Errorf("%s on detached DataView must throw", expr)
		} else if !strings.Contains(err.Error(), "detached") {
			t.Errorf("%s: expected a detached TypeError, got %v", expr, err)
		}
	}
}

// TestTransferOverlappingViewsDetach is the primary sample: one buffer viewed by several
// overlapping TypedArrays/DataViews is transferred; all views must show detached behaviour,
// while the target keeps the original bytes in the same allocation.
func TestTransferOverlappingViewsDetach(t *testing.T) {
	src := New()
	dst := New()
	mb, ab := buildOverlapping(t, src)

	ptrBefore := mb.BackingPointer()
	if ptrBefore == 0 {
		t.Fatal("expected non-zero backing pointer")
	}

	out, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab})
	if err != nil {
		t.Fatalf("transfer failed: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 output, got %d", len(out))
	}

	// Zero-copy evidence: the managed handle reports the identical backing address.
	if ptr := mb.BackingPointer(); ptr != ptrBefore {
		t.Fatalf("backing storage was copied: before=%x after=%x", ptrBefore, ptr)
	}
	if backingPtr(out[0].Bytes()) != ptrBefore {
		t.Fatalf("target backing pointer differs from source (bytes were copied)")
	}
	if mb.TransferCount() != 1 {
		t.Fatalf("want transfer count 1, got %d", mb.TransferCount())
	}

	if !ab.Detached() || ab.Bytes() != nil {
		t.Fatal("source ArrayBuffer must be detached with nil Bytes()")
	}
	assertDetachedViews(t, src)

	// Target retains original bytes in the same storage.
	dst.Set("tb", dst.ToValue(out[0]))
	if got := evalInt(t, dst, `new Uint32Array(tb, 4, 1)[0]`); got != int64(0xDEADBEEF) {
		t.Errorf("u32 marker mismatch: %x", got)
	}
	if got := evalInt(t, dst, `new DataView(tb,8,4).getUint32(0)`); got != int64(0x01020304) {
		t.Errorf("dv marker mismatch: %x", got)
	}
	if got := evalInt(t, dst, `new Uint8Array(tb,31,1)[0]`); got != 32 {
		t.Errorf("byte marker mismatch: %d", got)
	}
}

// TestTransferAtomicity puts an illegal entry at the end of a valid batch and asserts the
// whole batch is rejected with no source buffer invalidated.
func TestTransferAtomicity(t *testing.T) {
	src := New()
	dst := New()

	good1, ab1 := buildOverlapping(t, src)

	mb2, ab2 := NewManagedBuffer(src, 8)
	if p := mb2.Pin(); p != nil {
		for i := range p {
			p[i] = 9
		}
		p[0] = 7
		mb2.Unpin()
	}

	// Third entry: a buffer from a *foreign* runtime, placed last.
	foreign := New()
	_, foreignAB := NewManagedBuffer(foreign, 4)

	_, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab1, ab2, foreignAB})
	if err == nil {
		t.Fatal("expected error for foreign-source entry")
	}
	if !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("want ErrInvalidSource, got %v", err)
	}
	if !strings.Contains(err.Error(), "entry 2") {
		t.Errorf("error should identify the failing index, got %v", err)
	}

	// None of the valid sources were detached or lost their bytes.
	if ab1.Detached() || ab2.Detached() {
		t.Fatal("atomicity violated: a valid source was detached despite failed batch")
	}
	if good1.BackingPointer() == 0 || mb2.BackingPointer() == 0 {
		t.Fatal("valid buffers lost their backing storage after failed batch")
	}
	// Views on the first buffer still operate.
	if got := evalInt(t, src, `new Uint32Array(buf,4,1)[0]`); got != int64(0xDEADBEEF) {
		t.Errorf("source bytes changed after failed batch: %x", got)
	}
	if mb2.TransferCount() != 0 {
		t.Errorf("transfer count must stay 0 on failure, got %d", mb2.TransferCount())
	}

	// A fresh retry without the bad entry succeeds.
	out, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab1, ab2})
	if err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if len(out) != 2 || !ab1.Detached() || !ab2.Detached() {
		t.Fatal("retry did not transfer and detach both buffers")
	}
}

// TestTransferDuplicates rejects the same buffer appearing twice.
func TestTransferDuplicates(t *testing.T) {
	src := New()
	dst := New()
	_, ab := NewManagedBuffer(src, 4)

	_, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab, ab})
	if !errors.Is(err, ErrDuplicateSource) {
		t.Fatalf("want ErrDuplicateSource, got %v", err)
	}
	if ab.Detached() {
		t.Fatal("source detached on duplicate rejection")
	}
}

// TestTransferAlreadyDetached rejects a detached entry.
func TestTransferAlreadyDetached(t *testing.T) {
	src := New()
	dst := New()
	_, ab := NewManagedBuffer(src, 4)
	if !ab.Detach() {
		t.Fatal("detach should succeed")
	}
	_, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab})
	if !errors.Is(err, ErrDetachedSource) {
		t.Fatalf("want ErrDetachedSource, got %v", err)
	}
}

// TestTransferSameRuntime rejects source == target.
func TestTransferSameRuntime(t *testing.T) {
	r := New()
	_, ab := NewManagedBuffer(r, 4)
	if _, err := TransferArrayBuffers(r, r, []ArrayBuffer{ab}); !errors.Is(err, ErrSameRuntime) {
		t.Fatalf("want ErrSameRuntime, got %v", err)
	}
	if ab.Detached() {
		t.Fatal("buffer detached on same-runtime rejection")
	}
}

// TestManagedModeRejectsUnmanaged: in the default mode plain NewArrayBuffer buffers are
// rejected because the host cannot be given an exclusive-holding guarantee for them.
func TestManagedModeRejectsUnmanaged(t *testing.T) {
	src := New()
	dst := New()
	data := make([]byte, 4)
	ab := src.NewArrayBuffer(data)

	if _, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab}); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("managed mode must reject an unmanaged buffer, got %v", err)
	}
	if ab.Detached() {
		t.Fatal("unmanaged buffer detached despite rejection")
	}
}

// TestEphemeralAllowsUnmanaged documents the looser mode: the transfer is still a real
// ownership move on the ECMAScript side (detach + same bytes), but a Go []byte the host
// kept beforehand is NOT revoked — this is the documented limitation, not a silent copy.
func TestEphemeralAllowsUnmanaged(t *testing.T) {
	src := New()
	dst := New()
	data := make([]byte, 16)
	for i := range data {
		data[i] = byte(i + 1)
	}
	ab := src.NewArrayBuffer(data)

	out, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab}, TransferOptions{Mode: OwnershipEphemeral})
	if err != nil {
		t.Fatalf("ephemeral transfer failed: %v", err)
	}
	if !ab.Detached() {
		t.Fatal("source must detach even in ephemeral mode")
	}
	// Zero-copy: target views the exact array the host allocated.
	if backingPtr(out[0].Bytes()) != backingPtr(data) {
		t.Fatal("ephemeral transfer copied the bytes")
	}
	// The pre-existing host alias still references the same memory and is *not* revoked.
	// This is precisely the caveat OwnershipEphemeral documents.
	data[0] = 0x5A
	dst.Set("tb", dst.ToValue(out[0]))
	if got := evalInt(t, dst, `new Uint8Array(tb)[0]`); got != 0x5A {
		t.Fatalf("host alias and target no longer share storage: %x", got)
	}
}

// TestManagedPinBlocksTransfer enforces the exclusive lease: an outstanding Pin rejects
// the transfer atomically, and after Unpin the transfer goes through.
func TestManagedPinBlocksTransfer(t *testing.T) {
	src := New()
	dst := New()
	mb, ab := NewManagedBuffer(src, 8)

	lease := mb.Pin()
	if lease == nil {
		t.Fatal("pin failed on live buffer")
	}
	if !mb.Pinned() {
		t.Fatal("Pinned() should report true")
	}

	if _, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab}); !errors.Is(err, ErrHostPinned) {
		t.Fatalf("want ErrHostPinned, got %v", err)
	}
	if ab.Detached() {
		t.Fatal("buffer detached while pinned")
	}

	mb.Unpin()
	if mb.Pinned() {
		t.Fatal("Pinned() should be false after Unpin")
	}
	out, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab})
	if err != nil {
		t.Fatalf("transfer after unpin failed: %v", err)
	}
	if !ab.Detached() || backingPtr(out[0].Bytes()) != mb.BackingPointer() {
		t.Fatal("post-unpin transfer did not move the same storage")
	}

	// After moving away, the handle leases on the target side; a pin there returns the
	// moved bytes and pinning blocks a further transfer back.
	lease2 := mb.Pin()
	if backingPtr(lease2) != mb.BackingPointer() {
		t.Fatal("post-transfer pin does not reference moved storage")
	}
	if _, err := TransferArrayBuffers(src, dst, []ArrayBuffer{out[0]}); !errors.Is(err, ErrHostPinned) {
		t.Fatalf("second transfer must be blocked while pinned, got %v", err)
	}
	mb.Unpin()
}

// TestManagedPinUnbalancedPanics documents the pairing contract.
func TestManagedPinUnbalancedUnpin(t *testing.T) {
	r := New()
	mb, _ := NewManagedBuffer(r, 1)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Unpin without Pin must panic")
			}
		}()
		mb.Unpin()
	}()
}

// TestTransferRejectsWrongSourceRuntime ensures a buffer from a third runtime cannot be
// smuggled through an unrelated src runtime.
func TestTransferRejectsWrongSourceRuntime(t *testing.T) {
	src := New()
	dst := New()
	other := New()
	_, ab := NewManagedBuffer(other, 4)
	_, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab})
	if !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("want ErrInvalidSource for foreign buffer, got %v", err)
	}
}

// TestTransferRejectsConcurrentExecution rejects the combination that goja does not
// support: a Runtime executing JavaScript on another goroutine at transfer time.
func TestTransferRejectsConcurrentExecution(t *testing.T) {
	src := New()
	dst := New()
	_, ab := NewManagedBuffer(src, 4)

	entered := make(chan struct{})
	release := make(chan struct{})
	src.Set("__hold", func(FunctionCall) Value {
		close(entered)
		<-release
		return _undefined
	})
	go func() {
		_, _ = src.RunString(`__hold()`)
	}()

	<-entered
	done := make(chan error, 1)
	go func() {
		_, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrRuntimeBusy) {
			t.Fatalf("want ErrRuntimeBusy, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transfer blocked instead of rejecting a busy source Runtime")
	}

	if ab.Detached() {
		t.Fatal("buffer detached despite rejected concurrent transfer")
	}
	close(release)
}

// TestRunProgramRejectsConcurrentGoroutine checks the underlying execution guard itself.
func TestRunProgramRejectsConcurrentGoroutine(t *testing.T) {
	r := New()
	entered := make(chan struct{})
	release := make(chan struct{})
	r.Set("__hold", func(FunctionCall) Value {
		close(entered)
		<-release
		return _undefined
	})
	go func() {
		_, _ = r.RunString(`__hold()`)
	}()
	<-entered

	errCh := make(chan error, 1)
	go func() {
		_, err := r.RunString(`1+1`)
		errCh <- err
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrRuntimeBusy) {
			t.Fatalf("want ErrRuntimeBusy, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent RunProgram blocked instead of returning ErrRuntimeBusy")
	}
	close(release)
}

// TestTransferReentrantFromHostCallback allows the legitimate re-entrant case: a native
// Go function running inside the source Runtime's own goroutine hands a buffer to another
// (idle) Runtime.
func TestTransferReentrantFromHostCallback(t *testing.T) {
	src := New()
	dst := New()
	mb, ab := NewManagedBuffer(src, 16)
	base := mb.BackingPointer()
	src.Set("buf", src.ToValue(ab))

	var transferred ArrayBuffer
	var transferErr error
	src.Set("handOff", func(FunctionCall) Value {
		out, err := TransferArrayBuffers(dst, src, []ArrayBuffer{ab})
		if err == nil {
			transferred = out[0]
		}
		transferErr = err
		return _undefined
	})

	if _, err := src.RunString(`
		var v = new Uint8Array(buf);
		v[3] = 77;
		handOff();
	`); err != nil {
		t.Fatalf("script error: %v", err)
	}
	if transferErr != nil {
		t.Fatalf("re-entrant transfer rejected: %v", transferErr)
	}
	if !ab.Detached() {
		t.Fatal("source not detached after re-entrant transfer")
	}
	if backingPtr(transferred.Bytes()) != base {
		t.Fatal("re-entrant transfer copied the bytes")
	}
	if transferred.Bytes()[3] != 77 {
		t.Fatalf("byte not preserved: %d", transferred.Bytes()[3])
	}
}

// TestTransferChainsAcrossRuntimes proves storage can hop runtimes exactly once per call,
// always retaining the same allocation and a single owner.
func TestTransferChainsAcrossRuntimes(t *testing.T) {
	r1 := New()
	r2 := New()
	r3 := New()
	mb, ab := NewManagedBuffer(r1, 64)

	out, err := TransferArrayBuffers(r2, r1, []ArrayBuffer{ab})
	if err != nil {
		t.Fatal(err)
	}
	base := mb.BackingPointer()
	out, err = TransferArrayBuffers(r3, r2, out)
	if err != nil {
		t.Fatal(err)
	}
	if mb.TransferCount() != 2 || backingPtr(out[0].Bytes()) != base {
		t.Fatalf("chain broke single storage: count=%d ptr=%x base=%x",
			mb.TransferCount(), backingPtr(out[0].Bytes()), base)
	}
	// A second transfer from the original runtime identity is rejected: it is detached.
	if _, err := TransferArrayBuffers(r2, r1, []ArrayBuffer{ab}); !errors.Is(err, ErrDetachedSource) {
		t.Fatalf("want ErrDetachedSource, got %v", err)
	}
}

// TestTransferBatchPreservesOrder verifies positional correspondence.
func TestTransferBatchPreservesOrder(t *testing.T) {
	src := New()
	dst := New()
	var abs []ArrayBuffer
	var ptrs []uintptr
	for i := 0; i < 4; i++ {
		mb, ab := NewManagedBuffer(src, 8)
		p := mb.Pin()
		p[0] = byte(0xA0 + i)
		mb.Unpin()
		abs = append(abs, ab)
		ptrs = append(ptrs, mb.BackingPointer())
	}
	out, err := TransferArrayBuffers(dst, src, abs)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("want 4, got %d", len(out))
	}
	for i, o := range out {
		if backingPtr(o.Bytes()) != ptrs[i] {
			t.Errorf("entry %d pointer mismatch (copied)", i)
		}
		if o.Bytes()[0] != byte(0xA0+i) {
			t.Errorf("entry %d content mismatch", i)
		}
	}
}

// TestTransferNoImplicitSharedMemory documents that goja has no SharedArrayBuffer; the
// rejected combination cannot even be constructed, and the sentinel remains part of the
// contract for any future shared representation.
func TestTransferNoImplicitSharedMemory(t *testing.T) {
	r := New()
	v, err := r.RunString(`typeof SharedArrayBuffer`)
	if err != nil {
		t.Fatal(err)
	}
	if v.String() != "undefined" {
		t.Skipf("SharedArrayBuffer is now defined (%q); explicit rejection test needed", v)
	}
	if ErrSharedMemory == nil {
		t.Fatal("ErrSharedMemory must remain a defined contract sentinel")
	}
	var _ = sync.Mutex{}
}
