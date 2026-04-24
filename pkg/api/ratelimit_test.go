package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimitMiddlewareAllows(t *testing.T) {
	handler := RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:12345"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestRateLimitMiddlewareBlocks(t *testing.T) {
	handler := RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust the burst limit
	for i := 0; i < RateLimitBurst+10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.99:12345"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if i >= RateLimitBurst && w.Code == http.StatusTooManyRequests {
			return // success
		}
	}
	t.Error("expected 429 after exceeding burst limit")
}

func TestRateLimitMiddlewarePerIP(t *testing.T) {
	handler := RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust limit for one IP
	for i := 0; i < RateLimitBurst+5; i++ {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = "10.0.0.50:12345"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}

	// Different IP should still work
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.51:12345"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("different IP should not be rate limited, got %d", w.Code)
	}
}

func TestRateLimitMiddlewareBadRemoteAddr(t *testing.T) {
	handler := RateLimitMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "not-a-valid-addr"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}
