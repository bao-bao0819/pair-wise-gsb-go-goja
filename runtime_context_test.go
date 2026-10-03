package goja

import (
	stdcontext "context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

var contextFollowupProgram = MustCompile("followup.js", `42`, false)

func runFixedValueAfterCancellation(t *testing.T, rt *Runtime) {
	t.Helper()
	v, err := rt.RunProgram(contextFollowupProgram)
	if err != nil {
		t.Fatalf("late cancellation leaked into the next execution: %v", err)
	}
	if v.ToInteger() != 42 {
		t.Fatalf("unexpected follow-up result: %v", v)
	}
}

func assertContextInterrupted(t *testing.T, err error, ctx stdcontext.Context) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an interrupt error, got nil")
	}
	var intErr *InterruptedError
	if !errors.As(err, &intErr) {
		t.Fatalf("expected *InterruptedError, got %T: %v", err, err)
	}
	if !errors.Is(err, stdcontext.Canceled) {
		t.Fatalf("expected errors.Is(err, stdcontext.Canceled), got %v", err)
	}
	if ctx != nil && ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
		t.Fatalf("expected the error to wrap %v, got %v", ctx.Err(), err)
	}
}

// Cancellation before the execution starts: the script must not run at all.
func TestRunProgramContextCancelledBeforeStart(t *testing.T) {
	rt := New()
	var ran int32
	rt.Set("mark", func() {
		atomic.StoreInt32(&ran, 1)
	})
	prg := MustCompile("cancelled_before.js", "mark(); 1;", false)

	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()

	_, err := rt.RunProgramContext(ctx, prg)
	assertContextInterrupted(t, err, ctx)
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatal("script from a pre-cancelled context must not run")
	}
	runFixedValueAfterCancellation(t, rt)
}

// Cancellation while the interpreter is executing a JS loop.
func TestRunProgramContextCancelledInLoop(t *testing.T) {
	rt := New()
	prg := MustCompile("loop.js", `
	var i = 0;
	while (true) { i++; }
	i;
	`, false)

	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cancel()
	}()

	_, err := rt.RunProgramContext(ctx, prg)
	wg.Wait()
	assertContextInterrupted(t, err, ctx)
	runFixedValueAfterCancellation(t, rt)
}

// Cancellation delivered at the execution return boundary, cancellation first:
// the host function cancels the context immediately before returning control to
// the interpreter, so the interrupt is observed at the next between-instruction
// check before the single remaining instruction runs.
func TestRunProgramContextCancelledAtReturnBoundaryInterrupted(t *testing.T) {
	rt := New()
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	armed := make(chan struct{})
	rt.vm.interruptForGenHook = func(gen uint32, didArm bool) {
		if didArm {
			close(armed)
		}
	}
	rt.Set("cancelThenReturn", func() {
		cancel()
		<-armed
	})

	prg := MustCompile("boundary_interrupted.js", `cancelThenReturn(); 7;`, false)
	_, err := rt.RunProgramContext(ctx, prg)
	assertContextInterrupted(t, err, ctx)
	runFixedValueAfterCancellation(t, rt)
}

// Cancellation delivered at the execution return boundary, completion first:
// the cancellation and the host function's return are released together behind
// a synchronisation barrier and repeatedly race the cancellation callback
// against the single remaining interpreter instruction. Whenever the flag is
// observed only after the script finished, normal completion must win and the
// late cancellation must not affect the next execution.
func TestRunProgramContextCancelledAtReturnBoundaryRace(t *testing.T) {
	const iterations = 200
	var normalWins int
	for i := 0; i < iterations; i++ {
		rt := New()

		entered := make(chan struct{})
		release := make(chan struct{})
		rt.Set("barrier", func() {
			close(entered)
			<-release
		})

		prg := MustCompile("boundary_race.js", `barrier(); 7;`, false)
		ctx, cancel := stdcontext.WithCancel(stdcontext.Background())

		resultCh := make(chan runResult, 1)
		go func() {
			v, err := rt.RunProgramContext(ctx, prg)
			resultCh <- runResult{v: v, err: err}
		}()

		<-entered
		cancel()
		close(release)

		res := <-resultCh
		if res.err != nil {
			var intErr *InterruptedError
			if !errors.As(res.err, &intErr) {
				t.Fatalf("iteration %d: unexpected error type: %v", i, res.err)
			}
			runFixedValueAfterCancellation(t, rt)
			continue
		}
		if res.v.ToInteger() != 7 {
			t.Fatalf("iteration %d: unexpected result: %v", i, res.v)
		}
		normalWins++
		runFixedValueAfterCancellation(t, rt)
	}
	if normalWins == 0 {
		t.Fatal("the completion-first ordering of the return-boundary race was never observed")
	}
}

type runResult struct {
	v   Value
	err error
}

