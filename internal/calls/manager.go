package calls

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"sipp-service/internal/ports"
	"sipp-service/internal/process"
	"sipp-service/internal/sipp"
)

var (
	ErrNotFound     = errors.New("call not found")
	ErrInvalidState = errors.New("invalid state for operation")
	ErrTooManyCalls = errors.New("max active calls reached")
)

// ManagerConfig contains runtime dependencies for the call manager.
type ManagerConfig struct {
	MaxActiveCalls int
	Sipp           sipp.Config
	Retention      time.Duration // how long to keep terminal calls before cleanup
}

// Manager coordinates call lifecycle and uses external SIPp processes.
type Manager struct {
	cfg    ManagerConfig
	reg    *Registry
	ports  *ports.Allocator
	logger *zap.Logger

	mu   sync.RWMutex
	cmds map[string]*exec.Cmd
}

func NewManager(cfg ManagerConfig, reg *Registry, allocator *ports.Allocator, logger *zap.Logger) *Manager {
	if cfg.Retention == 0 {
		cfg.Retention = 5 * time.Minute
	}
	return &Manager{
		cfg: cfg, reg: reg, ports: allocator, logger: logger,
		cmds: make(map[string]*exec.Cmd),
	}
}

// CreateRequest contains everything we need to start a new call.
// The Service field is required now - it's the actual number you want to call.
// We used to mistakenly use the server IP for this, which didn't work.
type CreateRequest struct {
	RemoteHost  string // Where to send the call (SIP server IP)
	RemotePort  int    // SIP server port (usually 5060)
	Destination string // Optional label for your own tracking
	Service     string // Required: the number to call (like "1234567890")
	Scenario    string // Which SIPp scenario file to use
}

// CreateOutgoing starts a new outgoing call using SIPp.
// We allocate three different ports to avoid conflicts, check that you provided
// a service number, and log all the important details.
// Returns the call info if everything works, or an error if something goes wrong.
func (m *Manager) CreateOutgoing(ctx context.Context, req CreateRequest) (*CallContext, error) {
	if req.RemoteHost == "" || req.RemotePort <= 0 || req.Service == "" || req.Scenario == "" {
		return nil, fmt.Errorf("invalid request")
	}
	if m.cfg.MaxActiveCalls > 0 && m.reg.CountActive() >= m.cfg.MaxActiveCalls {
		return nil, ErrTooManyCalls
	}

	triple, err := m.ports.Acquire()
	if err != nil {
		return nil, err
	}

	callID := uuid.New().String()
	now := time.Now().UTC()

	ctxCall := &CallContext{
		CallID:      callID,
		State:       StateCalling,
		CreatedAt:   now,
		UpdatedAt:   now,
		RemoteHost:  req.RemoteHost,
		RemotePort:  req.RemotePort,
		Destination: req.Destination,
		Service:     req.Service,
		Scenario:    req.Scenario,
		SipPort:     triple.SipPort,
		MediaPort:   triple.MediaPort,
		ControlPort: triple.ControlPort,
		ProcessPid:  0,
		LastError:   nil,
	}

	if err := m.reg.Add(ctxCall); err != nil {
		m.ports.Release(triple)
		return nil, err
	}

	cmd, workDir, err := sipp.StartOutgoing(ctx, m.cfg.Sipp, callID, req.RemoteHost, req.RemotePort, req.Service, req.Scenario, triple.SipPort, triple.MediaPort, triple.ControlPort)
	if err != nil {
		m.reg.Update(callID, func(c *CallContext) {
			es := err.Error()
			c.LastError = &es
			c.State = StateFailed
		})
		m.ports.Release(triple)
		return nil, err
	}

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}

	m.mu.Lock()
	m.cmds[callID] = cmd
	m.mu.Unlock()

	// Start a goroutine to wait for the process to complete and clean up resources
	go func() {
		err := cmd.Wait()

		// Check if call is already in terminal state before updating
		c, ok := m.reg.Get(callID)
		if !ok {
			return // Call was already cleaned up
		}

		// Only update state if not already terminal (idempotent behavior)
		if !c.State.IsTerminal() {
			// Determine final state based on exit status
			finalState := StateDisconnected
			if err != nil {
				finalState = StateFailed
			}

			// Update the call state and capture error if any
			m.finalizeCall(callID, finalState)

			// Set LastError if there was an error
			if err != nil {
				m.reg.Update(callID, func(cc *CallContext) {
					es := err.Error()
					cc.LastError = &es
				})
			}
		}

		// Always release resources regardless of state
		m.ReleaseResources(callID)

		if err != nil {
			m.logger.Warn("sipp process exited with error",
				zap.String("callId", callID),
				zap.Error(err))
		} else {
			m.logger.Info("sipp process completed successfully",
				zap.String("callId", callID))
		}
	}()

	updated, _ := m.reg.Update(callID, func(c *CallContext) {
		c.ProcessPid = pid
		// IMPORTANT: do not lie.
		// We only know the SIPp process started and bound control port; call progress depends on SIP flow.
		c.State = StateCalling
	})

	m.logger.Info(
		"call created",
		zap.String("callId", callID),
		zap.String("remoteHost", req.RemoteHost),
		zap.Int("remotePort", req.RemotePort),
		zap.String("destination", req.Destination),
		zap.String("service", req.Service),
		zap.String("scenarioName", req.Scenario),
		zap.Int("sipPort", triple.SipPort),
		zap.Int("mediaPort", triple.MediaPort),
		zap.Int("controlPort", triple.ControlPort),
		zap.Int("pid", pid),
		zap.Strings("sippCmd", cmd.Args),
		zap.String("workDir", workDir),
	)
	return updated, nil
}

