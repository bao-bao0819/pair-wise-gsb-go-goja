package goja

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// chainScript builds a short promise chain where every then callback itself
// chains another then. It records the order in which reactions run and resolves
// the final promise with a computed value.
const chainScript = `
var log = [];
var order = 0;
function tag(name, v) {
	log.push(name + ":" + (++order));
	return v;
}
var finalPromise =
	Promise.resolve(1)
	.then(function (v) {
		tag("a", v);
		return Promise.resolve(v + 1)
			.then(function (w) { return tag("a1", w); });
	})
	.then(function (v) {
		tag("b", v);
		return Promise.resolve(v + 1)
			.then(function (w) { return tag("b1", w); });
	})
	.then(function (v) {
		return tag("c", v + 17);
	});
`

const expectedOrder = "a:1,a1:2,b:3,b1:4,c:5"

func runManualVM(t *testing.T) *Runtime {
	t.Helper()
	vm := New()
	vm.SetManualJobPump(true)
	if _, err := vm.RunString(chainScript); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	return vm
}

func logValue(t *testing.T, vm *Runtime) string {
	t.Helper()
	v, err := vm.RunString("log.join(',')")
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return v.String()
}

func finalValue(t *testing.T, vm *Runtime) string {
	t.Helper()
	p := vm.Get("finalPromise").Export().(*Promise)
	if p.State() != PromiseStateFulfilled {
		t.Fatalf("final promise state = %v, want fulfilled", p.State())
	}
	return p.Result().String()
}

func TestManualJobPump_DefaultStillDrains(t *testing.T) {
	vm := New()
	if _, err := vm.RunString(chainScript); err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if vm.HasPendingJobs() {
		t.Fatal("default mode must drain the queue on return")
	}
	if got := logValue(t, vm); got != expectedOrder {
		t.Fatalf("order = %s, want %s", got, expectedOrder)
	}
	if got := finalValue(t, vm); got != "20" {
		t.Fatalf("final value = %s, want 20", got)
	}
}

func TestManualJobPump_BudgetSlicesMatchFullDrain(t *testing.T) {
	// Every budget slice (including a full drain) must produce the same
	// FIFO order and the same final value.
	for _, budget := range []int{1, 2, 3, 5, 100} {
		t.Run(fmt.Sprintf("budget%d", budget), func(t *testing.T) {
			vm := runManualVM(t)
			if !vm.HasPendingJobs() {
				t.Fatal("queue must be retained after the top level script returns")
			}

			total := 0
			for {
				res := vm.RunJobs(budget)
				if res.Err != nil {
					t.Fatalf("RunJobs: %v", res.Err)
				}
				if res.Ran == 0 {
					t.Fatal("made no progress")
				}
				if res.Ran > budget {
					t.Fatalf("ran %d jobs, budget is %d", res.Ran, budget)
				}
				total += res.Ran
				if !res.HasPending {
					break
				}
				if res.Ran != budget {
					t.Fatalf("partial batch of %d with pending jobs and budget %d", res.Ran, budget)
				}
			}

			// The five tagged reactions plus internal thenable-resolution jobs.
			if total != 9 {
				t.Fatalf("executed %d jobs in total, want 9", total)
			}
			if vm.HasPendingJobs() {
				t.Fatal("queue must be empty after draining")
			}
			if got := logValue(t, vm); got != expectedOrder {
				t.Fatalf("order = %s, want %s", got, expectedOrder)
			}
			if got := finalValue(t, vm); got != "20" {
				t.Fatalf("final value = %s, want 20", got)
			}
		})
	}
}