// A cancellation belonging to one generation must not wipe an external
// interrupt requested via Interrupt(), and an external interrupt armed before
// the next run must still interrupt it.
func TestRunProgramContextDoesNotClearExternalInterrupt(t *testing.T) {
	rt := New()
	prg := MustCompile("ok.js", `1;`, false)

	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	cancel()
	// External interrupt armed after the context cancellation: the
	// pre-cancelled run must not consume it.
	rt.Interrupt("external")
	_, err := rt.RunProgramContext(ctx, prg)
	assertContextInterrupted(t, err, ctx)
	if ie, ok := err.(*InterruptedError); ok && ie.Value() == "external" {
		t.Fatal("pre-cancelled run must not consume the external interrupt")
	}

	// The external interrupt is still pending and must hit the next run.
	_, err = rt.RunProgram(prg)
	if err == nil {
		t.Fatal("expected the external interrupt to affect the next execution")
	}
	var intErr *InterruptedError
	if !errors.As(err, &intErr) || intErr.Value() != "external" {
		t.Fatalf("expected the external interrupt, got %v", err)
	}

	rt.ClearInterrupt()
	runFixedValueAfterCancellation(t, rt)
}

// Interrupt() requested while a context-bound execution is running takes
// precedence over a concurrent context cancellation.
func TestRunProgramContextExternalInterruptWins(t *testing.T) {
	rt := New()
	entered := make(chan struct{})
	release := make(chan struct{})
	rt.Set("park", func() {
		close(entered)
		<-release
	})
	prg := MustCompile("park.js", `park(); "done";`, false)
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	ctxArmed := make(chan struct{})
	rt.vm.interruptForGenHook = func(gen uint32, didArm bool) {
		if didArm {
			close(ctxArmed)
		}
	}

	errCh := make(chan error, 1)
	go func() {
		_, err := rt.RunProgramContext(ctx, prg)
		errCh <- err
	}()

	<-entered
	cancel()
	<-ctxArmed // the context cancellation interrupt is armed first
	rt.Interrupt("boom")
	close(release)

	err := <-errCh
	var intErr *InterruptedError
	if !errors.As(err, &intErr) || intErr.Value() != "boom" {
		t.Fatalf("expected the external interrupt to win, got %v", err)
	}
	rt.ClearInterrupt()
	runFixedValueAfterCancellation(t, rt)
}

// JS code invoked from a nested Go host callback belongs to the same execution
// generation and is interrupted by the same context.
func TestRunProgramContextNestedCallbackGeneration(t *testing.T) {
	rt := New()

	inner, err := rt.RunString(`(function() { var i = 0; while (true) { i++; } })`)
	if err != nil {
		t.Fatal(err)
	}
	innerFn, ok := AssertFunction(inner)
	if !ok {
		t.Fatal("not a function")
	}

	rt.Set("callInner", func(call FunctionCall) Value {
		v, callErr := innerFn(nil)
		if callErr != nil {
			panic(callErr)
		}
		return v
	})

	prg := MustCompile("nested.js", `callInner();`, false)
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())

	errCh := make(chan error, 1)
	go func() {
		_, e := rt.RunProgramContext(ctx, prg)
		errCh <- e
	}()

	cancel()
	err = <-errCh
	assertContextInterrupted(t, err, ctx)
	runFixedValueAfterCancellation(t, rt)
}

// RunProgramContext with a non-cancelled context behaves like RunProgram.
func TestRunProgramContextNormalCompletion(t *testing.T) {
	rt := New()
	prg := MustCompile("normal.js", `1 + 2`, false)
	v, err := rt.RunProgramContext(stdcontext.Background(), prg)
	if err != nil {
		t.Fatal(err)
	}
	if v.ToInteger() != 3 {
		t.Fatalf("unexpected result: %v", v)
	}
	runFixedValueAfterCancellation(t, rt)
}

// A cancellation while a blocking Go host function is running does not
// forcefully terminate the Go function; the interrupt applies once control is
// back in the interpreter.
func TestRunProgramContextDoesNotInterruptBlockingGo(t *testing.T) {
	rt := New()
	entered := make(chan struct{})
	release := make(chan struct{})
	armed := make(chan struct{})
	var hostFinished int32
	ctx, cancel := stdcontext.WithCancel(stdcontext.Background())
	rt.vm.interruptForGenHook = func(gen uint32, didArm bool) {
		if didArm {
			close(armed)
		}
	}

	rt.Set("blocking", func() {
		close(entered)
		<-release
		cancel()
		<-armed
		atomic.StoreInt32(&hostFinished, 1)
	})

	prg := MustCompile("blocking.js", `blocking(); "after";`, false)

	errCh := make(chan error, 1)
	go func() {
		_, err := rt.RunProgramContext(ctx, prg)
		errCh <- err
	}()

	<-entered
	// The host function is still blocked; the run cannot return yet.
	select {
	case err := <-errCh:
		t.Fatalf("run returned while the host function was blocked: %v", err)
	default:
	}

	close(release)
	err := <-errCh
	assertContextInterrupted(t, err, ctx)
	if atomic.LoadInt32(&hostFinished) != 1 {
		t.Fatal("the blocking host function must run to completion, it is not forcefully terminated")
	}
	runFixedValueAfterCancellation(t, rt)
}
