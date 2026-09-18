package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFaultResetClosesExistingConnections(t *testing.T) {
	local, peer := net.Pipe()
	defer peer.Close()
	p := &proxy{active: map[net.Conn]struct{}{local: {}}}
	handler := faultHandler(map[string]*proxy{"redis": p})
	for _, body := range []string{`{"mode":"drop"}`, `{}`} {
		req := httptest.NewRequest(http.MethodPost, "/fault/redis", strings.NewReader(body))
		w := httptest.NewRecorder()
		handler(w, req)
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	if got := p.current().Mode; got != "" {
		t.Fatalf("fault did not reset: %q", got)
	}
	if _, err := peer.Write([]byte("x")); err == nil {
		t.Fatal("existing connection survived drop")
	}
}
