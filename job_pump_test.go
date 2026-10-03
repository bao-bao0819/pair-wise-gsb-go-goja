package goja

import (
	"errors"
	"fmt"
	"testing"
)

// A short chain in which every 'then' handler itself creates another 'then'
// (via a thenable). The whole chain needs 9 reaction jobs, produces the log
// a0,b1,c2 and settles globalThis.p with the value 3.
const pumpChainScript = `
var log = [];
function thenable(prev, label, val) {
	return {
		then: function(resolve) {
			Promise.resolve(prev).then(function(v) {
				log.push(label + v);
				resolve(val);
			});
		}
	};
}
globalThis.p = Promise.resolve(0)
	.then(function(v) { return thenable(v, "a", 1); })
	.then(function(v) { return thenable(v, "b", 2); })
	.then(function(v) { return thenable(v, "c", 3); });
`

func getPumpLog(t *testing.T, r *Runtime) string {
	t.Helper()
	v, err := r.RunString("log.join(',')")
	if err != nil {
		t.Fatal(err)
	}
	return v.String()
}

func finalPumpResult(t *testing.T, r *Runtime) (PromiseState, Value) {
	t.Helper()
	p, err := r.RunString("p")
	if err != nil {
		t.Fatal(err)
	}
	promise := p.Export().(*Promise)
	return promise.State(), promise.Result()
}

// TestManualJobPumpDefaultStillDrains ensures the default behaviour is
// unchanged: when the manual pump is not enabled a top level script fully
// drains the reaction queue before returning.
func TestManualJobPumpDefaultStillDrains(t *testing.T) {
	r := New()
	if _, err := r.RunString(pumpChainScript); err != nil {
		t.Fatal(err)
	}
	if log := getPumpLog(t, r); log != "a0,b1,c2" {
		t.Fatalf("log = %q, want a0,b1,c2", log)
	}
	state, result := finalPumpResult(t, r)
	if state != PromiseStateFulfilled || result.ToInteger() != 3 {
		t.Fatalf("p = (%v, %v), want (fulfilled, 3)", state, result)
	}
}

func TestManualJobPumpBudgetSplits(t *testing.T) {
	type roundSpec struct {
		executed int
		log      string
		pending  bool
	}
	cases := []struct {
		budget int
		rounds []roundSpec
	}{
		{
			budget: 100,
			rounds: []roundSpec{
				{9, "a0,b1,c2", false},
			},
		},
		{
			budget: 3,
			rounds: []roundSpec{
				{3, "a0", true},
				{3, "a0,b1", true},
				{3, "a0,b1,c2", false},
			},
		},
		{
			budget: 2,
			rounds: []roundSpec{
				{2, "", true},
				{2, "a0", true},
				{2, "a0,b1", true},
				{2, "a0,b1", true},
				{1, "a0,b1,c2", false},
			},
		},
		{
			budget: 1,
			rounds: []roundSpec{
				{1, "", true},
				{1, "", true},
				{1, "a0", true},
				{1, "a0", true},
				{1, "a0", true},
				{1, "a0,b1", true},
				{1, "a0,b1", true},
				{1, "a0,b1", true},
				{1, "a0,b1,c2", false},
			},
		},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("budget=%d", tc.budget), func(t *testing.T) {
			r := New()
			r.SetManualJobPump(true)
			if _, err := r.RunString(pumpChainScript); err != nil {
				t.Fatal(err)
			}
			total := 0
			for i, want := range tc.rounds {
				if got := r.PendingJobCount(); got == 0 && want.pending {
					t.Fatalf("round %d: PendingJobCount = 0, want jobs pending", i)
				}
				executed, remaining, err := r.PumpJobs(tc.budget)
				if err != nil {
					t.Fatal(err)
				}
				if executed != want.executed {
					t.Fatalf("round %d: executed = %d, want %d", i, executed, want.executed)
				}
				if remaining != want.pending {
					t.Fatalf("round %d: remaining = %v, want %v", i, remaining, want.pending)
				}
				if log := getPumpLog(t, r); log != want.log {
					t.Fatalf("round %d: log = %q, want %q", i, log, want.log)
				}
				total += executed
			}
			if total != 9 {
				t.Fatalf("total executed = %d, want 9", total)
			}
			if r.PendingJobCount() != 0 {
				t.Fatalf("pending = %d, want 0", r.PendingJobCount())
			}
			state, result := finalPumpResult(t, r)
			if state != PromiseStateFulfilled || result.ToInteger() != 3 {
				t.Fatalf("p = (%v, %v), want (fulfilled, 3)", state, result)
			}
		})
	}
}

