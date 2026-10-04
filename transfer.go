package goja

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// OwnershipMode declares how the backing storage of an ArrayBuffer is allowed to move
// between two isolated Runtimes.
type OwnershipMode int

const (
	// OwnershipHostManaged requires every source ArrayBuffer to have been created
	// through NewManagedBuffer (or adopted with ManagedBuffer.Adopt). Such buffers are
	// subject to the exclusive-holding contract: while the host holds an outstanding
	// pin (a Go []byte obtained from the buffer) the buffer cannot be transferred. On a
	// successful transfer the storage moves to the target Runtime exactly once, without
	// copying the bytes, and all source-side views become detached.
	OwnershipHostManaged OwnershipMode = iota

	// OwnershipEphemeral additionally permits plain ArrayBuffers created through
	// NewArrayBuffer or "new ArrayBuffer(...)". Such buffers are not covered by the
	// exclusive-holding contract: goja cannot invalidate []byte slices the host (or any
	// other Go code) obtained earlier via ArrayBuffer.Bytes, Export, or by keeping the
	// slice passed to NewArrayBuffer. The caller explicitly accepts that those pre-existing
	// aliases are not revoked and remain valid, independently writable Go memory.
	//
	// Even in this mode the ECMAScript side behaves as specified: the source ArrayBuffer
	// and every TypedArray/DataView that views it are detached.
	OwnershipEphemeral
)

// TransferOptions controls a TransferArrayBuffers call.
type TransferOptions struct {
	// Mode selects the permitted ownership contract (OwnershipHostManaged by default).
	Mode OwnershipMode
}

// Sentinel errors returned by TransferArrayBuffers. They are wrapped with contextual
// information, use errors.Is to match them.
var (
	// ErrInvalidSource is returned when a source entry is not an ECMAScript ArrayBuffer
	// belonging to the source Runtime.
	ErrInvalidSource = errors.New("goja/transfer: entry is not an ArrayBuffer of the source Runtime")
	// ErrDuplicateSource is returned when the same ArrayBuffer appears more than once.
	ErrDuplicateSource = errors.New("goja/transfer: duplicate ArrayBuffer in source list")
	// ErrDetachedSource is returned when a source ArrayBuffer is already detached.
	ErrDetachedSource = errors.New("goja/transfer: source ArrayBuffer is already detached")
	// ErrHostPinned is returned when a managed buffer cannot be transferred because the
	// host still holds an exclusive pin to its backing storage.
	ErrHostPinned = errors.New("goja/transfer: ArrayBuffer is pinned by the host and cannot be transferred")
	// ErrSharedMemory is returned when a buffer represents shared memory
	// (SharedArrayBuffer). goja does not implement shared memory, and transferring shared
	// storage between isolated Runtimes is not supported.
	ErrSharedMemory = errors.New("goja/transfer: SharedArrayBuffer / shared memory is not supported")
	// ErrSameRuntime is returned when source and target are the same Runtime. Transfer is
	// defined between two isolated Runtimes; use ArrayBuffer.prototype.transfer within a
	// single Runtime instead.
	ErrSameRuntime = errors.New("goja/transfer: source and target Runtime must be different")
)

// transferMu serializes ownership moves across all Runtimes. Combined with the per
// Runtime execution guard it guarantees that a validated batch commits as a whole
// without racing JavaScript execution or another transfer.
var transferMu sync.Mutex

// managedRegistry tracks every buffer created through the managed entry points. It is
// keyed by the immutable internal object pointer, which stays stable for the lifetime of
// the ArrayBuffer (including after detach).
var managedRegistry = struct {
	sync.Mutex
	buffers map[*arrayBufferObject]*ManagedBuffer
}{buffers: make(map[*arrayBufferObject]*ManagedBuffer)}

// ManagedBuffer is the exclusive-holding entry point for binary storage that is intended
// to be transferred between isolated Runtimes without copying.
//
// A ManagedBuffer owns exactly one backing allocation. Ownership of that allocation can
// move at most once per Runtime boundary via TransferArrayBuffers. Go access to the bytes
// is mediated by Pin/Unpin: every outstanding pin represents an exclusive Go lease on the
// storage and blocks transfer until it is released.
//
// Important limitation: Pin/Unpin is a cooperative contract for access obtained through
// this API. goja cannot reach into arbitrary Go code and invalidate []byte aliases it
// never knew about. It specifically cannot revoke:
//   - a slice the caller kept after passing it to NewArrayBuffer, or
//   - bytes obtained through ArrayBuffer.Bytes / Value.Export on non-managed buffers, or
//   - bytes copied out before a pin was taken.
//
// Use NewManagedBuffer and only Pin-based access whenever revocation guarantees matter.
type ManagedBuffer struct {
	runtime *Runtime

	mu       sync.Mutex
	current  *arrayBufferObject // live object in current Runtime (nil after transfer)
	pins     int                // outstanding host leases
	data     []byte             // the single backing allocation; never copied on transfer
	transfer uint64             // number of times ownership moved
}

