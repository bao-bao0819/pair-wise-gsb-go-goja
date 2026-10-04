package goja

import (
	"errors"
	"io"
)

// Errors returned by the managed-buffer entry points.
var (
	// ErrManagedBytesConsumed is returned when a ManagedBytes is handed to an
	// ArrayBuffer constructor after its storage has already been taken.
	ErrManagedBytesConsumed = errors.New("ManagedBytes has already been consumed")

	// ErrManagedBytesInvalid is returned for invalid parameters such as a
	// negative size or a nil fill function.
	ErrManagedBytesInvalid = errors.New("invalid ManagedBytes parameters")
)

// ManagedBytes is the exclusive-ownership entry for large binary payloads that
// are to be transferred between isolated Runtimes.
//
// The contract is deliberately narrower than passing a []byte to
// Runtime.NewArrayBuffer:
//
//   - The storage is produced specifically for one ArrayBuffer. There is no
//     caller-held []byte before construction, so there is no pre-existing alias
//     that transfer would have to invalidate.
//   - take() can only succeed once; afterwards the ManagedBytes is consumed and
//     cannot be used to build a second buffer over the same bytes.
//   - The fill callback receives the destination slice to write into. It must
//     not retain that slice after returning: the whole point of this entry
//     point is that once the buffer exists, the engine is the single owner.
//
// A buffer created from ManagedBytes has OwnershipExclusive until the host
// takes its bytes (ArrayBuffer.Bytes or an export into []byte), at which point
// an alias has escaped and it behaves like an OwnershipExternal buffer.
type ManagedBytes struct {
	state *managedBytesState
}

type managedBytesState struct {
	data  []byte
	taken bool
}

// NewManagedBytes allocates size bytes that will be owned exclusively by the
// ArrayBuffer constructed from the returned value. The bytes start zeroed.
// Filling them without ever exposing the slice to a retained alias is the
// caller's responsibility; if the caller needs a callback for that use
// NewManagedBytesFunc.
func NewManagedBytes(size int) ManagedBytes {
	var data []byte
	if size > 0 {
		data = make([]byte, size)
	}
	return ManagedBytes{state: &managedBytesState{data: data}}
}

// NewManagedBytesFunc produces exactly size bytes through fill, without a
// second allocation: fill writes directly into the storage that the future
// ArrayBuffer will own. size must be non-negative and fill must be non-nil
// when size > 0 (fill may be nil for size == 0).
func NewManagedBytesFunc(size int, fill func(dst []byte) error) (ManagedBytes, error) {
	if size < 0 {
		return ManagedBytes{}, ErrManagedBytesInvalid
	}
	if size > 0 && fill == nil {
		return ManagedBytes{}, ErrManagedBytesInvalid
	}
	mb := NewManagedBytes(size)
	if size > 0 {
		if err := fill(mb.state.data); err != nil {
			return ManagedBytes{}, err
		}
	}
	return mb, nil
}

// NewManagedBytesFromReader reads exactly size bytes from r into exclusively
// owned storage (using io.ReadFull semantics). It is the streaming analogue of
// NewManagedBytesFunc and performs no intermediate copy.
func NewManagedBytesFromReader(r io.Reader, size int) (ManagedBytes, error) {
	if size < 0 || r == nil {
		return ManagedBytes{}, ErrManagedBytesInvalid
	}
	mb := NewManagedBytes(size)
	if size > 0 {
		if _, err := io.ReadFull(r, mb.state.data); err != nil {
			return ManagedBytes{}, err
		}
	}
	return mb, nil
}

// Len reports the number of bytes that will be handed over.
func (m *ManagedBytes) Len() int {
	if m.state == nil {
		return 0
	}
	return len(m.state.data)
}

// Consumed reports whether take has already run.
func (m ManagedBytes) Consumed() bool {
	return m.state != nil && m.state.taken
}

// take returns the storage and marks the ManagedBytes consumed. It may only
// succeed once so that two ArrayBuffers can never alias the same managed
// storage.
func (m ManagedBytes) take() ([]byte, error) {
	if m.state == nil {
		return nil, ErrManagedBytesInvalid
	}
	if m.state.taken {
		return nil, ErrManagedBytesConsumed
	}
	m.state.taken = true
	return m.state.data, nil
}