// TestManualJobPumpZeroBudget checks a zero budget round only reports the
// queue state without executing anything.
func TestManualJobPumpZeroBudget(t *testing.T) {
	r := New()
	r.SetManualJobPump(true)
	if _, err := r.RunString(pumpChainScript); err != nil {
		t.Fatal(err)
	}
	executed, remaining, err := r.PumpJobs(0)
	if err != nil {
		t.Fatal(err)
	}
	if executed != 0 || !remaining {
		t.Fatalf("PumpJobs(0) = (%d, %v), want (0, true)", executed, remaining)
	}
	if log := getPumpLog(t, r); log != "" {
		t.Fatalf("log = %q, want empty", log)
	}

	executed, remaining, err = r.PumpJobs(100)
	if err != nil || executed != 9 || remaining {
		t.Fatalf("PumpJobs(100) = (%d, %v, %v), want (9, false, nil)", executed, remaining, err)
	}
	executed, remaining, err = r.PumpJobs(0)
	if err != nil || executed != 0 || remaining {
		t.Fatalf("PumpJobs(0) on empty queue = (%d, %v, %v), want (0, false, nil)", executed, remaining, err)
	}
}

// TestManualJobPumpFIFO checks that jobs appended by a running job go to the
// end of the queue, so a pre-scheduled independent reaction is not overtaken.
func TestManualJobPumpFIFO(t *testing.T) {
	r := New()
	r.SetManualJobPump(true)
	if _, err := r.RunString(`
		var log = [];
		Promise.resolve().then(function() {
			log.push("first");
			Promise.resolve().then(function() {
				log.push("appended");
			});
		});
		Promise.resolve().then(function() {
			log.push("second");
		});
	`); err != nil {
		t.Fatal(err)
	}
	executed, remaining, err := r.PumpJobs(2)
	if err != nil {
		t.Fatal(err)
	}
	if executed != 2 || !remaining {
		t.Fatalf("PumpJobs(2) = (%d, %v), want (2, true)", executed, remaining)
	}
	if log := getPumpLog(t, r); log != "first,second" {
		t.Fatalf("log = %q, want first,second", log)
	}
	executed, remaining, err = r.PumpJobs(1)
	if err != nil || executed != 1 || remaining {
		t.Fatalf("PumpJobs(1) = (%d, %v, %v), want (1, false, nil)", executed, remaining, err)
	}
	if log := getPumpLog(t, r); log != "first,second,appended" {
		t.Fatalf("log = %q, want first,second,appended", log)
	}
}

