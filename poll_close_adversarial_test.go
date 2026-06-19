package srtgo

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Adversarial tests for the pd.close() inline-unblock fix.
// Goal: expose race conditions, edge cases, or correctness problems.
// ---------------------------------------------------------------------------

// TestAdversarialConcurrentCloseAndWait hammers close() and wait()
// simultaneously from separate goroutines. Run with -race to catch data races.
func TestAdversarialConcurrentCloseAndWait(t *testing.T) {
	const iterations = 200

	for i := 0; i < iterations; i++ {
		pd, cleanup := newTestPollDesc(t)

		var wg sync.WaitGroup
		wg.Add(2)

		// Goroutine 1: call wait(ModeRead)
		go func() {
			defer wg.Done()
			_ = pd.wait(ModeRead)
		}()

		// Goroutine 2: call close() -- no sleep, maximum race pressure
		go func() {
			defer wg.Done()
			pd.close()
		}()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
			// Success -- both returned without deadlock.
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: deadlock -- concurrent close+wait did not complete in 5s", i)
		}
		cleanup()
	}
}

// TestAdversarialConcurrentCloseAndWaitWrite is the ModeWrite variant.
func TestAdversarialConcurrentCloseAndWaitWrite(t *testing.T) {
	const iterations = 200

	for i := 0; i < iterations; i++ {
		pd, cleanup := newTestPollDesc(t)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			_ = pd.wait(ModeWrite)
		}()

		go func() {
			defer wg.Done()
			pd.close()
		}()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: deadlock -- concurrent close+wait(ModeWrite) did not complete in 5s", i)
		}
		cleanup()
	}
}

// TestAdversarialCloseDuringStateTransition tries to hit the window where
// wait() has done CAS(pollDefault -> pollWait) but hasn't entered the select
// yet. We do this by running many iterations with no sleep between launching
// the waiter and calling close().
func TestAdversarialCloseDuringStateTransition(t *testing.T) {
	const iterations = 500

	for i := 0; i < iterations; i++ {
		pd, cleanup := newTestPollDesc(t)
		errCh := make(chan error, 1)

		go func() {
			errCh <- pd.wait(ModeRead)
		}()

		// Spin-wait until rdState transitions to pollWait, then immediately close.
		// This maximises the chance of hitting the exact window.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if atomic.LoadInt32(&pd.rdState) == pollWait {
				break
			}
			// Busy-spin intentionally -- we want minimal delay.
		}

		pd.close()

		select {
		case err := <-errCh:
			if err == nil {
				t.Fatalf("iteration %d: wait() returned nil after close; expected SrtSocketClosed", i)
			}
			if _, ok := err.(*SrtSocketClosed); !ok {
				// Could also be SrtEpollTimeout if a timer raced, but SrtSocketClosed is expected.
				if _, ok2 := err.(*SrtEpollTimeout); !ok2 {
					t.Fatalf("iteration %d: unexpected error type %T: %v", i, err, err)
				}
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: deadlock -- wait() did not unblock after close()", i)
		}
		cleanup()
	}
}

// TestAdversarialDoubleClose calls close() twice. Must not panic or deadlock.
func TestAdversarialDoubleClose(t *testing.T) {
	pd, cleanup := newTestPollDesc(t)
	defer cleanup()

	// First close -- normal.
	pd.close()

	// Second close -- should be a no-op.
	done := make(chan struct{})
	go func() {
		pd.close()
		close(done)
	}()

	select {
	case <-done:
		t.Log("PASS: double close did not panic or deadlock")
	case <-time.After(2 * time.Second):
		t.Fatal("FAIL: double close deadlocked")
	}
}

// TestAdversarialDoubleCloseConcurrent calls close() from two goroutines
// simultaneously. Must not panic, double-signal, or deadlock.
func TestAdversarialDoubleCloseConcurrent(t *testing.T) {
	const iterations = 200

	for i := 0; i < iterations; i++ {
		pd, cleanup := newTestPollDesc(t)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			pd.close()
		}()
		go func() {
			defer wg.Done()
			pd.close()
		}()

		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: concurrent double close deadlocked", i)
		}
		cleanup()
	}
}

// TestAdversarialCloseWithNoWaiters closes a pollDesc when nobody is
// blocked in wait(). Must not panic or block.
func TestAdversarialCloseWithNoWaiters(t *testing.T) {
	pd, cleanup := newTestPollDesc(t)
	defer cleanup()

	// Verify initial state: nobody is waiting.
	if atomic.LoadInt32(&pd.rdState) != pollDefault {
		t.Fatalf("expected rdState=pollDefault, got %d", atomic.LoadInt32(&pd.rdState))
	}
	if atomic.LoadInt32(&pd.wrState) != pollDefault {
		t.Fatalf("expected wrState=pollDefault, got %d", atomic.LoadInt32(&pd.wrState))
	}

	done := make(chan struct{})
	go func() {
		pd.close()
		close(done)
	}()

	select {
	case <-done:
		t.Log("PASS: close with no waiters completed without panic or block")
	case <-time.After(2 * time.Second):
		t.Fatal("FAIL: close with no waiters blocked")
	}
}

