package srtgo

/*
#cgo LDFLAGS: -lsrt
#include <srt/srt.h>
*/
import "C"
import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	pollDefault = int32(iota)
	pollReady   = int32(iota)
	pollWait    = int32(iota)
)

type PollMode int

// Issue FOUR
// The logic in setDeadline needs both ModeWrite and ModeRead to be non-zero.
// E.G., Previously, setDeadline(modeWrite) is equivalent to setDeadline(modeRead + modeWrite)
const (
	Blah = PollMode(iota)
	ModeWrite
	ModeRead
)

/*
pollDesc contains the polling state for the associated SrtSocket
closing: socket is closing, reject all poll operations
pollErr: an error occured on the socket, indicates it's not useable anymore.
unblockRd: is used to unblock the poller when the socket becomes ready for io
rdState: polling state for read operations
rdDeadline: deadline in NS before poll operation times out, -1 means timedout (needs to be cleared), 0 is without timeout
rdSeq: sequence number protects against spurious signalling of timeouts when timer is reset.
rdTimer: timer used to enforce deadline.
*/
type pollDesc struct {
	lock       sync.Mutex
	closing    bool
	fd         C.SRTSOCKET
	pollErr    bool
	unblockRd  chan interface{}
	rdState    int32
	rdLock     sync.Mutex
	rdDeadline int64
	rdSeq      int64
	rdTimer    *time.Timer
	rtSeq      int64
	unblockWr  chan interface{}
	wrState    int32
	wrLock     sync.Mutex
	wdDeadline int64
	wdSeq      int64
	wdTimer    *time.Timer
	wtSeq      int64
	pollS      *pollServer
}

var pdPool = sync.Pool{
	New: func() interface{} {
		return &pollDesc{
			unblockRd: make(chan interface{}, 1),
			unblockWr: make(chan interface{}, 1),
			// Issue ONE
			// These timers, which are how the deadline stuff works, fire instantly because they were time.NewTimer(0).
			// A hack fix is below.
			rdTimer: time.NewTimer(time.Duration(time.Hour * 1)), // hack fix
			wdTimer: time.NewTimer(time.Duration(time.Hour * 1)), // hack fix
		}
	},
}

func pollDescInit(s C.SRTSOCKET) *pollDesc {
	pd := pdPool.Get().(*pollDesc)
	pd.lock.Lock()
	defer pd.lock.Unlock()
	pd.fd = s
	pd.rdState = pollDefault
	pd.wrState = pollDefault
	pd.pollS = pollServerCtx()
	pd.closing = false
	pd.pollErr = false
	pd.rdSeq++
	pd.wdSeq++
	pd.pollS.pollOpen(pd)
	return pd
}

func (pd *pollDesc) release() {
	pd.lock.Lock()
	defer pd.lock.Unlock()
	if !pd.closing || pd.rdState == pollWait || pd.wrState == pollWait {
		panic("returning open or blocked upon pollDesc")
	}
	pd.fd = 0
	pdPool.Put(pd)
}

func (pd *pollDesc) wait(mode PollMode) error {
	fmt.Println("[nathan debug] wait start")
	defer pd.reset(mode)
	if err := pd.checkPollErr(mode); err != nil {
		return err
	}
	state := &pd.rdState
	unblockChan := pd.unblockRd
	expiryChan := pd.rdTimer.C
	timerSeq := int64(0)
	pd.lock.Lock()
	if mode == ModeRead {
		fmt.Println("[nathan debug] setting up stuff for read mode")
		timerSeq = pd.rtSeq
		pd.rdLock.Lock()
		defer pd.rdLock.Unlock()
	} else if mode == ModeWrite {
		fmt.Println("[nathan debug] setting up stuff for write mode")
		timerSeq = pd.wtSeq
		state = &pd.wrState
		unblockChan = pd.unblockWr
		expiryChan = pd.wdTimer.C
		pd.wrLock.Lock()
		defer pd.wrLock.Unlock()
	}

	for {
		old := *state
		if old == pollReady {
			*state = pollDefault
			pd.lock.Unlock()
			return nil
		}
		if atomic.CompareAndSwapInt32(state, pollDefault, pollWait) {
			break
		}
	}
	pd.lock.Unlock()

wait:
	for {
		select {
		case <-unblockChan:
			fmt.Println("[nathan debug] unblock chan written to")
			break wait
		case <-expiryChan:
			fmt.Println("[nathan debug] expiry chan written to")
			pd.lock.Lock()
			if mode == ModeRead {
				if timerSeq == pd.rdSeq {
					pd.rdDeadline = -1
					pd.lock.Unlock()
					break wait
				}
				timerSeq = pd.rtSeq
			}
			if mode == ModeWrite {
				if timerSeq == pd.wdSeq {
					pd.wdDeadline = -1
					pd.lock.Unlock()
					break wait
				}
				timerSeq = pd.wtSeq
			}
			pd.lock.Unlock()
		}
	}
	err := pd.checkPollErr(mode)
	fmt.Println("[nathan debug] wait end")
	return err
}

