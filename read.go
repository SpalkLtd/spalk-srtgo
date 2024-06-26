package srtgo

/*
#cgo LDFLAGS: -lsrt
#include <srt/srt.h>

int srt_recvmsg2_wrapped(SRTSOCKET u, char* buf, int len, SRT_MSGCTRL *mctrl, int *srterror, int *syserror)
{
	int ret = srt_recvmsg2(u, buf, len, mctrl);
	if (ret < 0) {
		*srterror = srt_getlasterror(syserror);
	}
	return ret;
}

*/
import "C"
import (
	"errors"
	"syscall"
	"unsafe"
)

func srtRecvMsg2Impl(u C.SRTSOCKET, buf []byte, msgctrl *C.SRT_MSGCTRL) (n int, err error) {
	srterr := C.int(0)
	syserr := C.int(0)
	n = int(C.srt_recvmsg2_wrapped(u, (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)), msgctrl, &srterr, &syserr))
	if n < 0 {
		srterror := SRTErrno(srterr)
		if syserr < 0 {
			srterror.wrapSysErr(syscall.Errno(syserr))
		}
		err = srterror
		n = 0
	}
	return
}

// Read data from the SRT socket
func (s SrtSocket) Read(b []byte) (n int, err error) {
	//Fastpath
	if !s.blocking {
		s.pd.reset(ModeRead)
	}
	n, err = srtRecvMsg2Impl(s.socket, b, nil)

	for {
		if !errors.Is(err, error(EAsyncRCV)) || s.blocking {
			return
		}
		s.pd.wait(ModeRead)
		n, err = srtRecvMsg2Impl(s.socket, b, nil)
	}
}

// Read data from the SRT socket and put into the provided struct (reduces allocations)

type SrtPacket struct {
	Buffer  []byte
	Srctime int64 // [OUT] timestamp set for this dataset when sending
	Pktseq  int32 // [OUT] packet sequence number (first packet from the message, if it spans multiple UDP packets)
	Msgno   int32 // [OUT] message number assigned to the currently received message
}

func (s SrtSocket) ReadPacket(packet *SrtPacket) (n int, err error) {
	// Fast path
	if !s.blocking {
		s.pd.reset(ModeRead)
	}

	var msgctrl C.SRT_MSGCTRL
	C.srt_msgctrl_init((*C.SRT_MSGCTRL)(unsafe.Pointer(&msgctrl)))

	n, err = srtRecvMsg2Impl(s.socket, packet.Buffer, (*C.SRT_MSGCTRL)(unsafe.Pointer(&msgctrl)))

	for {
		if !errors.Is(err, error(EAsyncRCV)) || s.blocking {
			packet.Pktseq = int32(msgctrl.pktseq)
			packet.Msgno = int32(msgctrl.msgno)
			packet.Srctime = int64(msgctrl.srctime)
			return
		}
		s.pd.wait(ModeRead)
		n, err = srtRecvMsg2Impl(s.socket, packet.Buffer, (*C.SRT_MSGCTRL)(unsafe.Pointer(&msgctrl)))
	}
}
