package monitor

import (
	"time"

	"go.uber.org/zap"

	"sipp-service/internal/calls"
	"sipp-service/internal/process"
)

// Monitor supervises calls and performs cleanup of terminal calls.
type Monitor struct {
	reg    *calls.Registry
	mgr    *calls.Manager
	logger *zap.Logger

	processTick time.Duration
	cleanupTick time.Duration
	retention   time.Duration
}

func NewMonitor(reg *calls.Registry, mgr *calls.Manager, logger *zap.Logger, retention time.Duration) *Monitor {
	if retention == 0 {
		retention = 5 * time.Minute
	}
	return &Monitor{
		reg: reg, mgr: mgr, logger: logger,
		processTick: 5 * time.Second,
		cleanupTick: 5 * time.Minute,
		retention:   retention,
	}
}

func (m *Monitor) Start(stop <-chan struct{}) {
	go m.runProcessLoop(stop)
	go m.runCleanupLoop(stop)
}

func (m *Monitor) runProcessLoop(stop <-chan struct{}) {
	t := time.NewTicker(m.processTick)
	defer t.Stop()

	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.checkProcesses()
		}
	}
}

func (m *Monitor) checkProcesses() {
	callsList := m.reg.List()
	for _, c := range callsList {
		if c.State.IsTerminal() {
			continue
		}
		// If process PID is missing -> treat as failed.
		if c.ProcessPid <= 0 {
			m.reg.Update(c.CallID, func(cc *calls.CallContext) {
				msg := "missing process pid"
				cc.LastError = &msg
				cc.State = calls.StateFailed
			})
			m.mgr.ReleaseResources(c.CallID)
			continue
		}

		// Best-effort liveness: if process is gone, mark FAILED and release ports via Disconnect().
		if !process.ProcessAlive(c.ProcessPid) {
			m.logger.Warn("process exited unexpectedly", zap.String("callId", c.CallID), zap.Int("pid", c.ProcessPid))
			m.reg.Update(c.CallID, func(cc *calls.CallContext) {
				msg := "process exited"
				cc.LastError = &msg
				cc.State = calls.StateFailed
			})
			// ensure resources released but keep FAILED state
			m.mgr.ReleaseResources(c.CallID)
		}
	}
}

func (m *Monitor) runCleanupLoop(stop <-chan struct{}) {
	t := time.NewTicker(m.cleanupTick)
	defer t.Stop()

	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.cleanup()
		}
	}
}

func (m *Monitor) cleanup() {
	now := time.Now().UTC()
	for _, c := range m.reg.List() {
		if !c.State.IsTerminal() {
			continue
		}
		if now.Sub(c.UpdatedAt) > m.retention {
			m.reg.Delete(c.CallID)
		}
	}
}