func (m *Manager) SendDTMF(callID string, digits string, interDigitDelayMs int) (*CallContext, error) {
	c, ok := m.reg.Get(callID)
	if !ok {
		return nil, ErrNotFound
	}
	if c.State != StateEstablished {
		return nil, ErrInvalidState
	}

	delay := time.Duration(interDigitDelayMs) * time.Millisecond
	if interDigitDelayMs <= 0 {
		delay = 0
	}
	if err := sipp.SendDTMF(m.cfg.Sipp, c.ControlPort, digits, delay); err != nil {
		updated, _ := m.reg.Update(callID, func(cc *CallContext) {
			es := err.Error()
			cc.LastError = &es
		})
		m.logger.Warn("dtmf failed", zap.String("callId", callID), zap.Error(err))
		return updated, err
	}
	updated, _ := m.reg.Update(callID, func(cc *CallContext) {})
	m.logger.Info("dtmf sent", zap.String("callId", callID), zap.String("digits", digits))
	return updated, nil
}

func (m *Manager) Hangup(callID string, gracefulTimeout time.Duration) (*CallContext, error) {
	c, ok := m.reg.Get(callID)
	if !ok {
		return nil, ErrNotFound
	}
	if c.State.IsTerminal() {
		return c, nil
	}

	// Best effort: ask SIPp to softly quit via control ('q'), then fallback to signals.
	_ = sipp.SoftQuit(m.cfg.Sipp, c.ControlPort)

	cmd := m.getCmd(callID)
	if gracefulTimeout <= 0 {
		gracefulTimeout = 3 * time.Second
	}

	// Try SIGTERM first (graceful stop).
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	deadline := time.Now().Add(gracefulTimeout)
	for time.Now().Before(deadline) {
		if cmd == nil || cmd.Process == nil {
			break
		}
		if !process.ProcessAlive(cmd.Process.Pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Force kill if still alive.
	if cmd != nil && cmd.Process != nil && process.ProcessAlive(cmd.Process.Pid) {
		_ = cmd.Process.Kill()
	}

	m.finalizeCall(callID, StateHangup)
	updated, _ := m.reg.Get(callID)
	m.logger.Info("call hangup", zap.String("callId", callID))
	return updated, nil
}

func (m *Manager) Disconnect(callID string) (*CallContext, error) {
	c, ok := m.reg.Get(callID)
	if !ok {
		return nil, ErrNotFound
	}
	if c.State.IsTerminal() {
		return c, nil
	}

	cmd := m.getCmd(callID)
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}

	m.finalizeCall(callID, StateDisconnected)
	updated, _ := m.reg.Get(callID)
	m.logger.Info("call disconnected", zap.String("callId", callID))
	return updated, nil
}

// ReleaseResources releases ports and detaches the exec.Cmd from manager storage WITHOUT changing call state.
// This is useful for background monitors when the process exited unexpectedly but we want to keep FAILED state.
func (m *Manager) ReleaseResources(callID string) {
	// Atomically get ports and mark as released to prevent race conditions
	var portsToRelease *ports.Triple

	_, ok := m.reg.Update(callID, func(cc *CallContext) {
		// Capture ports to release if they're still allocated
		if cc.SipPort > 0 {
			portsToRelease = &ports.Triple{
				SipPort:     cc.SipPort,
				MediaPort:   cc.MediaPort,
				ControlPort: cc.ControlPort,
			}
			// Mark ports as released in registry
			cc.SipPort = 0
			cc.MediaPort = 0
			cc.ControlPort = 0
		}
	})

	// Release ports outside the registry lock if we captured them
	if ok && portsToRelease != nil {
		m.ports.Release(*portsToRelease)
	}

	m.mu.Lock()
	delete(m.cmds, callID)
	m.mu.Unlock()
}

func (m *Manager) finalizeCall(callID string, terminal State) {
	// Atomically get ports and update state to prevent race conditions
	var portsToRelease *ports.Triple

	_, ok := m.reg.Update(callID, func(cc *CallContext) {
		// Only update if not already terminal (idempotent)
		if !cc.State.IsTerminal() {
			cc.State = terminal

			// Capture ports to release if they're still allocated
			if cc.SipPort > 0 {
				portsToRelease = &ports.Triple{
					SipPort:     cc.SipPort,
					MediaPort:   cc.MediaPort,
					ControlPort: cc.ControlPort,
				}
				// Mark ports as released in registry
				cc.SipPort = 0
				cc.MediaPort = 0
				cc.ControlPort = 0
			}
		}
	})

	// Release ports outside the registry lock if we captured them
	if ok && portsToRelease != nil {
		m.ports.Release(*portsToRelease)
	}

	m.mu.Lock()
	delete(m.cmds, callID)
	m.mu.Unlock()
}

func (m *Manager) getCmd(callID string) *exec.Cmd {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cmds[callID]
}