// TestManualJobPumpRejectionTrackerAcrossRounds checks that a rejection is
// reported exactly once in the round it happens, and the subsequent PromiseRejectionHandle
// is reported exactly once when a handler is attached in a later round.
func TestManualJobPumpRejectionTrackerAcrossRounds(t *testing.T) {
	r := New()
	r.SetManualJobPump(true)

	var ops []PromiseRejectionOperation
	r.SetPromiseRejectionTracker(func(p *Promise, op PromiseRejectionOperation) {
		ops = append(ops, op)
	})

	p, _, reject := r.NewPromise()
	r.Set("p", r.ToValue(p))
	r.Set("rejectP", r.ToValue(func(v Value) Value {
		if err := reject(v); err != nil {
			t.Fatal(err)
		}
		return _undefined
	}))

	if _, err := r.RunString(`
		var caught;
		var rejected = false;
		Promise.resolve().then(function() {
			rejectP("boom");
			rejected = true;
		});
	`); err != nil {
		t.Fatal(err)
	}

	// Round 1: the job rejects p while it has no handler attached, so exactly
	// one PromiseRejectionReject is emitted, and the queue becomes empty.
	executed, remaining, err := r.PumpJobs(1)
	if err != nil || executed != 1 || remaining {
		t.Fatalf("round 1: PumpJobs(1) = (%d, %v, %v), want (1, false, nil)", executed, remaining, err)
	}
	v, err := r.RunString("rejected")
	if err != nil || !v.ToBoolean() {
		t.Fatalf("rejected = %v, err = %v, want true", v, err)
	}
	if len(ops) != 1 || ops[0] != PromiseRejectionReject {
		t.Fatalf("ops after round 1 = %v, want [Reject]", ops)
	}
	pv := r.Get("p").Export().(*Promise)
	if pv.State() != PromiseStateRejected {
		t.Fatalf("p state = %v, want rejected", pv.State())
	}

	// Round 2: attach a handler to the already rejected promise. addReactions
	// emits exactly one PromiseRejectionHandle and enqueues one reaction job;
	// the earlier Reject notification must not be repeated.
	if _, err := r.RunString(`
		p.then(function() {
			throw new Error("unexpected fulfillment");
		}, function(reason) {
			if (reason !== "boom") throw new Error("bad reason");
			caught = reason;
		});
	`); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 || ops[0] != PromiseRejectionReject || ops[1] != PromiseRejectionHandle {
		t.Fatalf("ops after attaching handler = %v, want [Reject, Handle]", ops)
	}
	if r.PendingJobCount() != 1 {
		t.Fatalf("pending = %d, want 1", r.PendingJobCount())
	}

	executed, remaining, err = r.PumpJobs(100)
	if err != nil || executed != 1 || remaining {
		t.Fatalf("round 2: PumpJobs = (%d, %v, %v), want (1, false, nil)", executed, remaining, err)
	}
	v, err = r.RunString("caught")
	if err != nil || v.String() != "boom" {
		t.Fatalf("caught = %v, err = %v, want boom", v, err)
	}
	if len(ops) != 2 {
		t.Fatalf("ops after round 2 = %v, want [Reject, Handle]", ops)
	}
	executed, remaining, err = r.PumpJobs(100)
	if err != nil || executed != 0 || remaining {
		t.Fatalf("extra PumpJobs = (%d, %v, %v), want (0, false, nil)", executed, remaining, err)
	}
	if len(ops) != 2 {
		t.Fatalf("ops after extra pump = %v, want [Reject, Handle]", ops)
	}
}

// TestManualJobPumpJavaScriptExceptionIsRejection checks that a JS exception
// thrown in a reaction becomes a promise rejection (reported through the
// tracker), not an error returned from PumpJobs, and that a downstream .catch
// can recover the chain in a later round.
func TestManualJobPumpJavaScriptExceptionIsRejection(t *testing.T) {
	r := New()
	r.SetManualJobPump(true)
	var rejections []string
	r.SetPromiseRejectionTracker(func(p *Promise, op PromiseRejectionOperation) {
		if op == PromiseRejectionReject {
			rejections = append(rejections, p.Result().String())
		}
	})
	if _, err := r.RunString(`
		var recovered;
		Promise.resolve().then(function() {
			throw "kaboom";
		}).then(function() {
			throw new Error("unexpected fulfillment");
		}).catch(function(reason) {
			recovered = reason;
		});
	`); err != nil {
		t.Fatal(err)
	}

	// First job throws: its result promise is rejected, but there is already a
	// .then queued after it which passes the rejection through, so no unhandled
	// rejection event is generated yet and PumpJobs reports no Go error.
	executed, remaining, err := r.PumpJobs(2)
	if err != nil {
		t.Fatalf("PumpJobs returned error %v, JS exceptions must reject promises", err)
	}
	if executed != 2 || !remaining {
		t.Fatalf("PumpJobs(2) = (%d, %v), want (2, true)", executed, remaining)
	}
	executed, remaining, err = r.PumpJobs(100)
	if err != nil || remaining {
		t.Fatalf("drain PumpJobs = (%d, %v, %v)", executed, remaining, err)
	}
	v, err := r.RunString("recovered")
	if err != nil || v.String() != "kaboom" {
		t.Fatalf("recovered = %v, err = %v, want kaboom", v, err)
	}
	if len(rejections) != 0 {
		t.Fatalf("unhandled rejections = %v, want none", rejections)
	}
}

