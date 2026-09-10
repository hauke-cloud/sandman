package server

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// shutdownTimeout bounds how long in-flight requests may finish after a
// signal. Every handler here is a cache read or a single patch, so this is
// generous.
const shutdownTimeout = 10 * time.Second

// Start runs the HTTP server until the context is cancelled. It satisfies
// controller-runtime's manager.Runnable, so the API's lifetime is the
// manager's.
func (s *Server) Start(ctx context.Context) error {
	httpServer := s.HTTPServer()

	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("graceful shutdown of the API failed", "error", err)
		}
	}()

	s.logger.Info("serving the REST API", "address", s.opts.Address, "metrics_path", s.opts.MetricsPath,
		"authenticated", s.opts.Token != "", "read_only", s.opts.ReadOnly)

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}

// NeedLeaderElection reports that the API runs on every replica.
//
// Reads have to work wherever the request lands, and a write is only a patch
// recording what was asked for -- the leader is the one that acts on it. That
// keeps a standby replica useful rather than idle.
func (s *Server) NeedLeaderElection() bool { return false }
