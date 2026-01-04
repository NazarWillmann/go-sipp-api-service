package calls

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"sipp-service/internal/ports"
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

type CreateRequest struct {
	RemoteHost  string
	RemotePort  int
	Destination string
	Scenario    string
}

func (m *Manager) CreateOutgoing(ctx context.Context, req CreateRequest) (*CallContext, error) {
	if req.RemoteHost == "" || req.RemotePort <= 0 || req.Scenario == "" {
		return nil, fmt.Errorf("invalid request")
	}
	if m.cfg.MaxActiveCalls > 0 && m.reg.CountActive() >= m.cfg.MaxActiveCalls {
		return nil, ErrTooManyCalls
	}

	pair, err := m.ports.Acquire()
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
		Scenario:    req.Scenario,
		SipPort:     pair.SipPort,
		ControlPort: pair.ControlPort,
		ProcessPid:  0,
		LastError:   nil,
	}

	if err := m.reg.Add(ctxCall); err != nil {
		m.ports.Release(pair)
		return nil, err
	}

	cmd, workDir, err := sipp.StartOutgoing(ctx, m.cfg.Sipp, callID, req.RemoteHost, req.RemotePort, req.Destination, req.Scenario, pair.SipPort, pair.ControlPort)
	if err != nil {
		m.reg.Update(callID, func(c *CallContext) {
			es := err.Error()
			c.LastError = &es
			c.State = StateFailed
		})
		m.ports.Release(pair)
		return nil, err
	}

	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}

	m.mu.Lock()
	m.cmds[callID] = cmd
	m.mu.Unlock()

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
		zap.String("scenario", req.Scenario),
		zap.Int("sipPort", pair.SipPort),
		zap.Int("controlPort", pair.ControlPort),
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

	// Best effort: ask SIPp to soft quit via control ('q'), then fallback to signals.
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
		if !processAlive(cmd.Process.Pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Force kill if still alive.
	if cmd != nil && cmd.Process != nil && processAlive(cmd.Process.Pid) {
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
	c, ok := m.reg.Get(callID)
	if ok {
		m.ports.Release(ports.Pair{SipPort: c.SipPort, ControlPort: c.ControlPort})
	}
	m.mu.Lock()
	delete(m.cmds, callID)
	m.mu.Unlock()
}

func (m *Manager) finalizeCall(callID string, terminal State) {
	c, ok := m.reg.Get(callID)
	if ok {
		m.ports.Release(ports.Pair{SipPort: c.SipPort, ControlPort: c.ControlPort})
	}

	m.reg.Update(callID, func(cc *CallContext) {
		cc.State = terminal
	})

	m.mu.Lock()
	delete(m.cmds, callID)
	m.mu.Unlock()
}

func (m *Manager) getCmd(callID string) *exec.Cmd {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cmds[callID]
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// unix liveness check: signal 0
	err = p.Signal(syscall.Signal(0))
	return err == nil
}