// TestManualJobPumpReentrantPanics checks that re-entering the pump from a job
// (or from a top level script in manual mode) panics and leaves the runtime
// usable for the next round.
func TestManualJobPumpReentrantPanics(t *testing.T) {
	r := New()
	r.SetManualJobPump(true)
	r.Set("pumpFromJob", r.ToValue(func() {
		func() {
			defer func() {
				if x := recover(); x == nil {
					t.Error("expected panic on re-entrant PumpJobs")
				}
			}()
			_, _, _ = r.PumpJobs(1)
		}()
	}))
	if _, err := r.RunString(`
		var log = [];
		Promise.resolve().then(function() {
			pumpFromJob();
			log.push("after");
		});
	`); err != nil {
		t.Fatal(err)
	}
	executed, remaining, err := r.PumpJobs(1)
	if err != nil || executed != 1 || remaining {
		t.Fatalf("PumpJobs = (%d, %v, %v), want (1, false, nil)", executed, remaining, err)
	}
	if log := getPumpLog(t, r); log != "after" {
		t.Fatalf("log = %q, want after", log)
	}

	// Re-entering the active pump directly (e.g. a Go host helper that is
	// invoked synchronously while a round is running and itself calls
	// PumpJobs) must panic as well. Go-bound functions called from a script
	// turn panics into JS exceptions instead (see README, Exceptions).
	secondRound := false
	r.Set("pumpFromSecondJob", r.ToValue(func() {
		func() {
			defer func() {
				if x := recover(); x == nil {
					t.Error("expected panic on PumpJobs while a round is active")
				} else {
					secondRound = true
				}
			}()
			_, _, _ = r.PumpJobs(1)
		}()
	}))
	if _, err := r.RunString(`
		var second = false;
		Promise.resolve().then(function() {
			pumpFromSecondJob();
			second = true;
		});
	`); err != nil {
		t.Fatal(err)
	}
	executed, remaining, err = r.PumpJobs(100)
	if err != nil || executed != 1 || remaining || !secondRound {
		t.Fatalf("PumpJobs = (%d, %v, %v) secondRound=%v", executed, remaining, err, secondRound)
	}
	v, err := r.RunString("second")
	if err != nil || !v.ToBoolean() {
		t.Fatalf("second = %v, err = %v, want true", v, err)
	}
}

// TestManualJobPumpInterruptRetainsQueue checks that when an interrupt hits a
// job the remaining jobs are retained (not silently lost), the interrupt is
// cleared and pumping can resume.
func TestManualJobPumpInterruptRetainsQueue(t *testing.T) {
	r := New()
	r.SetManualJobPump(true)
	r.Set("abort", r.ToValue(func() {
		r.Interrupt("stop here")
	}))
	if _, err := r.RunString(`
		var log = [];
		Promise.resolve().then(function() {
			abort();
		});
		Promise.resolve().then(function() { log.push("two"); });
		Promise.resolve().then(function() { log.push("three"); });
	`); err != nil {
		t.Fatal(err)
	}

	executed, remaining, err := r.PumpJobs(10)
	if err == nil {
		t.Fatal("expected InterruptedError, got nil")
	}
	var intErr *InterruptedError
	if !errors.As(err, &intErr) {
		t.Fatalf("err = %T, want *InterruptedError", err)
	}
	if intErr.Value() != "stop here" {
		t.Fatalf("interrupt value = %v, want stop here", intErr.Value())
	}
	if executed != 1 {
		t.Fatalf("executed = %d, want 1", executed)
	}
	if !remaining || r.PendingJobCount() != 2 {
		t.Fatalf("remaining = %v, pending = %d, want 2 jobs retained", remaining, r.PendingJobCount())
	}

	// The interrupt flag is cleared, so a fresh script can run and the retained
	// queue can be pumped.
	if _, err := r.RunString("log.push('resumed')"); err != nil {
		t.Fatalf("RunString after interrupt: %v", err)
	}
	executed, remaining, err = r.PumpJobs(10)
	if err != nil || executed != 2 || remaining {
		t.Fatalf("resume PumpJobs = (%d, %v, %v), want (2, false, nil)", executed, remaining, err)
	}
	if log := getPumpLog(t, r); log != "resumed,two,three" {
		t.Fatalf("log = %q, want resumed,two,three", log)
	}
}