func TestManualJobPump_BudgetExhaustionPausesMidChain(t *testing.T) {
	vm := runManualVM(t)

	res := vm.RunJobs(1)
	if res.Err != nil || res.Ran != 1 || !res.HasPending {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := logValue(t, vm); got != "a:1" {
		t.Fatalf("order after 1 job = %q, want a:1", got)
	}

	// The final promise is still pending; state must be resumable later.
	if p := vm.Get("finalPromise").Export().(*Promise); p.State() != PromiseStatePending {
		t.Fatalf("final promise state = %v, want pending", p.State())
	}

	res = vm.RunJobs(1)
	if res.Err != nil || res.Ran != 1 || !res.HasPending {
		t.Fatalf("unexpected second result: %+v", res)
	}
	if got := logValue(t, vm); got != "a:1,a1:2" {
		t.Fatalf("order after 2 jobs = %q", got)
	}

	res = vm.RunJobs(0) // 0 means drain the rest
	if res.Err != nil || res.HasPending {
		t.Fatalf("drain failed: %+v", res)
	}
	if got := logValue(t, vm); got != expectedOrder {
		t.Fatalf("order = %s, want %s", got, expectedOrder)
	}
	if got := finalValue(t, vm); got != "20" {
		t.Fatalf("final value = %s, want 20", got)
	}
}

func TestManualJobPump_RejectionTrackedOnceAcrossRuns(t *testing.T) {
	vm := New()
	var ops []string
	vm.SetPromiseRejectionTracker(func(p *Promise, operation PromiseRejectionOperation) {
		switch operation {
		case PromiseRejectionReject:
			ops = append(ops, "reject:"+p.Result().String())
		case PromiseRejectionHandle:
			ops = append(ops, "handle:"+p.Result().String())
		default:
			t.Errorf("unknown tracker operation %d", operation)
		}
	})
	vm.SetManualJobPump(true)

	if _, err := vm.RunString(`
		var recovered;
		var p;
		Promise.resolve().then(function () {
			p = new Promise(function (_, reject) { reject(new Error("boom")); });
		});
		Promise.resolve().then(function () {
			p.then(function (v) { recovered = v; },
			       function (e) { recovered = "handled:" + e.message; });
		});
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	// First run executes only the rejection job: the promise rejects without
	// any handler yet, reported exactly once as PromiseRejectionReject.
	res := vm.RunJobs(1)
	if res.Err != nil {
		t.Fatalf("RunJobs: %v", res.Err)
	}
	if got := strings.Join(ops, ","); got != "reject:Error: boom" {
		t.Fatalf("tracker ops after first run = %q, want reject only", got)
	}
	if !res.HasPending || res.Ran != 1 {
		t.Fatalf("only the rejection job should have run, got %+v", res)
	}

	// Second run: the rejection handler is attached (Handle once) and its
	// reaction job executes. The rejection must not be reported again.
	before := len(ops)
	res = vm.RunJobs(10)
	if res.Err != nil || res.HasPending {
		t.Fatalf("RunJobs: %+v", res)
	}
	if got := strings.Join(ops, ","); got != "reject:Error: boom,handle:Error: boom" {
		t.Fatalf("tracker ops after second run = %q", got)
	}
	if len(ops) != before+1 {
		t.Fatalf("expected exactly one Handle notification, got %v", ops)
	}

	v, err := vm.RunString("recovered")
	if err != nil {
		t.Fatalf("read recovered: %v", err)
	}
	if v.String() != "handled:boom" {
		t.Fatalf("recovered = %q", v.String())
	}
}

func TestManualJobPump_RejectsReentrancy(t *testing.T) {
	vm := New()
	vm.SetManualJobPump(true)
	if _, err := vm.RunString(`
		var resolve;
		var p = new Promise(function (res) { resolve = res; });
		p.then(function () {});
		resolve(1);
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	vm.Set("pump", func(call FunctionCall) Value {
		res := vm.RunJobs(1)
		if res.Err == nil {
			t.Error("reentrant RunJobs must return an error")
		}
		return _undefined
	})

	// A job that itself triggers RunJobs: the outer pump keeps going, the
	// nested call is refused and the queue is not corrupted.
	if _, err := vm.RunString(`
		new Promise(function (resolve) { resolve(); }).then(function () { pump(); });
	`); err != nil {
		t.Fatalf("RunScript enqueue: %v", err)
	}

	res := vm.RunJobs(10)
	if res.Err != nil || res.HasPending {
		t.Fatalf("drain after reentrant attempt: %+v", res)
	}
}

func TestManualJobPump_RequiresManualMode(t *testing.T) {
	vm := New()
	res := vm.RunJobs(10)
	if res.Err == nil {
		t.Fatal("RunJobs in automatic mode must fail")
	}
	if res.HasPending {
		t.Fatal("no jobs can be pending in automatic mode after return")
	}
}

func TestManualJobPump_InterruptKeepsQueuedJobs(t *testing.T) {
	vm := New()
	vm.SetManualJobPump(true)
	if _, err := vm.RunString(`
		var steps = [];
		function make(i) {
			return function () {
				if (i === 1 && !steps.sawOne) {
					steps.sawOne = true;
					while (true) {} // interrupted on the first attempt
				}
				steps.push(i);
			};
		}
		// Four independent reactions, all four jobs are enqueued up front.
		Promise.resolve().then(make(0));
		Promise.resolve().then(make(1));
		Promise.resolve().then(make(2));
		Promise.resolve().then(make(3));
	`); err != nil {
		t.Fatalf("RunScript: %v", err)
	}

	time.AfterFunc(100*time.Millisecond, func() { vm.Interrupt("stop") })

	res := vm.RunJobs(0)
	if res.Err == nil {
		t.Fatal("expected the interrupt to be reported")
	}
	if _, ok := res.Err.(*InterruptedError); !ok {
		t.Fatalf("want *InterruptedError, got %T: %v", res.Err, res.Err)
	}
	if res.Ran != 1 {
		t.Fatalf("ran %d jobs before interrupt, want 1", res.Ran)
	}
	if !res.HasPending {
		t.Fatal("the interrupted and queued jobs must be retained, not discarded")
	}

	vm.ClearInterrupt()

	// Continuing after clearing the interrupt retries the interrupted job at
	// the head of the queue and drains the rest. No reaction is lost or run
	// twice: make1's side effect only appears after the successful retry.
	res = vm.RunJobs(0)
	if res.Err != nil || res.HasPending {
		t.Fatalf("continue after interrupt: %+v", res)
	}
	v, err := vm.RunString("steps.join(',')")
	if err != nil {
		t.Fatalf("read steps: %v", err)
	}
	if v.String() != "0,1,2,3" {
		t.Fatalf("steps = %q, want 0,1,2,3", v.String())
	}
}

func TestManualJobPump_RepeatedTopLevelRunsKeepQueue(t *testing.T) {
	vm := New()
	vm.SetManualJobPump(true)
	if _, err := vm.RunString(`
		var log = [];
		Promise.resolve().then(function () { log.push(1); });
	`); err != nil {
		t.Fatalf("RunScript 1: %v", err)
	}
	if !vm.HasPendingJobs() {
		t.Fatal("queue must survive the first top level return")
	}
	if _, err := vm.RunString(`
		Promise.resolve().then(function () { log.push(2); });
	`); err != nil {
		t.Fatalf("RunScript 2: %v", err)
	}
	res := vm.RunJobs(0)
	if res.Err != nil || res.HasPending {
		t.Fatalf("drain: %+v", res)
	}
	v, err := vm.RunString("log.join(',')")
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if v.String() != "1,2" {
		t.Fatalf("log = %q, want 1,2", v.String())
	}
}
