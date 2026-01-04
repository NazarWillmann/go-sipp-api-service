package ports

import (
	"errors"
	"fmt"
	"net"
	"sync"
)

// Pair represents allocated ports for one SIPp instance.
type Pair struct {
	SipPort     int
	ControlPort int
}

var (
	ErrNoPorts           = errors.New("no free ports available")
	ErrInvalidPortRanges = errors.New("invalid port ranges")
)

// Allocator manages allocation of SIP and control ports within configured ranges.
// It also checks OS-level availability to avoid collisions with other processes.
type Allocator struct {
	sipStart, sipEnd   int
	ctrlStart, ctrlEnd int

	mu         sync.Mutex
	sipCursor  int
	ctrlCursor int
	allocated  map[int]struct{} // both sip and ctrl ports tracked together
}

func NewAllocator(sipStart, sipEnd, ctrlStart, ctrlEnd int) (*Allocator, error) {
	if sipStart <= 0 || sipEnd <= 0 || ctrlStart <= 0 || ctrlEnd <= 0 {
		return nil, ErrInvalidPortRanges
	}
	if sipStart > sipEnd || ctrlStart > ctrlEnd {
		return nil, ErrInvalidPortRanges
	}
	// ranges must not overlap
	if rangesOverlap(sipStart, sipEnd, ctrlStart, ctrlEnd) {
		return nil, fmt.Errorf("%w: SIP and CONTROL ranges overlap", ErrInvalidPortRanges)
	}

	return &Allocator{
		sipStart: sipStart, sipEnd: sipEnd,
		ctrlStart: ctrlStart, ctrlEnd: ctrlEnd,
		sipCursor: sipStart, ctrlCursor: ctrlStart,
		allocated: make(map[int]struct{}),
	}, nil
}

func rangesOverlap(aStart, aEnd, bStart, bEnd int) bool {
	return aStart <= bEnd && bStart <= aEnd
}

func (a *Allocator) Acquire() (Pair, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	maxAttempts := a.sipEnd - a.sipStart + 1
	for i := 0; i < maxAttempts; i++ {
		sip := a.nextSip()
		if _, ok := a.allocated[sip]; ok {
			continue
		}
		if isUdpPortInUse(sip) {
			continue
		}

		ctrl, ok := a.findControlPortLocked()
		if !ok {
			return Pair{}, ErrNoPorts
		}

		a.allocated[sip] = struct{}{}
		a.allocated[ctrl] = struct{}{}
		return Pair{SipPort: sip, ControlPort: ctrl}, nil
	}

	return Pair{}, ErrNoPorts
}

func (a *Allocator) Release(p Pair) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.allocated, p.SipPort)
	delete(a.allocated, p.ControlPort)
}

func (a *Allocator) nextSip() int {
	p := a.sipCursor
	a.sipCursor++
	if a.sipCursor > a.sipEnd {
		a.sipCursor = a.sipStart
	}
	return p
}

func (a *Allocator) nextCtrl() int {
	p := a.ctrlCursor
	a.ctrlCursor++
	if a.ctrlCursor > a.ctrlEnd {
		a.ctrlCursor = a.ctrlStart
	}
	return p
}

func (a *Allocator) findControlPortLocked() (int, bool) {
	maxAttempts := a.ctrlEnd - a.ctrlStart + 1
	for i := 0; i < maxAttempts; i++ {
		ctrl := a.nextCtrl()
		if _, ok := a.allocated[ctrl]; ok {
			continue
		}
		if isTcpPortInUse(ctrl) {
			continue
		}
		return ctrl, true
	}
	return 0, false
}

func isTcpPortInUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err == nil {
		_ = ln.Close()
		return false
	}
	return true
}

func isUdpPortInUse(port int) bool {
	pc, err := net.ListenPacket("udp", fmt.Sprintf(":%d", port))
	if err == nil {
		_ = pc.Close()
		return false
	}
	return true
}