// TestManualJobPumpInterruptInTopLevelScriptRetainsQueue checks that when the
// top level script itself is interrupted in manual mode, the jobs queued before
// the interruption are retained (rather than cleared) and remain resumable.
func TestManualJobPumpInterruptInTopLevelScriptRetainsQueue(t *testing.T) {
	r := New()
	r.SetManualJobPump(true)
	r.Set("abort", r.ToValue(func() {
		r.Interrupt("top stop")
	}))
	_, err := r.RunString(`
		var log = [];
		Promise.resolve().then(function() { log.push("one"); });
		Promise.resolve().then(function() { log.push("two"); });
		abort();
	`)
	if err == nil {
		t.Fatal("expected InterruptedError")
	}
	var intErr *InterruptedError
	if !errors.As(err, &intErr) {
		t.Fatalf("err = %T, want *InterruptedError", err)
	}
	if r.PendingJobCount() != 2 {
		t.Fatalf("pending = %d, want 2 jobs retained", r.PendingJobCount())
	}
	executed, remaining, err := r.PumpJobs(100)
	if err != nil || executed != 2 || remaining {
		t.Fatalf("PumpJobs = (%d, %v, %v), want (2, false, nil)", executed, remaining, err)
	}
	if log := getPumpLog(t, r); log != "one,two" {
		t.Fatalf("log = %q, want one,two", log)
	}
}

// TestManualJobPumpStackOverflowRetainsQueue checks that other uncatchable
// errors (stack overflow) are returned as Go errors while the queue survives.
func TestManualJobPumpStackOverflowRetainsQueue(t *testing.T) {
	r := New()
	r.SetManualJobPump(true)
	r.SetMaxCallStackSize(20)
	r.Set("overflow", r.ToValue(func() {
		_, err := r.RunString("(function loop() { loop() })()")
		if err != nil {
			panic(err)
		}
	}))
	if _, err := r.RunString(`
		var log = [];
		Promise.resolve().then(function() {
			overflow();
		});
		Promise.resolve().then(function() { log.push("survivor"); });
	`); err != nil {
		t.Fatal(err)
	}
	executed, remaining, err := r.PumpJobs(10)
	if err == nil {
		t.Fatal("expected StackOverflowError, got nil")
	}
	var soErr *StackOverflowError
	if !errors.As(err, &soErr) {
		t.Fatalf("err = %T, want *StackOverflowError", err)
	}
	if executed != 1 || !remaining || r.PendingJobCount() != 1 {
		t.Fatalf("executed = %d, remaining = %v, pending = %d", executed, remaining, r.PendingJobCount())
	}
	executed, remaining, err = r.PumpJobs(10)
	if err != nil || executed != 1 || remaining {
		t.Fatalf("resume PumpJobs = (%d, %v, %v), want (1, false, nil)", executed, remaining, err)
	}
	if log := getPumpLog(t, r); log != "survivor" {
		t.Fatalf("log = %q, want survivor", log)
	}
}

