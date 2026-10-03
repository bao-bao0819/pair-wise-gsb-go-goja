package goja

// SetManualJobPump enables or disables the manual promise job pumping mode.
//
// By default (manual == false) the Runtime automatically drains the promise
// reaction job queue every time a top level Run*()/callable invocation returns
// to Go. When manual pump mode is enabled the queue is NOT drained at that
// boundary: the top level script finishes immediately and the pending promise
// reactions stay queued. The caller then advances the queue explicitly, a
// bounded number of jobs at a time, using PumpJobs(). This allows an embedded
// rule service running a long promise chain to hand control back to Go in the
// middle of the chain and resume it later.
//
// Toggling the mode does not discard already queued jobs: enabling the manual
// mode retains the current queue for the next PumpJobs() call; disabling it
// lets the next top level boundary drain the remaining jobs automatically.
//
// This method may only be called when the Runtime is idle (not executing
// JavaScript and not inside a PumpJobs() call). As with the rest of the
// Runtime API it is not safe to call from multiple goroutines: a Runtime can
// only ever be operated on by a single goroutine at a time.
func (r *Runtime) SetManualJobPump(manual bool) {
	if len(r.vm.callStack) > 0 || r.jobPumpActive {
		panic("goja: SetManualJobPump cannot be called while the Runtime is running")
	}
	r.manualJobPump = manual
}

// PendingJobCount returns the number of promise jobs currently queued. The
// count is only meaningful while the Runtime is idle.
func (r *Runtime) PendingJobCount() int {
	return len(r.jobQueue)
}

// DiscardPendingJobs removes all currently queued promise jobs without
// executing them. It is the explicit way to abandon pending work.
//
// Jobs that were already executed are not affected, and their side effects on
// promise state are kept. Pending reactions that are discarded never run, so a
// promise may remain pending forever; the caller takes responsibility for that.
//
// This method may only be called while the Runtime is idle and it is not
// goroutine-safe (see SetManualJobPump).
func (r *Runtime) DiscardPendingJobs() {
	if len(r.vm.callStack) > 0 || r.jobPumpActive {
		panic("goja: DiscardPendingJobs cannot be called while the Runtime is running")
	}
	r.jobQueue = nil
}

// PumpJobs executes at most maxJobs queued promise reaction jobs in FIFO order
// and returns control to the caller. Jobs appended by an executed job are
// placed at the end of the queue and are only reached if the budget allows.
//
// The returned values are:
//   - executed: number of jobs actually executed in this round;
//   - remaining: true if jobs are still queued when the round returns (the
//     budget was exhausted, or maxJobs was zero);
//   - err: an uncatchable error (such as *InterruptedError or
//     *StackOverflowError) raised by one of the executed jobs. Regular
//     JavaScript exceptions thrown by promise callbacks are not returned here:
//     they reject the corresponding promise and are reported through the
//     PromiseRejectionTracker, exactly as in the automatic draining mode.
//
// Exhausting the budget is a normal pause, not an error: executed == maxJobs
// with remaining == true and err == nil. Promise states, async context
// tracking and rejection tracking all carry over to the next PumpJobs() call.
// If err != nil the job that raised it has already run (and is included in
// executed), while the rest of the queue is retained; for an *InterruptedError
// the interrupt flag is cleared (mirroring the top level boundary semantics),
// so the caller can resume by calling PumpJobs() again, or call
// DiscardPendingJobs() to abandon the retained jobs. The retained queue is
// therefore handled by an explicit choice and is never silently lost.
//
// PumpJobs panics if:
//   - the manual job pump mode is not enabled (see SetManualJobPump);
//   - maxJobs is negative;
//   - the Runtime is already executing JavaScript, or PumpJobs is re-entered
//     (e.g. a job calls back into PumpJobs): a single Runtime must not be
//     pumped recursively.
//
// A maxJobs of zero executes nothing and reports whether the queue is empty.
// PumpJobs is not safe for concurrent use and may only be called from the
// goroutine that owns the Runtime.
func (r *Runtime) PumpJobs(maxJobs int) (executed int, remaining bool, err error) {
	if !r.manualJobPump {
		panic("goja: PumpJobs called without enabling the manual job pump mode (SetManualJobPump)")
	}
	if maxJobs < 0 {
		panic("goja: PumpJobs maxJobs must not be negative")
	}
	if len(r.vm.callStack) > 0 || r.jobPumpActive {
		panic("goja: PumpJobs cannot be re-entered while the Runtime is running")
	}

	r.jobPumpActive = true
	defer func() {
		r.jobPumpActive = false
	}()

	abrupt := false
	for executed < maxJobs && len(r.jobQueue) > 0 {
		job := r.jobQueue[0]
		r.jobQueue[0] = nil
		r.jobQueue = r.jobQueue[1:]
		executed++

		func() {
			defer func() {
				if x := recover(); x != nil {
					if ex := asUncatchableException(x); ex != nil {
						err = ex
						abrupt = true
						return
					}
					panic(x)
				}
			}()
			job()
		}()
		if err != nil {
			break
		}
	}

	if len(r.jobQueue) > 0 {
		// Compact the queue so that the consumed prefix does not keep the
		// backing array (and any values referenced from it) alive between
		// PumpJobs calls.
		r.jobQueue = append([]func(){}, r.jobQueue...)
		remaining = true
	} else {
		r.jobQueue = nil
	}
	if err != nil {
		if _, ok := err.(*InterruptedError); ok {
			r.ClearInterrupt()
		}
	}
	if !abrupt {
		// Match the top level leave() invariant so stale operand values do not
		// stay referenced between pump rounds.
		r.vm.sp = 0
		r.vm.stack = nil
	}
	return
}
