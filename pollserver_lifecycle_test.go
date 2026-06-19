package srtgo

import (
	"sync"
	"testing"
	"time"
)

// pollServerStopped returns true if the pollServer's run() goroutine has
// exited within the given timeout.
func pollServerStopped(timeout time.Duration) bool {
	phMu.Lock()
	ps := phctx
	phMu.Unlock()
	if ps == nil {
		return true
	}
	select {
	case <-ps.stopped:
		return true
	case <-time.After(timeout):
		return false
	}
}

// pollServerRunning returns true if phctx is non-nil and its run()
// goroutine has NOT exited.
func pollServerRunning() bool {
	phMu.Lock()
	ps := phctx
	phMu.Unlock()
	if ps == nil {
		return false
	}
	select {
	case <-ps.stopped:
		return false
	default:
		return true
	}
}

// TestPollServerStartsOnFirstSocket verifies that creating a non-blocking
// SRT socket causes the pollServer singleton to start its run() goroutine.
func TestPollServerStartsOnFirstSocket(t *testing.T) {
	InitSRT()
	defer CleanupSRT()

	sock, err := NewSrtSocket("127.0.0.1", 0, map[string]string{})
	if err != nil {
		t.Fatalf("failed to create SRT socket: %v", err)
	}
	defer sock.Close()

	if phctx == nil {
		t.Fatal("phctx is nil after creating a socket — pollServer was not started")
	}
	if !pollServerRunning() {
		t.Fatal("pollServer run() goroutine is not running after socket creation")
	}
	t.Log("PASS: pollServer started on first socket creation")
}

// TestPollServerStopsWhenLastSocketCloses verifies that closing the only
// open socket causes the pollServer's run() goroutine to exit cleanly.
func TestPollServerStopsWhenLastSocketCloses(t *testing.T) {
	InitSRT()
	defer CleanupSRT()

	sock, err := NewSrtSocket("127.0.0.1", 0, map[string]string{})
	if err != nil {
		t.Fatalf("failed to create SRT socket: %v", err)
	}

	// Verify running before close.
	if !pollServerRunning() {
		t.Fatal("pollServer should be running after socket creation")
	}

	sock.Close()

	// The run() goroutine uses a 100ms epoll timeout, so give it time to
	// notice the stop signal and exit.
	if !pollServerStopped(2 * time.Second) {
		t.Fatal("FAIL: pollServer run() goroutine did not stop within 2s after last socket closed")
	}
	t.Log("PASS: pollServer stopped after last socket closed")
}

// TestPollServerRestartsAfterStop verifies that after the pollServer stops
// (last socket closed), creating a new socket restarts it, and the new
// socket works normally.
func TestPollServerRestartsAfterStop(t *testing.T) {
	InitSRT()
	defer CleanupSRT()

	// Phase 1: create and close socket A — pollServer should stop.
	sockA, err := NewSrtSocket("127.0.0.1", 0, map[string]string{})
	if err != nil {
		t.Fatalf("failed to create socket A: %v", err)
	}
	sockA.Close()

	if !pollServerStopped(2 * time.Second) {
		t.Fatal("pollServer did not stop after closing socket A")
	}
	t.Log("Phase 1 OK: pollServer stopped after socket A closed")

	// Phase 2: create socket B — pollServer should restart.
	sockB, err := NewSrtSocket("127.0.0.1", 0, map[string]string{})
	if err != nil {
		t.Fatalf("failed to create socket B: %v", err)
	}

	if !pollServerRunning() {
		t.Fatal("pollServer did not restart when socket B was created")
	}
	t.Log("Phase 2 OK: pollServer restarted for socket B")

	// Phase 3: close socket B — pollServer should stop again.
	sockB.Close()

	if !pollServerStopped(2 * time.Second) {
		t.Fatal("pollServer did not stop after closing socket B")
	}
	t.Log("Phase 3 OK: pollServer stopped after socket B closed — full restart cycle works")
}

