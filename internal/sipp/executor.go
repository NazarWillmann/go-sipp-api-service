package sipp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Config holds the settings we need to run SIPp.
//
// Note: SIPp's remote control uses UDP, not TCP. You can send simple commands
// like 'q' to quit or 'Q' to force quit, just like pressing keys in interactive mode.
type Config struct {
	Binary                string
	ScenariosDir          string
	LocalIP               string
	ControlConnectTimeout time.Duration
	ControlReadTimeout    time.Duration
	LogsDir               string // where to save logs for each call
}

var (
	ErrControlUnreachable = errors.New("control port is not bound by SIPp after start")
)

// StartOutgoing starts SIPp to make an outgoing call.
// The 'destination' parameter is the actual number/service you want to call.
// We used to incorrectly use the server IP for this, which didn't work well.
//
// We use three different ports:
// - sipPort: for SIP messages
// - mediaPort: for audio/RTP (different from SIP port to avoid conflicts)
// - controlPort: so we can control SIPp while it's running
func StartOutgoing(
	ctx context.Context,
	cfg Config,
	callID string,
	remoteHost string,
	remotePort int,
	destination string, // the number/service you want to call
	scenario string,
	sipPort int, // port for SIP messages
	mediaPort int, // port for audio (must be different from SIP port)
	controlPort int, // port for controlling SIPp
) (*exec.Cmd, string, error) {
	if cfg.Binary == "" {
		return nil, "", errors.New("SIPP_BINARY is empty")
	}
	if cfg.ScenariosDir == "" {
		return nil, "", errors.New("SCENARIOS_DIR is empty")
	}
	if cfg.LocalIP == "" {
		return nil, "", errors.New("LOCAL_IP is empty")
	}

	scenarioSrc := filepath.Join(cfg.ScenariosDir, scenario)
	if _, err := os.Stat(scenarioSrc); err != nil {
		return nil, "", fmt.Errorf("scenario not found: %s: %w", scenarioSrc, err)
	}

	workDir := ""
	if cfg.LogsDir != "" {
		workDir = filepath.Join(cfg.LogsDir, callID)
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			return nil, "", fmt.Errorf("create workDir: %w", err)
		}
		// Copy scenario into workDir (SIPp uses scenario path to decide where to put trace logs).
		if err := copyFile(scenarioSrc, filepath.Join(workDir, filepath.Base(scenarioSrc))); err != nil {
			return nil, "", fmt.Errorf("copy scenario: %w", err)
		}
	} else {
		// If LogsDir isn't configured, still run from SCENARIOS_DIR so traces end up next to the scenario.
		workDir = cfg.ScenariosDir
	}

	scenarioPath := filepath.Join(workDir, filepath.Base(scenarioSrc))
	target := fmt.Sprintf("%s:%d", remoteHost, remotePort)

	args := []string{
		target,
		"-i", cfg.LocalIP,
		"-p", strconv.Itoa(sipPort),
		"-mp", strconv.Itoa(mediaPort),
		"-sf", scenarioPath,
		"-m", "1",
		"-bg", // non-interactive
		"-cp", strconv.Itoa(controlPort),
		"-nostdin", // do not wait for stdin in any mode
	}
	if destination != "" {
		args = append(args, "-s", destination)
	}

	// File naming is controlled by SIPp itself: <scenario>_<pid>_messages.log, _errors.log, _screen.log, etc.
	args = append(args,
		"-trace_err",
		"-trace_msg",
		"-trace_screen",
		"-trace_shortmsg",
		"-trace_calldebug",
		"-trace_logs",
		"-trace_counts",
	)

	cmd := exec.CommandContext(ctx, cfg.Binary, args...)
	cmd.Dir = workDir

	// Persist SIPp stdout/stderr too (super useful when SIPp fails before creating trace files).
	// IMPORTANT: we must NOT call cmd.Wait() here (Manager owns lifecycle). So we use pipes and copy.
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if cfg.LogsDir != "" {
		stdoutPath := filepath.Join(workDir, "sipp_stdout.log")
		stderrPath := filepath.Join(workDir, "sipp_stderr.log")
		stdoutFile, _ := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		stderrFile, _ := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		go copyAndClose(stdoutFile, stdout)
		go copyAndClose(stderrFile, stderr)
	} else {
		go drainPipe(stdout)
		go drainPipe(stderr)
	}

	if err := cmd.Start(); err != nil {
		return nil, "", err
	}

	// Fast failure check: many misconfigs cause SIPp to exit immediately.
	// Give it a tiny window, then see if it already died.
	time.Sleep(200 * time.Millisecond)
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return nil, workDir, fmt.Errorf("sipp exited immediately (code=%d)", cmd.ProcessState.ExitCode())
	}

	// Control port validation: SIPp remote control is UDP; there's no handshake.
	// We validate by attempting to bind the UDP port ourselves: if SIPp bound it, we should get EADDRINUSE.
	if err := waitUDPPortBound(cfg.LocalIP, controlPort, cfg.ControlConnectTimeout); err != nil {
		_ = killProcess(cmd)
		return nil, workDir, fmt.Errorf("%w: %v", ErrControlUnreachable, err)
	}

	return cmd, workDir, nil
}

func copyAndClose(dst *os.File, src io.ReadCloser) {
	defer func() {
		if dst != nil {
			_ = dst.Close()
		}
		if src != nil {
			_ = src.Close()
		}
	}()
	if dst == nil || src == nil {
		return
	}
	_, _ = io.Copy(dst, src)
}

// SoftQuit triggers SIPp's graceful stop (same as pressing 'q' in interactive mode).
// Per SIPp docs: 'q' quits after all calls complete.
func SoftQuit(cfg Config, controlPort int) error {
	return sendRemoteControlUDP(cfg.LocalIP, controlPort, "q")
}

// HardQuit triggers SIPp's immediate stop (same as pressing 'Q' in interactive mode).
func HardQuit(cfg Config, controlPort int) error {
	return sendRemoteControlUDP(cfg.LocalIP, controlPort, "Q")
}

// SendDTMF: remote control is NOT a DTMF API.
func SendDTMF(_ Config, _ int, digits string, _ time.Duration) error {
	digits = strings.TrimSpace(digits)
	if digits == "" {
		return errors.New("digits is empty")
	}
	return errors.New("DTMF via SIPp remote control is not supported; put DTMF into scenario (SIP INFO / RTP telephone-event)")
}

func sendRemoteControlUDP(host string, port int, payload string) error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	c, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return err
	}
	defer c.Close()
	_, werr := c.Write([]byte(payload))
	return werr
}

func waitUDPPortBound(host string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return err
	}
	for time.Now().Before(deadline) {
		c, err := net.ListenUDP("udp", addr)
		if err == nil {
			// We managed to bind => SIPp did NOT bind it.
			_ = c.Close()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// If address already in use, we consider port bound by SIPp.
		if strings.Contains(strings.ToLower(err.Error()), "address already in use") {
			return nil
		}
		// Other errors (e.g., host ip not present) -> return quickly.
		return err
	}
	return fmt.Errorf("udp %s:%d not bound within %s", host, port, timeout)
}

func drainPipe(r io.ReadCloser) {
	if r == nil {
		return
	}
	defer r.Close()
	s := bufio.NewScanner(r)
	for s.Scan() {
		// discard
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// best effort: try SIGKILL
	_ = cmd.Process.Signal(syscall.SIGKILL)
	_, _ = cmd.Process.Wait()
	return nil
}
