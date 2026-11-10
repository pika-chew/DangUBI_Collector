package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

type workerHealth struct {
	queueReady atomic.Bool
	pingDB     func(context.Context) error
}

func (h *workerHealth) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !h.queueReady.Load() {
			http.Error(w, "SQS polling is not ready", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if h.pingDB(ctx) != nil {
			http.Error(w, "MySQL is not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (w queueWorker) serve(ctx context.Context, address string, pingDB func(context.Context) error) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen for health checks: %w", err)
	}
	health := workerHealth{pingDB: pingDB}
	server := &http.Server{
		Handler:           health.handler(),
		ReadHeaderTimeout: 2 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       15 * time.Second,
	}
	defer server.Close()
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
		stopWorker()
	}()

	log.Printf("collector worker started, health checks on %s", listener.Addr())
	w.poll(workerCtx, &health.queueReady)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve health checks: %w", err)
	}
	if shutdownErr != nil {
		return fmt.Errorf("stop health checks: %w", shutdownErr)
	}
	return nil
}