// TestPollServerMultipleSocketsRefCounting verifies that the pollServer
// stays running as long as at least one socket is open, and stops only
// when the very last socket is closed.
func TestPollServerMultipleSocketsRefCounting(t *testing.T) {
	InitSRT()
	defer CleanupSRT()

	sockets := make([]*SrtSocket, 3)
	for i := range sockets {
		s, err := NewSrtSocket("127.0.0.1", 0, map[string]string{})
		if err != nil {
			t.Fatalf("failed to create socket %d: %v", i, err)
		}
		sockets[i] = s
	}

	// Close first two — pollServer must still be running.
	sockets[0].Close()
	sockets[1].Close()

	// Give a brief moment for any incorrect shutdown to propagate.
	time.Sleep(200 * time.Millisecond)

	if !pollServerRunning() {
		t.Fatal("pollServer stopped prematurely — still have one socket open")
	}
	t.Log("OK: pollServer still running with 1 of 3 sockets remaining")

	// Close the last socket — pollServer should stop.
	sockets[2].Close()

	if !pollServerStopped(2 * time.Second) {
		t.Fatal("pollServer did not stop after last socket closed")
	}
	t.Log("PASS: pollServer stopped when last of 3 sockets closed — ref counting works")
}

// TestPollServerConcurrentCreateClose spawns N goroutines that each create
// and close a socket. After all goroutines finish, the pollServer must have
// stopped cleanly. Run with -race to verify no data races.
func TestPollServerConcurrentCreateClose(t *testing.T) {
	InitSRT()
	defer CleanupSRT()

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)

	errCh := make(chan error, N)

	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			s, err := NewSrtSocket("127.0.0.1", 0, map[string]string{})
			if err != nil {
				errCh <- err
				return
			}
			// Brief hold to increase overlap.
			time.Sleep(5 * time.Millisecond)
			s.Close()
		}()
	}

	// Wait for all goroutines with a timeout.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// All goroutines finished.
	case <-time.After(10 * time.Second):
		t.Fatal("FAIL: concurrent create/close did not complete within 10s — possible deadlock")
	}

	// Check for socket creation errors.
	close(errCh)
	for err := range errCh {
		t.Errorf("socket creation error: %v", err)
	}

	// After all sockets are closed, pollServer must stop.
	if !pollServerStopped(2 * time.Second) {
		t.Fatal("pollServer did not stop after all concurrent sockets closed")
	}
	t.Log("PASS: concurrent create/close completed cleanly, pollServer stopped")
}

// TestPollServerStopsCleanlyDuringActiveIO creates a socket, starts a
// goroutine blocked in pd.wait(ModeRead), then closes the socket. Both
// the wait() must unblock AND the pollServer must stop.
func TestPollServerStopsCleanlyDuringActiveIO(t *testing.T) {
	InitSRT()
	defer CleanupSRT()

	sock, err := NewSrtSocket("127.0.0.1", 0, map[string]string{})
	if err != nil {
		t.Fatalf("failed to create socket: %v", err)
	}

	// Block a goroutine in pd.wait(ModeRead).
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- sock.pd.wait(ModeRead)
	}()

	// Give the goroutine time to enter the blocking select.
	time.Sleep(50 * time.Millisecond)

	// Close the socket — should unblock wait() AND eventually stop pollServer.
	sock.Close()

	// Verify wait() unblocked.
	select {
	case err := <-waitDone:
		if err == nil {
			t.Fatal("wait() returned nil after close; expected SrtSocketClosed")
		}
		t.Logf("wait(ModeRead) unblocked with: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("FAIL: wait(ModeRead) did not unblock within 2s after socket close")
	}

	// Verify pollServer stopped.
	if !pollServerStopped(2 * time.Second) {
		t.Fatal("FAIL: pollServer did not stop after socket with active IO was closed")
	}
	t.Log("PASS: active IO unblocked and pollServer stopped cleanly")
}

// TestCleanupSRTStopsPollServer verifies that CleanupSRT() stops the
// pollServer, and that a subsequent InitSRT() + socket creation works.
func TestCleanupSRTStopsPollServer(t *testing.T) {
	InitSRT()

	sock, err := NewSrtSocket("127.0.0.1", 0, map[string]string{})
	if err != nil {
		t.Fatalf("failed to create socket: %v", err)
	}
	sock.Close()

	// Wait for auto-stop first so we're testing CleanupSRT on a stopped server.
	if !pollServerStopped(2 * time.Second) {
		t.Fatal("pollServer did not auto-stop before CleanupSRT")
	}

	CleanupSRT()

	// Now reinitialize and verify everything works from scratch.
	InitSRT()
	defer CleanupSRT()

	sock2, err := NewSrtSocket("127.0.0.1", 0, map[string]string{})
	if err != nil {
		t.Fatalf("failed to create socket after reinit: %v", err)
	}
	defer sock2.Close()

	if !pollServerRunning() {
		t.Fatal("pollServer not running after InitSRT + new socket")
	}
	t.Log("PASS: CleanupSRT + reinit cycle works correctly")
}
