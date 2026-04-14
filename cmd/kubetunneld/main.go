package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/asumaran/kubetunnel/internal/certs"
	"github.com/asumaran/kubetunnel/internal/config"
	"github.com/asumaran/kubetunnel/internal/control"
	"github.com/asumaran/kubetunnel/internal/logging"
	"github.com/asumaran/kubetunnel/internal/proxy"
	"github.com/asumaran/kubetunnel/internal/supervisor"
)

type daemon struct {
	mu         sync.RWMutex
	configPath string
	cfg        *config.Config
	logger     *logging.Logger
	sup        *supervisor.Supervisor
	prx        *proxy.Server
	ctrl       *control.Server
	shutdown   context.CancelFunc
}

func (d *daemon) Config() *config.Config         { d.mu.RLock(); defer d.mu.RUnlock(); return d.cfg }
func (d *daemon) Supervisor() *supervisor.Supervisor { return d.sup }
func (d *daemon) Logger() *logging.Logger            { return d.logger }

func (d *daemon) Reload() error {
	cfg, err := config.Load(d.configPath)
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.cfg = cfg
	d.mu.Unlock()
	d.sup.Reload(cfg)
	if err := d.prx.Update(cfg.Tunnels); err != nil {
		return err
	}
	d.logger.Daemon("info", "", "reload", "config reloaded", nil)
	return nil
}

func (d *daemon) Shutdown() {
	if d.shutdown != nil {
		d.shutdown()
	}
}

func main() {
	var (
		configPath = flag.String("config", "", "path to config.yaml (required)")
	)
	flag.Parse()
	if *configPath == "" {
		fmt.Fprintln(os.Stderr, "--config is required")
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	logger, err := logging.New(logging.Options{
		Dir:        cfg.Logging.Dir,
		MaxSizeMB:  cfg.Logging.MaxSizeMB,
		MaxFiles:   cfg.Logging.MaxFiles,
		MaxAgeDays: cfg.Logging.MaxAgeDays,
		Compress:   cfg.Logging.Compress,
	})
	if err != nil {
		log.Fatalf("logger: %v", err)
	}
	logger.Daemon("info", "", "startup", "kubetunneld starting",
		map[string]any{"config": *configPath, "tunnels": len(cfg.Tunnels)})

	// Certs: load or generate one per tunnel hostname.
	loader := certs.NewLoader()
	for _, t := range cfg.Tunnels {
		certPath, keyPath, err := certs.EnsureCert(cfg.TLS.CertDir, t.Hostname)
		if err != nil {
			logger.Daemon("error", t.Name, "cert_error", err.Error(), nil)
			continue
		}
		if err := loader.Add(t.Hostname, certPath, keyPath); err != nil {
			logger.Daemon("error", t.Name, "cert_load_error", err.Error(), nil)
		}
	}

	// Supervisor.
	sup := supervisor.New(logger, supervisor.KubectlExecer{Env: cfg.Environment})

	// Proxy.
	prx := proxy.New(cfg.TLS.ListenAddr, logger, sup, loader)
	if err := prx.Update(cfg.Tunnels); err != nil {
		log.Fatalf("proxy update: %v", err)
	}

	// Daemon wrapper.
	ctx, cancel := context.WithCancel(context.Background())
	d := &daemon{
		configPath: *configPath,
		cfg:        cfg,
		logger:     logger,
		sup:        sup,
		prx:        prx,
		shutdown:   cancel,
	}

	// Control server.
	ctrl := control.NewServer(d, cfg.Control.Socket)
	d.ctrl = ctrl
	if err := ctrl.Start(); err != nil {
		log.Fatalf("control server: %v", err)
	}
	logger.Daemon("info", "", "control_ready", "control socket listening",
		map[string]any{"socket": cfg.Control.Socket})

	// Supervisor start.
	if err := sup.Start(ctx, cfg); err != nil {
		log.Fatalf("supervisor start: %v", err)
	}

	// Proxy start in background.
	go func() {
		logger.Daemon("info", "", "proxy_start", "TLS proxy listening",
			map[string]any{"addr": cfg.TLS.ListenAddr})
		if err := prx.Start(); err != nil {
			logger.Daemon("error", "", "proxy_exit", err.Error(), nil)
		}
	}()

	// SIGHUP → reload.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			switch sig {
			case syscall.SIGHUP:
				if err := d.Reload(); err != nil {
					logger.Daemon("error", "", "reload_failed", err.Error(), nil)
				}
			case syscall.SIGINT, syscall.SIGTERM:
				logger.Daemon("info", "", "signal", fmt.Sprintf("received %s", sig), nil)
				cancel()
				return
			}
		}
	}()

	<-ctx.Done()
	logger.Daemon("info", "", "shutdown", "shutting down", nil)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = prx.Shutdown(shutdownCtx)
	sup.Stop()
	ctrl.Stop()
	logger.Daemon("info", "", "shutdown_complete", "bye", nil)
}