// TestManualJobPumpDiscardAndDisable checks the explicit policies for a queue
// left over after a pause: DiscardPendingJobs drops it, and switching back to
// automatic mode drains it at the next top level boundary.
func TestManualJobPumpDiscardAndDisable(t *testing.T) {
	r := New()
	r.SetManualJobPump(true)
	if _, err := r.RunString(`
		var log = [];
		Promise.resolve().then(function() { log.push("discarded"); });
	`); err != nil {
		t.Fatal(err)
	}
	if n, remaining, err := r.PumpJobs(0); err != nil || n != 0 || !remaining {
		t.Fatalf("PumpJobs(0) = (%d, %v, %v), want (0, true, nil)", n, remaining, err)
	}
	r.DiscardPendingJobs()
	if r.PendingJobCount() != 0 {
		t.Fatalf("pending after discard = %d, want 0", r.PendingJobCount())
	}
	if _, _, err := r.PumpJobs(100); err != nil || r.PendingJobCount() != 0 {
		t.Fatalf("pump after discard: pending = %d, err = %v", r.PendingJobCount(), err)
	}
	if log := getPumpLog(t, r); log != "" {
		t.Fatalf("log = %q, discarded job must not run", log)
	}

	// New work queued after discarding still works.
	if _, err := r.RunString(`
		Promise.resolve().then(function() { log.push("kept"); });
	`); err != nil {
		t.Fatal(err)
	}
	r.SetManualJobPump(false)
	if r.PendingJobCount() != 1 {
		t.Fatalf("pending after disabling = %d, want 1", r.PendingJobCount())
	}
	if _, err := r.RunString(""); err != nil {
		t.Fatal(err)
	}
	if log := getPumpLog(t, r); log != "kept" {
		t.Fatalf("log = %q, want kept", log)
	}
	if r.PendingJobCount() != 0 {
		t.Fatalf("pending = %d, want 0", r.PendingJobCount())
	}
}

type pumpCountingAsyncTracker struct {
	grabbed, resumed, exited int
}

func (t *pumpCountingAsyncTracker) Grab() interface{} {
	t.grabbed++
	return t.grabbed
}

func (t *pumpCountingAsyncTracker) Resumed(interface{}) {
	t.resumed++
}

func (t *pumpCountingAsyncTracker) Exited() {
	t.exited++
}

// TestManualJobPumpAsyncContextBalance checks that every reaction resumed in a
// pump is also exited, even across pump rounds, and a waiting async function
// resumes with its context in a later round.
func TestManualJobPumpAsyncContextBalance(t *testing.T) {
	r := New()
	r.SetManualJobPump(true)
	tracker := &pumpCountingAsyncTracker{}
	r.SetAsyncContextTracker(tracker)
	if _, err := r.RunString(`
		async function work(v) {
			await Promise.resolve(v);
			return v + 1;
		}
		globalThis.result;
		work(41).then(function(v) {
			globalThis.result = v;
		});
	`); err != nil {
		t.Fatal(err)
	}

	for {
		executed, remaining, err := r.PumpJobs(1)
		if err != nil {
			t.Fatal(err)
		}
		if tracker.resumed != tracker.exited {
			t.Fatalf("resumed = %d, exited = %d, must match after each round", tracker.resumed, tracker.exited)
		}
		if !remaining {
			if executed == 0 {
				break
			}
			break
		}
	}
	v, err := r.RunString("result")
	if err != nil || v.ToInteger() != 42 {
		t.Fatalf("result = %v, err = %v, want 42", v, err)
	}
	if tracker.grabbed == 0 || tracker.resumed != tracker.exited || tracker.resumed > tracker.grabbed {
		t.Fatalf("tracker counts: grabbed=%d resumed=%d exited=%d", tracker.grabbed, tracker.resumed, tracker.exited)
	}
}

// TestManualJobPumpPanics checks the API misuse panics.
func TestManualJobPumpPanics(t *testing.T) {
	r := New()
	assertPanic := func(msg string, f func()) {
		t.Helper()
		defer func() {
			if x := recover(); x == nil {
				t.Fatalf("%s: expected panic", msg)
			}
		}()
		f()
	}
	assertPanic("PumpJobs before enabling", func() { _, _, _ = r.PumpJobs(1) })

	r.SetManualJobPump(true)
	assertPanic("negative budget", func() { _, _, _ = r.PumpJobs(-1) })
}