// NewManagedBuffer allocates length zero-initialized bytes owned by r and returns both
// the host handle and the ECMAScript ArrayBuffer. The returned ManagedBuffer is the only
// supported way to obtain revocable Go access (Pin) to the storage.
func NewManagedBuffer(r *Runtime, length int) (*ManagedBuffer, ArrayBuffer) {
	if length < 0 {
		panic(r.NewTypeError("Invalid ArrayBuffer length: %d", length))
	}
	data := make([]byte, length)
	return adoptManaged(r, data)
}

// AdoptManagedBuffer takes ownership of data for use as a managed ArrayBuffer. The caller
// MUST give up all other references to data (and its backing array) after adoption;
// retaining an alias circumvents the exclusive-holding contract and is a caller error.
func AdoptManagedBuffer(r *Runtime, data []byte) (*ManagedBuffer, ArrayBuffer) {
	return adoptManaged(r, data)
}

func adoptManaged(r *Runtime, data []byte) (*ManagedBuffer, ArrayBuffer) {
	buf := r._newArrayBuffer(r.getArrayBufferPrototype(), nil)
	buf.data = data
	mb := &ManagedBuffer{
		runtime: r,
		current: buf,
		data:    data,
	}
	managedRegistry.Lock()
	managedRegistry.buffers[buf] = mb
	managedRegistry.Unlock()
	return mb, ArrayBuffer{buf: buf}
}

// Pin grants an exclusive host lease over the backing bytes and returns the slice. While
// pinned, TransferArrayBuffers rejects the buffer with ErrHostPinned. Pin must be paired
// with Unpin exactly once. The returned slice MUST NOT be used after Unpin or after the
// buffer is transferred while unpinned.
//
// Pin returns nil if the buffer has already been transferred away from its current
// Runtime (its storage now belongs to the target side).
func (m *ManagedBuffer) Pin() []byte {
	// Serialize against an in-flight commit: a transfer holds transferMu for its whole
	// validate+commit window, so a pin can never appear half-way through a move.
	transferMu.Lock()
	defer transferMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil || m.current.detached {
		return nil
	}
	m.pins++
	return m.data
}

// Unpin releases a lease previously granted by Pin.
func (m *ManagedBuffer) Unpin() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pins == 0 {
		panic("goja/transfer: Unpin called without a matching Pin")
	}
	m.pins--
}

// Pinned reports whether the host currently holds at least one exclusive lease.
func (m *ManagedBuffer) Pinned() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pins > 0
}

// TransferCount reports how many times the backing storage moved between Runtimes.
func (m *ManagedBuffer) TransferCount() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transfer
}

// BackingPointer returns the address of the first byte of the single backing allocation.
// It is intended for tests and diagnostics that need to prove no copy occurred: the
// pointer is identical before and after a transfer. It returns 0 for a zero-length
// buffer.
func (m *ManagedBuffer) BackingPointer() uintptr {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.data) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(unsafe.SliceData(m.data)))
}

type validatedTransfer struct {
	mb     *ManagedBuffer // nil for OwnershipEphemeral non-managed buffers
	source *arrayBufferObject
	data   []byte
}

