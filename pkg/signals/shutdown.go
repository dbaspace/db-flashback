package signals

import (
	"context"
	"log/slog"
	"time"

	"github.com/spf13/viper"
)

type ShutdownFn func(context.Context) error

type Shutdown struct {
	serverShutdownTimeout time.Duration
}

func NewShutdown(serverShutdownTimeout time.Duration) (*Shutdown, error) {
	srv := &Shutdown{
		serverShutdownTimeout: serverShutdownTimeout,
	}

	return srv, nil
}

func (s *Shutdown) Graceful(stopCh <-chan struct{}, shutdowns ...ShutdownFn) {
	ctx := context.Background()

	// wait for SIGTERM or SIGINT
	<-stopCh
	ctx, cancel := context.WithTimeout(ctx, s.serverShutdownTimeout)
	defer cancel()

	slog.Info("Shutting down server", slog.String("timeout", s.serverShutdownTimeout.String()))

	// wait for Kubernetes readiness probe to remove this instance from the load balancer
	// the readiness check interval must be lower than the timeout
	if viper.GetString("level") != "debug" {
		time.Sleep(3 * time.Second)
	}

	for _, shutdown := range shutdowns {
		if err := shutdown(ctx); err != nil {
			slog.Error("Shutdown failed", "error", err)
		}
	}
}