func (pd *pollDesc) close() {
	pd.lock.Lock()
	defer pd.lock.Unlock()
	if pd.closing {
		return
	}
	pd.closing = true
	pd.pollS.pollClose(pd)
}

func (pd *pollDesc) checkPollErr(mode PollMode) error {
	pd.lock.Lock()
	defer pd.lock.Unlock()
	if pd.closing {
		return &SrtSocketClosed{}
	}

	if mode == ModeRead && pd.rdDeadline < 0 || mode == ModeWrite && pd.wdDeadline < 0 {
		return &SrtEpollTimeout{}
	}

	if pd.pollErr {
		return &SrtSocketClosed{}
	}

	return nil
}

func (pd *pollDesc) setDeadline(t time.Time, mode PollMode) {
	pd.lock.Lock()
	defer pd.lock.Unlock()
	var d int64
	if !t.IsZero() {
		d = int64(time.Until(t))
		if d == 0 {
			d = -1
		}
	}
	if mode == ModeRead || mode == ModeRead+ModeWrite {
		pd.rdSeq++
		pd.rtSeq = pd.rdSeq
		if pd.rdDeadline > 0 {
			pd.rdTimer.Stop()
		}
		pd.rdDeadline = d
		if d > 0 {
			timeResetReturnValue := pd.rdTimer.Reset(time.Duration(d))
			fmt.Printf("[nathan debug] finished setting up read deadline timer with duration: %.2f seconds. Timer was previously active: %t \n", time.Duration(d).Seconds(), timeResetReturnValue)
		}
		if d < 0 {
			fmt.Println("[nathan debug] read deadline is in past, so setting read to unblock")
			pd.unblock(ModeRead, false, false)
		}
	}
	if mode == ModeWrite || mode == ModeRead+ModeWrite {
		pd.wdSeq++
		pd.wtSeq = pd.wdSeq
		if pd.wdDeadline > 0 {
			pd.wdTimer.Stop()
		}
		pd.wdDeadline = d
		if d > 0 {
			timeResetReturnValue := pd.wdTimer.Reset(time.Duration(d))
			fmt.Printf("[nathan debug] finished setting up write deadline timer with duration: %.2f seconds. Timer was previously active: %t \n", time.Duration(d).Seconds(), timeResetReturnValue)
		}
		if d < 0 {
			fmt.Println("[nathan debug] write deadline is in past, so setting write to unblock")
			pd.unblock(ModeWrite, false, false)
		}
	}
}

func (pd *pollDesc) unblock(mode PollMode, pollerr, ioready bool) {
	if pollerr {
		pd.lock.Lock()
		pd.pollErr = pollerr
		pd.lock.Unlock()
	}
	state := &pd.rdState
	unblockChan := pd.unblockRd
	if mode == ModeWrite {
		state = &pd.wrState
		unblockChan = pd.unblockWr
	}
	pd.lock.Lock()
	old := atomic.LoadInt32(state)
	if ioready {
		atomic.StoreInt32(state, pollReady)
	}
	pd.lock.Unlock()
	if old == pollWait {
		//make sure we never block here
		select {
		case unblockChan <- struct{}{}:
			//
		default:
			//
		}
	}
}

func (pd *pollDesc) reset(mode PollMode) {
	if mode == ModeRead {
		pd.rdLock.Lock()
		pd.rdState = pollDefault
		pd.rdLock.Unlock()
	} else if mode == ModeWrite {
		pd.wrLock.Lock()
		pd.wrState = pollDefault
		pd.wrLock.Unlock()
	}
}
