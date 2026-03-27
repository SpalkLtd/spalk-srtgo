package srtgo

import (
	"sync"
	"testing"
	"time"
)

// newTestPollDesc creates a non-blocking SRT socket and returns its pollDesc.
// The caller must call cleanup() when done.
func newTestPollDesc(t *testing.T) (pd *pollDesc, cleanup func()) {
	t.Helper()
	sock, err := NewSrtSocket("127.0.0.1", 0, map[string]string{})
	if err != nil {
		t.Fatalf("failed to create SRT socket: %v", err)
	}
	if sock == nil {
		t.Fatal("NewSrtSocket returned nil")
	}
	if sock.blocking {
		t.Fatal("expected non-blocking socket")
	}
	return sock.pd, func() { sock.Close() }
}

// TestPollDescCloseUnblocksReadWait proves that pd.close() must unblock a
// goroutine stuck in pd.wait(ModeRead). Currently FAILS — close() sets the
// closing flag but never signals the unblockRd channel.
func TestPollDescCloseUnblocksReadWait(t *testing.T) {
	pd, cleanup := newTestPollDesc(t)
	defer cleanup()

	// Put the pollDesc into the waiting state from a goroutine.
	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		// This will block in the select{} inside wait() because:
		//   - no data arrives (unblockRd never receives)
		//   - no deadline is set (expiryChan never fires)
		// The ONLY way out should be pd.close() signalling us.
		err := pd.wait(ModeRead)
		errCh <- err
	}()

	// Give the goroutine time to enter the select in wait().
	time.Sleep(50 * time.Millisecond)

	// Close the pollDesc — this SHOULD unblock the waiter.
	pd.close()

	// Wait for the goroutine with a generous timeout.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Goroutine exited — check it returned an error.
		err := <-errCh
		if err == nil {
			t.Fatal("wait() returned nil error after close; expected SrtSocketClosed")
		}
		if _, ok := err.(*SrtSocketClosed); !ok {
			t.Fatalf("wait() returned unexpected error type %T: %v", err, err)
		}
		t.Logf("PASS: wait(ModeRead) unblocked with: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("FAIL: pd.close() did not unblock pd.wait(ModeRead) within 2s — goroutine is leaked")
	}
}

// TestPollDescCloseUnblocksWriteWait is the same test for ModeWrite.
// Connect() blocks on pd.wait(ModeWrite), so this proves that path too.
func TestPollDescCloseUnblocksWriteWait(t *testing.T) {
	pd, cleanup := newTestPollDesc(t)
	defer cleanup()

	var wg sync.WaitGroup
	errCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := pd.wait(ModeWrite)
		errCh <- err
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
		err := <-errCh
		if err == nil {
			t.Fatal("wait() returned nil error after close; expected SrtSocketClosed")
		}
		if _, ok := err.(*SrtSocketClosed); !ok {
			t.Fatalf("wait() returned unexpected error type %T: %v", err, err)
		}
		t.Logf("PASS: wait(ModeWrite) unblocked with: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("FAIL: pd.close() did not unblock pd.wait(ModeWrite) within 2s — goroutine is leaked")
	}
}

// TestPollDescCloseBeforeWaitReturnsError verifies that if close() is
// called BEFORE wait(), the subsequent wait() returns immediately with
// SrtSocketClosed. This is the non-racy path and should already work.
func TestPollDescCloseBeforeWaitReturnsError(t *testing.T) {
	pd, cleanup := newTestPollDesc(t)
	defer cleanup()

	// Close first.
	pd.close()

	// Now wait — should return immediately.
	done := make(chan error, 1)
	go func() {
		done <- pd.wait(ModeRead)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("wait() returned nil after close; expected SrtSocketClosed")
		}
		if _, ok := err.(*SrtSocketClosed); !ok {
			t.Fatalf("unexpected error type %T: %v", err, err)
		}
		t.Logf("PASS: wait(ModeRead) returned immediately after prior close: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("FAIL: wait(ModeRead) blocked even though close() was called first")
	}
}

// listenOnRandomPort tries up to 10 random ports to bind a listener.
// Caller must have called InitSRT() first.
func listenOnRandomPort(t *testing.T, opts map[string]string) (*SrtSocket, uint16) {
	t.Helper()
	for i := 0; i < 10; i++ {
		port := randomPort()
		listener, err := NewSrtSocket("127.0.0.1", port, opts)
		if err != nil || listener == nil {
			continue
		}
		if err := listener.Listen(1); err != nil {
			listener.Close()
			continue
		}
		return listener, port
	}
	t.Fatal("failed to bind listener after 10 attempts")
	return nil, 0
}

// TestSocketCloseUnblocksReadPacket is a higher-level integration test.
// It sets up a real SRT connection, then closes the reader while ReadPacket
// is blocked waiting for data. ReadPacket should return an error promptly.
func TestSocketCloseUnblocksReadPacket(t *testing.T) {
	InitSRT()
	listenerOpts := map[string]string{"transtype": "file", "latency": "300", "mode": "listener"}
	callerOpts := map[string]string{"transtype": "file", "latency": "300", "mode": "caller"}

	listener, port := listenOnRandomPort(t, listenerOpts)
	defer listener.Close()

	// Connect a caller in a goroutine.
	callerReady := make(chan *SrtSocket, 1)
	go func() {
		caller, _ := NewSrtSocket("127.0.0.1", port, callerOpts)
		if caller == nil {
			callerReady <- nil
			return
		}
		if err := caller.Connect(); err != nil {
			caller.Close()
			callerReady <- nil
			return
		}
		callerReady <- caller
	}()

	// Accept the connection on the listener side.
	accepted, _, err := listener.Accept()
	if err != nil || accepted == nil {
		t.Fatalf("accept failed: %v", err)
	}

	caller := <-callerReady
	if caller == nil {
		t.Fatal("caller failed to connect")
	}
	defer caller.Close()

	// Now block ReadPacket on the accepted socket — no data is being sent,
	// so it will block in pd.wait(ModeRead) forever.
	readDone := make(chan error, 1)
	go func() {
		pkt := &SrtPacket{Buffer: make([]byte, 1316)}
		_, err := accepted.ReadPacket(pkt)
		readDone <- err
	}()

	// Give ReadPacket time to enter the blocking wait.
	time.Sleep(100 * time.Millisecond)

	// Close the accepted socket — this should unblock ReadPacket.
	accepted.Close()

	select {
	case err := <-readDone:
		// ReadPacket returned — any error is fine, we just need it to not hang.
		t.Logf("PASS: ReadPacket unblocked after Close() with: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("FAIL: Close() did not unblock ReadPacket() within 2s — goroutine is leaked")
	}
}
