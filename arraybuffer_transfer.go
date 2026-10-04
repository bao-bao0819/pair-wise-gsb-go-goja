package goja

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// Transfer semantics errors. All failures reported by TransferArrayBuffers
// wrap one of these sentinels; use errors.Is to classify them.
var (
	// ErrInvalidTransferArgument is returned when a source entry is not an
	// ArrayBuffer (e.g. nil, a TypedArray/DataView, or a value from another
	// type).
	ErrInvalidTransferArgument = errors.New("transfer argument must be an ArrayBuffer")

	// ErrArrayBufferDetached is returned when one of the source buffers is
	// already detached.
	ErrArrayBufferDetached = errors.New("cannot transfer a detached ArrayBuffer")

	// ErrDuplicateTransferEntry is returned when the same ArrayBuffer object
	// appears more than once in one transfer batch (including two
	// TypedArray/DataView views over the same underlying buffer).
	ErrDuplicateTransferEntry = errors.New("ArrayBuffer appears more than once in the transfer list")

	// ErrWrongSourceRuntime is returned when a source buffer does not belong to
	// src, or when src and dst denote the same Runtime. A buffer that belongs
	// to dst was never owned by the source and must not be "transferred back".
	ErrWrongSourceRuntime = errors.New("ArrayBuffer does not belong to the source Runtime")

	// ErrOwnershipNotAllowed is returned when a buffer's ownership mode is not
	// permitted by Options.AllowedOwnership, e.g. RequireExclusiveOwnership is
	// set but a host-owned (OwnershipExternal) buffer is in the batch.
	ErrOwnershipNotAllowed = errors.New("ArrayBuffer ownership mode is not allowed for this transfer")

	// ErrSharedArrayBuffer is returned when a source is shared memory
	// (SharedArrayBuffer). Shared memory cannot have a single exclusive owner,
	// so transferring it is refused outright.
	ErrSharedArrayBuffer = errors.New("SharedArrayBuffer cannot be transferred")

	// ErrRuntimeBusy is returned when either Runtime is currently executing
	// JavaScript on another goroutine, or another transfer is in flight.
	// Overlapping transfer and execution is rejected rather than serialized so
	// that callers can never observe a buffer halfway through a move.
	ErrRuntimeBusy = errors.New("Runtime is busy executing JavaScript or another transfer")
)

// ArrayBufferTransferOptions controls validation performed before any buffer is
// detached. The zero value permits both OwnershipExternal and
// OwnershipExclusive buffers; set RequireExclusiveOwnership to enforce the
// strict single-owner contract for the whole batch.
type ArrayBufferTransferOptions struct {
	// AllowedOwnership lists ownership modes that may appear in the batch.
	// Empty defaults to allowing both OwnershipExternal and OwnershipExclusive.
	AllowedOwnership []ArrayBufferOwnership

	// RequireExclusiveOwnership is a convenience for
	// AllowedOwnership: []ArrayBufferOwnership{OwnershipExclusive}. When true
	// the transfer fails with ErrOwnershipNotAllowed if any source buffer was
	// created from host-supplied storage or has ever been exposed to Go.
	RequireExclusiveOwnership bool
}

