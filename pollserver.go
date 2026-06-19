package srtgo

/*
#cgo LDFLAGS: -lsrt
#include <srt/srt.h>
*/
import "C"

import (
	"sync"
	"unsafe"
)

var (
	phctx *pollServer
	phMu  sync.Mutex // protects phctx and lifecycle
)

// pollServerCtx returns the singleton pollServer, starting it if necessary.
// It increments the reference count — the caller must eventually call decref()
// (via pollClose) to allow shutdown.
func pollServerCtx() *pollServer {
	phMu.Lock()
	defer phMu.Unlock()
	if phctx == nil {
		eid := C.srt_epoll_create()
		C.srt_epoll_set(eid, C.SRT_EPOLL_ENABLE_EMPTY)
		phctx = &pollServer{
			srtEpollDescr: eid,
			pollDescs:     make(map[C.SRTSOCKET]*pollDesc),
			stopCh:        make(chan struct{}),
			stopped:       make(chan struct{}),
		}
		go phctx.run()
	}
	phctx.refs++
	return phctx
}

// pollServerStop stops the pollServer if it is running and waits for the
// run() goroutine to exit. Must be called with phMu held. The lock is held
// throughout — run() does not need phMu so there is no deadlock.
func pollServerStop() {
	if phctx == nil {
		return
	}
	select {
	case <-phctx.stopCh:
		// Already signalled (e.g. concurrent CleanupSRT + last socket close).
		// Just wait for run() to finish.
	default:
		close(phctx.stopCh)
	}
	// Wait for run() to exit. run() never acquires phMu, so this is safe
	// to do while holding the lock.
	<-phctx.stopped
	phctx = nil
}

type pollServer struct {
	srtEpollDescr C.int
	pollDescLock  sync.Mutex
	pollDescs     map[C.SRTSOCKET]*pollDesc
	refs          int
	stopCh        chan struct{}
	stopped       chan struct{}
}

func (p *pollServer) pollOpen(pd *pollDesc) {
	//use uint because otherwise with ET it would overflow :/ (srt should accept an uint instead, or fix it's SRT_EPOLL_ET definition)
	events := C.uint(C.SRT_EPOLL_IN | C.SRT_EPOLL_OUT | C.SRT_EPOLL_ERR | C.SRT_EPOLL_ET)
	//via unsafe.Pointer because we cannot cast *C.uint to *C.int directly
	//block poller
	p.pollDescLock.Lock()
	ret := C.srt_epoll_add_usock(p.srtEpollDescr, pd.fd, (*C.int)(unsafe.Pointer(&events)))
	if ret == -1 {
		panic("ERROR ADDING FD TO EPOLL")
	}
	p.pollDescs[pd.fd] = pd
	p.pollDescLock.Unlock()
}

func (p *pollServer) pollClose(pd *pollDesc) {
	sockstate := C.srt_getsockstate(pd.fd)
	//Broken/closed sockets get removed internally by SRT lib
	if sockstate == C.SRTS_BROKEN || sockstate == C.SRTS_CLOSING || sockstate == C.SRTS_CLOSED || sockstate == C.SRTS_NONEXIST {
		// Still need to clean up our map and decrement ref count
		p.pollDescLock.Lock()
		delete(p.pollDescs, pd.fd)
		p.pollDescLock.Unlock()
		p.decref()
		return
	}
	ret := C.srt_epoll_remove_usock(p.srtEpollDescr, pd.fd)
	if ret == -1 {
		panic("ERROR REMOVING FD FROM EPOLL")
	}
	p.pollDescLock.Lock()
	delete(p.pollDescs, pd.fd)
	p.pollDescLock.Unlock()
	p.decref()
}

// decref decrements the reference count and triggers shutdown when it hits zero.
func (p *pollServer) decref() {
	phMu.Lock()
	defer phMu.Unlock()
	p.refs--
	if p.refs <= 0 {
		pollServerStop()
	}
}

func (p *pollServer) run() {
	defer close(p.stopped)
	timeoutMs := C.int64_t(100) // 100ms so we can check stopCh periodically
	fds := [128]C.SRT_EPOLL_EVENT{}
	fdlen := C.int(128)
	for {
		select {
		case <-p.stopCh:
			C.srt_epoll_release(p.srtEpollDescr)
			return
		default:
		}
		res := C.srt_epoll_uwait(p.srtEpollDescr, &fds[0], fdlen, timeoutMs)
		if res == 0 {
			continue // timeout, no events
		} else if res == -1 {
			// During shutdown the epoll descriptor may have been released.
			select {
			case <-p.stopCh:
				return
			default:
			}
			// Genuine error outside of shutdown
			panic("srt_epoll_error")
		} else if res > 0 {
			max := int(res)
			if fdlen < res {
				max = int(fdlen)
			}
			p.pollDescLock.Lock()
			for i := 0; i < max; i++ {
				s := fds[i].fd
				events := fds[i].events

				pd := p.pollDescs[s]
				if pd == nil {
					continue // socket already removed
				}
				if events&C.SRT_EPOLL_ERR != 0 {
					pd.unblock(ModeRead, true, false)
					pd.unblock(ModeWrite, true, false)
					continue
				}
				if events&C.SRT_EPOLL_IN != 0 {
					pd.unblock(ModeRead, false, true)
				}
				if events&C.SRT_EPOLL_OUT != 0 {
					pd.unblock(ModeWrite, false, true)
				}
			}
			p.pollDescLock.Unlock()
		}
	}
}
