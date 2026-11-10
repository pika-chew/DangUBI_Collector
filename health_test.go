package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerHealthSeparatesLivenessAndReadiness(t *testing.T) {
	for _, test := range []struct {
		name       string
		queueReady bool
		dbErr      error
		wantReady  int
	}{
		{"before first successful poll", false, nil, http.StatusServiceUnavailable},
		{"MySQL unavailable", true, errors.New("database unreachable"), http.StatusServiceUnavailable},
		{"ready", true, nil, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			health := workerHealth{pingDB: func(ctx context.Context) error {
				if !test.queueReady {
					t.Error("readiness pinged the DB before SQS was ready")
				}
				if _, bounded := ctx.Deadline(); !bounded {
					t.Error("readiness did not set a DB ping timeout")
				}
				return test.dbErr
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
