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
	// 폴링 루프와 HTTP 핸들러가 동시에 접근하는 상태라 atomic.Bool을 사용한다.
	queueReady atomic.Bool
	pingDB     func(context.Context) error
}

func (h *workerHealth) handler() http.Handler {
	mux := http.NewServeMux()
	// 외부 서비스 장애가 Pod 재시작으로 이어지지 않도록 liveness는 HTTP 응답만 확인한다.
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
		// 헬스 서버가 멈추면 워커에도 종료 신호를 보내고 서버 오류를 전달한다.
		serveDone <- server.Serve(listener)
		stopWorker()
	}()

	log.Printf("collector worker started, health checks on %s", listener.Addr())
	w.poll(workerCtx, &health.queueReady)

	// 워커의 컨텍스트는 이미 취소됐을 수 있으므로 HTTP 종료에는 별도 제한 시간을 준다.
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
