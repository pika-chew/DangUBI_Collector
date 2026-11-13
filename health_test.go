package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerHealthSeparatesLivenessAndReadiness(t *testing.T) {
	// SQS나 Swift를 쓸 수 없어도 liveness는 성공하고 readiness만 실패해야 한다.
	for _, test := range []struct {
		name       string
		queueReady bool
		swiftErr   error
		wantReady  int
	}{
		{"before first successful poll", false, nil, http.StatusServiceUnavailable},
		{"Swift unavailable", true, errors.New("Swift unreachable"), http.StatusServiceUnavailable},
		{"ready", true, nil, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			health := workerHealth{checkSwift: func(ctx context.Context) error {
				if !test.queueReady {
					t.Error("readiness checked Swift before SQS was ready")
				}
				if _, bounded := ctx.Deadline(); !bounded {
					t.Error("readiness did not set a Swift check timeout")
				}
				return test.swiftErr
			}}
			health.queueReady.Store(test.queueReady)
			handler := health.handler()
			live := httptest.NewRecorder()
			handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, "/livez", nil))
			if live.Code != http.StatusOK {
				t.Errorf("liveness status = %d, want 200", live.Code)
			}
			ready := httptest.NewRecorder()
			handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if ready.Code != test.wantReady {
				t.Errorf("readiness status = %d, want %d", ready.Code, test.wantReady)
			}
		})
	}
}