// TestAdversarialPoolReuseStaleSignal tests that after close() signals the
// unblock channel, a recycled pollDesc from the pool doesn't see a stale
// signal. This is the most insidious edge case: if pollDescInit does not drain
// the channels, a new user of the pollDesc could get a spurious wakeup and
// return nil error (no closing, no pollErr, no timeout).
func TestAdversarialPoolReuseStaleSignal(t *testing.T) {
	// We can't easily force pool reuse through the public API, so we test
	// the mechanism directly by simulating the lifecycle.

	// Step 1: Create a pollDesc and have a waiter blocked on it.
	pd, cleanup := newTestPollDesc(t)

	errCh := make(chan error, 1)
	go func() {
		errCh <- pd.wait(ModeRead)
	}()

	// Wait for the goroutine to enter the wait state.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&pd.rdState) == pollWait {
			break
		}
	}
	if atomic.LoadInt32(&pd.rdState) != pollWait {
		cleanup()
		t.Fatal("waiter never entered pollWait state")
	}

	// Step 2: Close. This signals unblockRd.
	pd.close()

	// Wait for the waiter to exit.
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		cleanup()
		t.Fatal("waiter did not exit after close")
	}
	cleanup()

	// Step 3: At this point, the channel MIGHT have a stale signal if close()
	// sent one but the waiter consumed the epoll-loop signal instead, or vice
	// versa. Check if the unblockRd channel has a leftover message.
	//
	// We can't access the pool item directly after cleanup() returns it, but
	// we CAN create a new pollDesc and check if it gets a spurious wakeup.
	// If the pool returns the same object (likely since we just put it back),
	// and pollDescInit doesn't drain the channel, then wait() would return
	// immediately with nil error.

	pd2, cleanup2 := newTestPollDesc(t)
	defer cleanup2()

	// Set a very short deadline so wait() doesn't block forever if there's
	// no stale signal.
	pd2.setDeadline(time.Now().Add(200*time.Millisecond), ModeRead)

	err := pd2.wait(ModeRead)
	if err == nil {
		t.Fatal("FAIL: wait() on reused pollDesc returned nil -- stale signal in unblock channel from previous close()")
	}
	// Expected: SrtEpollTimeout (deadline expired, no data). That's fine.
	t.Logf("PASS: reused pollDesc wait returned: %v (no spurious nil)", err)
}

// TestAdversarialRapidOpenCloseReadCycle creates and closes pollDescs in a
// tight loop to stress pool recycling.
func TestAdversarialRapidOpenCloseReadCycle(t *testing.T) {
	const iterations = 100

	for i := 0; i < iterations; i++ {
		pd, cleanup := newTestPollDesc(t)

		// Launch a waiter.
		errCh := make(chan error, 1)
		go func() {
			errCh <- pd.wait(ModeRead)
		}()

		// Tiny sleep to let the goroutine enter wait sometimes, but not always.
		// This creates a mix of close-before-wait and close-during-wait.
		if i%2 == 0 {
			time.Sleep(time.Millisecond)
		}

		pd.close()

		select {
		case err := <-errCh:
			if err == nil {
				t.Fatalf("iteration %d: wait() returned nil after close", i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: deadlock in rapid open/close cycle", i)
		}
		cleanup()
	}
}

// TestAdversarialMultipleWaitersReadWrite tests simultaneous read and write
// waiters. Both must unblock on close().
func TestAdversarialMultipleWaitersReadWrite(t *testing.T) {
	pd, cleanup := newTestPollDesc(t)
	defer cleanup()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		_ = pd.wait(ModeRead)
	}()
	go func() {
		defer wg.Done()
		_ = pd.wait(ModeWrite)
	}()

	time.Sleep(50 * time.Millisecond)
	pd.close()

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Log("PASS: both read and write waiters unblocked after close")
	case <-time.After(5 * time.Second):
		t.Fatal("FAIL: read+write waiters deadlocked after close")
	}
}

// TestAdversarialCloseAndUnblockRace tests close() racing with unblock()
// from the epoll loop. Both try to send on the same buffered channel.
// Neither should block (default branch), and the waiter must still exit.
func TestAdversarialCloseAndUnblockRace(t *testing.T) {
	const iterations = 200

	for i := 0; i < iterations; i++ {
		pd, cleanup := newTestPollDesc(t)

		errCh := make(chan error, 1)
		go func() {
			errCh <- pd.wait(ModeRead)
		}()

		// Wait for pollWait state.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if atomic.LoadInt32(&pd.rdState) == pollWait {
				break
			}
		}

		// Fire unblock and close concurrently.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			pd.unblock(ModeRead, false, true)
		}()
		go func() {
			defer wg.Done()
			pd.close()
		}()
		wg.Wait()

		select {
		case err := <-errCh:
			// Either nil (unblock won the race) or SrtSocketClosed (close won).
			// Both are acceptable.
			_ = err
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: deadlock -- wait() not unblocked after concurrent close+unblock", i)
		}
		cleanup()
	}
}

// TestAdversarialWaitAfterCloseRepeated calls wait() many times on a closed
// pollDesc. Each call must return SrtSocketClosed immediately, never block.
func TestAdversarialWaitAfterCloseRepeated(t *testing.T) {
	pd, cleanup := newTestPollDesc(t)
	defer cleanup()

	pd.close()

	for i := 0; i < 100; i++ {
		done := make(chan error, 1)
		go func() {
			done <- pd.wait(ModeRead)
		}()

		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("iteration %d: wait on closed pd returned nil", i)
			}
			if _, ok := err.(*SrtSocketClosed); !ok {
				t.Fatalf("iteration %d: unexpected error %T: %v", i, err, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: wait on closed pd blocked", i)
		}
	}
}