// TransferArrayBuffers moves a batch of ArrayBuffers from the isolated source
// Runtime src to the isolated destination Runtime dst without copying the
// underlying bytes. It is a host-callable entry point intended for delivering
// large binary payloads between Runtimes.
//
// The move is atomic with respect to the source buffers: every entry is
// validated first (type, source Runtime, duplicates, detached state, ownership
// mode, shared memory, busy Runtimes) and only if the whole batch passes do the
// sources get detached. On failure no source buffer changes state.
//
// On success each returned ArrayBuffer belongs to dst, contains the exact
// original byte sequence, and is backed by the same allocation that the source
// used to be backed by (the slice header is relinked; bytes are not copied).
// The source ArrayBuffer objects and every TypedArray/DataView view over them
// behave exactly like ECMAScript detached buffers: their byteLength/length is 0,
// indexed access yields undefined, and method access throws TypeError.
//
// Ownership limitation (important): transferring cannot invalidate []byte
// slices that Go code already obtained from a buffer (NewArrayBuffer data,
// Bytes(), Export). Those aliases stay valid. For a strict single-owner
// contract create sources with NewManagedArrayBuffer (or allocate them inside
// the Runtime) and pass RequireExclusiveOwnership: true; then the only existing
// reference to the storage is the ECMAScript buffer itself and a successful
// transfer provably moves the sole owner.
//
// Concurrency: src and dst must not be executing JavaScript on another
// goroutine while the transfer happens; such a combination returns
// ErrRuntimeBusy. Shared-memory buffers are refused with ErrSharedArrayBuffer.
func TransferArrayBuffers(dst *Runtime, src *Runtime, buffers []ArrayBuffer, options ArrayBufferTransferOptions) ([]ArrayBuffer, error) {
	if dst == nil || src == nil {
		return nil, fmt.Errorf("%w: source and destination Runtimes must be non-nil", ErrInvalidTransferArgument)
	}
	if dst == src {
		return nil, fmt.Errorf("%w: source and destination must be different isolated Runtimes", ErrWrongSourceRuntime)
	}

	allowed := make(map[ArrayBufferOwnership]bool, len(options.AllowedOwnership)+1)
	if options.RequireExclusiveOwnership {
		allowed[OwnershipExclusive] = true
	} else if len(options.AllowedOwnership) == 0 {
		allowed[OwnershipExternal] = true
		allowed[OwnershipExclusive] = true
	} else {
		for _, mode := range options.AllowedOwnership {
			allowed[mode] = true
		}
	}

	// Lock both Runtimes for the whole validate+commit window. Acquisition is
	// ordered by Runtime pointer and non-blocking: a Runtime running script on
	// another goroutine, or concurrently taking part in another transfer,
	// causes ErrRuntimeBusy instead of blocking half-way.
	unlock, err := lockRuntimesForTransfer(src, dst)
	if err != nil {
		return nil, err
	}
	defer unlock()

	type transferEntry struct {
		srcObj *arrayBufferObject
	}
	entries := make([]transferEntry, len(buffers))
	seen := make(map[*arrayBufferObject]int, len(buffers))

	// Phase 1: validate everything. No state changes here, so any failure
	// leaves all sources intact.
	for i, ab := range buffers {
		if ab.buf == nil {
			return nil, fmt.Errorf("%w: entry %d is nil", ErrInvalidTransferArgument, i)
		}
		obj := ab.buf
		if obj.val == nil || obj.val.runtime == nil {
			return nil, fmt.Errorf("%w: entry %d", ErrInvalidTransferArgument, i)
		}
		if obj.val.runtime != src {
			if obj.val.runtime == dst {
				return nil, fmt.Errorf("%w: entry %d already belongs to the destination Runtime", ErrWrongSourceRuntime, i)
			}
			return nil, fmt.Errorf("%w: entry %d belongs to a different Runtime", ErrWrongSourceRuntime, i)
		}
		if prev, dup := seen[obj]; dup {
			return nil, fmt.Errorf("%w: entries %d and %d share the same underlying buffer", ErrDuplicateTransferEntry, prev, i)
		}
		seen[obj] = i
		if obj.detached {
			return nil, fmt.Errorf("%w: entry %d", ErrArrayBufferDetached, i)
		}
		if obj.shared {
			return nil, fmt.Errorf("%w: entry %d", ErrSharedArrayBuffer, i)
		}
		if !allowed[obj.ownership] {
			return nil, fmt.Errorf("%w: entry %d is %v", ErrOwnershipNotAllowed, i, obj.ownership)
		}
		entries[i] = transferEntry{srcObj: obj}
	}

	// Phase 2: commit. First relink every storage into the destination; only
	// after all destination buffers exist do we detach the sources. The two
	// loops keep the window in which a source is detached but its destination
	// is not yet visible to the host empty as far as the returned slice is
	// concerned, and ensure that allocation of the destination objects either
	// all succeeds (they cannot fail beyond OOM panic) or nothing happened.
	result := make([]ArrayBuffer, len(entries))
	data := make([][]byte, len(entries))
	for i, e := range entries {
		data[i] = e.srcObj.data
	}
	for i, e := range entries {
		newBuf := dst._newArrayBuffer(dst.getArrayBufferPrototype(), nil)
		newBuf.data = data[i]
		newBuf.ownership = e.srcObj.ownership
		result[i] = ArrayBuffer{buf: newBuf}
	}
	for _, e := range entries {
		e.srcObj.detach()
	}

	return result, nil
}

// vmActivityGate serializes JavaScript execution and buffer transfers on one
// Runtime. The mutex is taken non-blockingly by the outermost VM entries
// (RunProgram / runWrapped, which are reached only when the call stack is
// empty) and by TransferArrayBuffers. Re-entrant execution that occurs while
// the VM is already running (eval, Callable invoked from a native Go callback)
// has a non-empty call stack and never touches the gate, so execution is never
// serialized against itself; the lock is taken and released exactly once per
// top-level run.
type vmActivityGate struct {
	mu sync.Mutex
}

// enter is called by an outermost VM execution entry. It returns false if the
// Runtime is currently active for another purpose (a transfer, or concurrent
// execution attempted by another goroutine).
func (g *vmActivityGate) enter() bool {
	return g.mu.TryLock()
}

func (g *vmActivityGate) exit() {
	g.mu.Unlock()
}

// transferLockResult releases the gates acquired for a transfer.
type transferUnlock func()

// lockRuntimesForTransfer acquires both gates in pointer order with bounded
// retries. Both gates are non-blocking: if either Runtime executes JavaScript
// concurrently (or is locked by another transfer), ErrRuntimeBusy is returned
// instead of blocking, which would risk a partially locked transfer.
func lockRuntimesForTransfer(a, b *Runtime) (transferUnlock, error) {
	first, second := &a.vmGate, &b.vmGate
	if uintptr(unsafe.Pointer(first)) > uintptr(unsafe.Pointer(second)) {
		first, second = second, first
	}

	// Bounded spinning. Under real scheduling a contended TryLock clears
	// within a scheduling quantum; the cap converts a pathological livelock
	// (two transfers repeatedly stealing the ordered locks from each other)
	// into the explicit ErrRuntimeBusy rejection the contract requires.
	for attempt := 0; attempt < 100; attempt++ {
		if !first.mu.TryLock() {
			runtime.Gosched()
			continue
		}
		if !second.mu.TryLock() {
			first.mu.Unlock()
			runtime.Gosched()
			continue
		}
		return func() {
			second.mu.Unlock()
			first.mu.Unlock()
		}, nil
	}
	return nil, fmt.Errorf("%w: could not acquire both Runtime gates", ErrRuntimeBusy)
}
