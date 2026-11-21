package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type closeCounter struct {
	Backend
	calls atomic.Int32
}

func (c *closeCounter) Close() error { c.calls.Add(1); return nil }

func TestCloseCancelsTurnBeforeReleasingBackend(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	backend := &closeCounter{}
	started := make(chan struct{})
	host := &Supervisor{closing: ctx, cancel: cancel, backend: backend, handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		if backend.calls.Load() != 0 {
			t.Error("released backend during a turn")
		}
	})}
	req := httptest.NewRequest("POST", "/v1/turn", strings.NewReader("{}"))
	done := make(chan struct{})
	go func() { host.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	<-started
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	<-done
	w := httptest.NewRecorder()
	host.ServeHTTP(w, req)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
	if err := host.Close(); err != nil || backend.calls.Load() != 1 {
		t.Fatal(err, backend.calls.Load())
	}
}
