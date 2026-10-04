package goja

import (
	"bytes"
	"errors"
	"runtime"
	"strconv"
)

// ErrRuntimeBusy is returned when an entry point that requires exclusive access to a
// Runtime is invoked while that Runtime is already executing JavaScript on another
// goroutine. A goja Runtime never supports concurrent execution: only one goroutine
// may run code (or host calls that mutate the VM) at any point in time.
var ErrRuntimeBusy = errors.New("goja: the Runtime is already executing on another goroutine")

// currentGoroutineID returns the numeric id of the calling goroutine. It is used solely
// to distinguish re-entrant calls from the owning goroutine from foreign-goroutine
// calls; it is never used for scheduling.
func currentGoroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// Expect: "goroutine 123 [..."
	line := bytes.TrimPrefix(buf[:n], []byte("goroutine "))
	sp := bytes.IndexByte(line, ' ')
	if sp < 0 {
		return 0
	}
	id, err := strconv.ParseUint(string(line[:sp]), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// tryEnterExecution attempts to claim the Runtime for top-level JavaScript execution.
// It allows nested entry from the goroutine that already owns the Runtime (for example
// RunProgram invoked from inside a native Go function) and rejects a claim from any
// other goroutine while the Runtime is busy.
func (r *Runtime) tryEnterExecution() bool {
	gid := currentGoroutineID()
	r.execMu.Lock()
	defer r.execMu.Unlock()
	if r.execRunning && r.execOwner != gid {
		return false
	}
	r.execRunning = true
	r.execOwner = gid
	r.execDepth++
	return true
}

func (r *Runtime) leaveExecution() {
	r.execMu.Lock()
	defer r.execMu.Unlock()
	if r.execDepth > 0 {
		r.execDepth--
	}
	if r.execDepth == 0 {
		r.execRunning = false
		r.execOwner = 0
	}
}

// executionState reports whether the Runtime currently runs JavaScript and whether the
// caller is the owning goroutine. It holds the execution mutex until unlock is called so
// the state cannot change between the observation and the guarded action.
func (r *Runtime) executionState() (busy, ownedByCaller bool, unlock func()) {
	gid := currentGoroutineID()
	r.execMu.Lock()
	busy = r.execRunning
	ownedByCaller = busy && r.execOwner == gid
	return busy, ownedByCaller, r.execMu.Unlock
}

// ownedByCurrentGoroutine reports whether the calling goroutine currently owns the
// Runtime's execution. It must be called without holding execMu across the check in a way
// that would deadlock re-entrancy; the lock is taken and released immediately.
func (r *Runtime) ownedByCurrentGoroutine() bool {
	gid := currentGoroutineID()
	r.execMu.Lock()
	owned := r.execRunning && r.execOwner == gid
	r.execMu.Unlock()
	return owned
}