// TransferArrayBuffers moves ownership of a batch of ArrayBuffers from src to dst without
// copying their bytes.
//
// The move is atomic with respect to validation: every entry is checked first (source
// Runtime membership, duplicates, detached state, shared memory, busy Runtimes and the
// ownership mode), and only if all checks pass does any state change. If it returns an
// error, none of the source buffers are detached and dst receives nothing.
//
// On success each source ArrayBuffer and every TypedArray/DataView that views it exhibits
// standard ECMAScript detached behaviour (zero length/byteLength/byteOffset, TypeError on
// indexed access), while the returned ArrayBuffers in dst are backed by the exact same
// allocations — each underlying storage has exactly one owner afterwards.
//
// src and dst must be distinct Runtimes, neither may be executing on another goroutine,
// and shared memory is rejected. See OwnershipMode for the guarantees provided to Go-side
// []byte aliases.
func TransferArrayBuffers(dst *Runtime, src *Runtime, sources []ArrayBuffer, opts ...TransferOptions) ([]ArrayBuffer, error) {
	if dst == nil || src == nil {
		return nil, errors.New("goja/transfer: nil Runtime")
	}
	mode := OwnershipHostManaged
	if len(opts) > 0 {
		mode = opts[0].Mode
	}
	if mode < OwnershipHostManaged || mode > OwnershipEphemeral {
		return nil, fmt.Errorf("goja/transfer: unsupported ownership mode %d", mode)
	}

	if dst == src {
		return nil, ErrSameRuntime
	}

	// Serialize ownership moves globally. The busy checks are taken while this lock is
	// held, which (together with the per-Runtime execution guard) rejects the unsupported
	// combination of either Runtime executing JavaScript on another goroutine. A call made
	// re-entrantly from a Runtime's own host callback on the same goroutine is allowed.
	transferMu.Lock()
	defer transferMu.Unlock()

	if busy, owned, unlock := src.executionState(); busy && !owned {
		unlock()
		return nil, fmt.Errorf("%w: source", ErrRuntimeBusy)
	} else {
		defer unlock()
	}
	if busy, owned, unlock := dst.executionState(); busy && !owned {
		unlock()
		return nil, fmt.Errorf("%w: target", ErrRuntimeBusy)
	} else {
		defer unlock()
	}

	entries := make([]validatedTransfer, 0, len(sources))
	seen := make(map[*arrayBufferObject]int, len(sources))

	for i, a := range sources {
		buf, err := asSourceArrayBuffer(src, a)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		if _, dup := seen[buf]; dup {
			return nil, fmt.Errorf("entry %d: %w (first at entry %d)", i, ErrDuplicateSource, seen[buf])
		}
		seen[buf] = i
		entries = append(entries, validatedTransfer{source: buf, data: buf.data})
	}

	// Look up managed handles and verify the exclusive-holding contract. This is a second
	// pass because registry membership is independent of object identity.
	managedRegistry.Lock()
	for i := range entries {
		buf := entries[i].source
		mb, managed := managedRegistry.buffers[buf]
		if !managed {
			if mode == OwnershipHostManaged {
				managedRegistry.Unlock()
				return nil, fmt.Errorf("entry %d: %w", i, ErrInvalidSource)
			}
			continue
		}
		mb.mu.Lock()
		pinned := mb.pins > 0
		stale := mb.current != buf
		mb.mu.Unlock()
		if pinned {
			managedRegistry.Unlock()
			return nil, fmt.Errorf("entry %d: %w", i, ErrHostPinned)
		}
		// Sanity: a registered buffer must still be the live object for its handle.
		if stale {
			managedRegistry.Unlock()
			return nil, fmt.Errorf("entry %d: %w (buffer already moved)", i, ErrDetachedSource)
		}
		entries[i].mb = mb
	}
	managedRegistry.Unlock()

	// --- All validation passed. Commit phase. No operation below fails. ---

	result := make([]ArrayBuffer, 0, len(entries))
	for _, e := range entries {
		newBuf := dst._newArrayBuffer(dst.getArrayBufferPrototype(), nil)
		newBuf.data = e.data // adopt the exact backing allocation; bytes are not copied

		if e.mb != nil {
			// Move managed ownership: detach source identity, publish new identity. The
			// registry swap and detach happen while both the transfer lock and the
			// handle lock are held.
			managedRegistry.Lock()
			delete(managedRegistry.buffers, e.source)
			managedRegistry.buffers[newBuf] = e.mb
			managedRegistry.Unlock()

			e.mb.mu.Lock()
			e.mb.runtime = dst
			e.mb.current = newBuf
			e.mb.transfer++
			e.mb.mu.Unlock()
		}

		e.source.detach()
		result = append(result, ArrayBuffer{buf: newBuf})
	}

	return result, nil
}

// asSourceArrayBuffer validates that a is an ArrayBuffer owned by src and returns its
// internal object. Shared memory cannot be represented in goja (there is no
// SharedArrayBuffer), so such objects can never reach this point.
func asSourceArrayBuffer(src *Runtime, a ArrayBuffer) (*arrayBufferObject, error) {
	buf := a.buf
	if buf == nil || buf.val == nil || buf.val.runtime != src {
		return nil, ErrInvalidSource
	}
	if buf.detached {
		return nil, ErrDetachedSource
	}
	return buf, nil
}
