// Package server serves fact-checker's HTTP surface.
//
// Today that is the operational health endpoint; the review queue export and
// other read-only surfaces join here in later stages.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// Pinger abstracts the datastore health probe so the handler is unit-testable
// without a database. *pgxpool.Pool satisfies it.
type Pinger interface {
	Ping(ctx context.Context) error
}

// New returns the HTTP handler for the operational surface.
func New(p Pinger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if err := p.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			writeJSON(w, map[string]string{"status": "unhealthy"})
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	})
	return mux
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func Run(ctx context.Context, p Pinger, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           New(p),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Nothing further we can do; the status code is already written.
		_ = err
	}
}
