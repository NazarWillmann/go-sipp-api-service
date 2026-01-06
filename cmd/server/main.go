package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/spf13/viper"
	"go.uber.org/zap"

	"sipp-service/internal/api"
	"sipp-service/internal/calls"
	"sipp-service/internal/monitor"
	"sipp-service/internal/ports"
	"sipp-service/internal/sipp"
)

type AppConfig struct {
	SippBinary              string `mapstructure:"SIPP_BINARY"`
	ScenariosDir            string `mapstructure:"SCENARIOS_DIR"`
	LocalIP                 string `mapstructure:"LOCAL_IP"`
	SipPortRangeStart       int    `mapstructure:"SIP_PORT_RANGE_START"`
	SipPortRangeEnd         int    `mapstructure:"SIP_PORT_RANGE_END"`
	ControlPortRangeStart   int    `mapstructure:"CONTROL_PORT_RANGE_START"`
	ControlPortRangeEnd     int    `mapstructure:"CONTROL_PORT_RANGE_END"`
	MaxActiveCalls          int    `mapstructure:"MAX_ACTIVE_CALLS"`
	ControlConnectTimeoutMs int    `mapstructure:"CONTROL_CONNECT_TIMEOUT_MS"`
	ControlReadTimeoutMs    int    `mapstructure:"CONTROL_READ_TIMEOUT_MS"`
	ApiBind                 string `mapstructure:"API_BIND"`
	LogsDir                 string `mapstructure:"LOGS_DIR"`
}

func loadConfig() (*AppConfig, error) {
	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath("./configs")
	v.AddConfigPath(".")
	v.AutomaticEnv()

	// defaults
	v.SetDefault("API_BIND", "0.0.0.0:8080")
	v.SetDefault("CONTROL_CONNECT_TIMEOUT_MS", 500)
	v.SetDefault("CONTROL_READ_TIMEOUT_MS", 500)
	v.SetDefault("MAX_ACTIVE_CALLS", 20)

	_ = v.ReadInConfig() // optional

	var cfg AppConfig
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	// Validate required fields
	if cfg.SippBinary == "" {
		return nil, errors.New("SIPP_BINARY is required")
	}
	if cfg.ScenariosDir == "" {
		return nil, errors.New("SCENARIOS_DIR is required")
	}
	if cfg.SipPortRangeStart <= 0 || cfg.SipPortRangeEnd <= 0 {
		return nil, errors.New("SIP port range must be positive")
	}
	if cfg.ControlPortRangeStart <= 0 || cfg.ControlPortRangeEnd <= 0 {
		return nil, errors.New("Control port range must be positive")
	}
	if cfg.SipPortRangeStart > cfg.SipPortRangeEnd {
		return nil, errors.New("SIP port range start must be <= end")
	}
	if cfg.ControlPortRangeStart > cfg.ControlPortRangeEnd {
		return nil, errors.New("Control port range start must be <= end")
	}

	// Allow LOCAL_IP to be omitted; we will autodetect.
	cfg.LocalIP = strings.TrimSpace(cfg.LocalIP)
	if cfg.LocalIP == "" {
		ip, err := detectLocalIP()
		if err != nil {
			return nil, fmt.Errorf("LOCAL_IP is empty and autodetect failed: %w", err)
		}
		cfg.LocalIP = ip
	}

	return &cfg, nil
}

// detectLocalIP returns the primary outbound IP (best-effort).
func detectLocalIP() (string, error) {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "", err
	}
	defer c.Close()
	localAddr := c.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String(), nil
}

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	cfg, err := loadConfig()
	if err != nil {
		logger.Fatal("failed to load config", zap.Error(err))
	}

	allocator, err := ports.NewAllocator(cfg.SipPortRangeStart, cfg.SipPortRangeEnd, cfg.ControlPortRangeStart, cfg.ControlPortRangeEnd)
	if err != nil {
		logger.Fatal("failed to init port allocator", zap.Error(err))
	}

	reg := calls.NewRegistry()

	mgr := calls.NewManager(
		calls.ManagerConfig{
			MaxActiveCalls: cfg.MaxActiveCalls,
			Retention:      5 * time.Minute,
			Sipp: sipp.Config{
				Binary:                cfg.SippBinary,
				ScenariosDir:          cfg.ScenariosDir,
				LocalIP:               cfg.LocalIP,
				ControlConnectTimeout: time.Duration(cfg.ControlConnectTimeoutMs) * time.Millisecond,
				ControlReadTimeout:    time.Duration(cfg.ControlReadTimeoutMs) * time.Millisecond,
				LogsDir:               cfg.LogsDir,
			},
		},
		reg,
		allocator,
		logger,
	)

	stopMon := make(chan struct{})
	mon := monitor.NewMonitor(reg, mgr, logger, 5*time.Minute)
	mon.Start(stopMon)

	h := &api.Handler{
		Mgr:            mgr,
		Reg:            reg,
		SippBinary:     cfg.SippBinary,
		ScenariosDir:   cfg.ScenariosDir,
		LocalIP:        cfg.LocalIP,
		LogsDir:        cfg.LogsDir,
		SipRangeStart:  cfg.SipPortRangeStart,
		SipRangeEnd:    cfg.SipPortRangeEnd,
		CtrlRangeStart: cfg.ControlPortRangeStart,
		CtrlRangeEnd:   cfg.ControlPortRangeEnd,
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Route("/api/v1", func(r chi.Router) {
		r.Mount("/", h.Routes())
	})

	srv := &http.Server{
		Addr:    cfg.ApiBind,
		Handler: r,
	}

	go func() {
		logger.Info("http server started", zap.String("bind", cfg.ApiBind), zap.String("localIp", cfg.LocalIP))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("http server failed", zap.Error(err))
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("shutting down gracefully...")

	// Terminate all active calls
	activeCalls := reg.List()
	for _, call := range activeCalls {
		if !call.State.IsTerminal() {
			logger.Info("terminating active call", zap.String("callId", call.CallID))
			mgr.Disconnect(call.CallID)
		}
	}

	close(stopMon)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	logger.Info("shutdown complete")
}
