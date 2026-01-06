package ports

import (
	"errors"
	"fmt"
	"net"
	"sync"
)

// Triple holds the three ports we need for each call.
// We used to have issues where SIP and media used the same port, which caused problems.
// Now we make sure they're all different.
type Triple struct {
	SipPort     int // Port for SIP messages
	MediaPort   int // Port for audio/RTP (different from SIP port)
	ControlPort int // Port for controlling SIPp
}

var (
	ErrNoPorts           = errors.New("no free ports available")
	ErrInvalidPortRanges = errors.New("invalid port ranges")
)

// Allocator helps us manage port allocation for calls.
// Each call gets three different ports to avoid conflicts.
// We also check if ports are actually free on the system before using them.
type Allocator struct {
	sipStart, sipEnd   int
	ctrlStart, ctrlEnd int

	mu         sync.Mutex
	sipCursor  int
	ctrlCursor int
	allocated  map[int]struct{} // tracks all allocated ports (sip, media, ctrl) together
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

// Acquire finds three free ports for a new call.
// We make sure the media port is different from the SIP port to avoid conflicts.
func (a *Allocator) Acquire() (Triple, error) {
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

		// Find media port (must be different from sip port)
		media, ok := a.findMediaPortLocked(sip)
		if !ok {
			continue
		}

		ctrl, ok := a.findControlPortLocked()
		if !ok {
			return Triple{}, ErrNoPorts
		}

		a.allocated[sip] = struct{}{}
		a.allocated[media] = struct{}{}
		a.allocated[ctrl] = struct{}{}
		return Triple{SipPort: sip, MediaPort: media, ControlPort: ctrl}, nil
	}

	return Triple{}, ErrNoPorts
}

func (a *Allocator) Release(t Triple) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.allocated, t.SipPort)
	delete(a.allocated, t.MediaPort)
	delete(a.allocated, t.ControlPort)
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

// findMediaPortLocked finds a media port that's different from the SIP port.
// This helps avoid conflicts that can cause calls to fail silently.
func (a *Allocator) findMediaPortLocked(sipPort int) (int, bool) {
	maxAttempts := a.sipEnd - a.sipStart + 1
	for i := 0; i < maxAttempts; i++ {
		media := a.nextSip()
		if media == sipPort {
			continue // media port must be different from sip port
		}
		if _, ok := a.allocated[media]; ok {
			continue
		}
		if isUdpPortInUse(media) {
			continue
		}
		return media, true
	}
	return 0, false
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
