package calls

import (
	"errors"
	"sync"
	"time"
)

// State represents the lifecycle of a call.
type State string

const (
	StateCreated      State = "CREATED"
	StateCalling      State = "CALLING"
	StateRinging      State = "RINGING"
	StateEstablished  State = "ESTABLISHED"
	StateHangup       State = "HANGUP"
	StateDisconnected State = "DISCONNECTED"
	StateFailed       State = "FAILED"
)

func (s State) IsTerminal() bool {
	switch s {
	case StateHangup, StateDisconnected, StateFailed:
		return true
	default:
		return false
	}
}

// CallContext keeps track of everything we know about a call.
type CallContext struct {
	CallID    string    `json:"callId"`
	State     State     `json:"state"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	RemoteHost  string `json:"remoteHost"`  // Where we're sending the call
	RemotePort  int    `json:"remotePort"`  // SIP server port
	Destination string `json:"destination"` // Your own label for tracking
	Service     string `json:"service"`     // The number we're calling
	Scenario    string `json:"scenario"`    // Which SIPp scenario we're using

	SipPort     int `json:"sipPort"`     // Port for SIP messages
	MediaPort   int `json:"mediaPort"`   // Port for audio/RTP
	ControlPort int `json:"controlPort"` // Port for controlling SIPp

	ProcessPid int     `json:"processPid"`
	LastError  *string `json:"lastError,omitempty"`
}

// Registry is an in-memory store of calls.
type Registry struct {
	mu    sync.RWMutex
	calls map[string]*CallContext
}

func NewRegistry() *Registry {
	return &Registry{calls: make(map[string]*CallContext)}
}

func (r *Registry) Add(c *CallContext) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.calls[c.CallID]; ok {
		return errors.New("call already exists")
	}
	r.calls[c.CallID] = c
	return nil
}

func (r *Registry) Get(callID string) (*CallContext, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.calls[callID]
	if !ok {
		return nil, false
	}
	// return a shallow copy to avoid external mutation without Update
	copy := *c
	return &copy, true
}

func (r *Registry) Update(callID string, fn func(c *CallContext)) (*CallContext, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.calls[callID]
	if !ok {
		return nil, false
	}
	fn(c)
	c.UpdatedAt = time.Now().UTC()
	copy := *c
	return &copy, true
}

func (r *Registry) Delete(callID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.calls, callID)
}

func (r *Registry) List() []*CallContext {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*CallContext, 0, len(r.calls))
	for _, c := range r.calls {
		copy := *c
		out = append(out, &copy)
	}
	return out
}

func (r *Registry) CountActive() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cnt := 0
	for _, c := range r.calls {
		if !c.State.IsTerminal() {
			cnt++
		}
	}
	return cnt
}
