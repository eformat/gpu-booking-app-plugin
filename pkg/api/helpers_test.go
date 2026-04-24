package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJsonResponse(t *testing.T) {
	w := httptest.NewRecorder()
	data := map[string]string{"hello": "world"}

	JsonResponse(w, data)

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	var got map[string]string
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["hello"] != "world" {
		t.Errorf("body hello = %q, want world", got["hello"])
	}
}

func TestJsonResponseStatus(t *testing.T) {
	w := httptest.NewRecorder()
	data := map[string]int{"count": 42}

	JsonResponseStatus(w, http.StatusCreated, data)

	if w.Code != http.StatusCreated {
		t.Errorf("status = %d, want %d", w.Code, http.StatusCreated)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]int
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["count"] != 42 {
		t.Errorf("body count = %d, want 42", got["count"])
	}
}

func TestHttpError(t *testing.T) {
	w := httptest.NewRecorder()

	HttpError(w, http.StatusBadRequest, "invalid input")

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got map[string]string
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["error"] != "invalid input" {
		t.Errorf("body error = %q, want 'invalid input'", got["error"])
	}
}

func TestHttpErrorStatuses(t *testing.T) {
	cases := []struct {
		status int
		msg    string
	}{
		{http.StatusNotFound, "not found"},
		{http.StatusForbidden, "forbidden"},
		{http.StatusInternalServerError, "internal error"},
		{http.StatusTooManyRequests, "rate_limit_exceeded"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		HttpError(w, tc.status, tc.msg)
		if w.Code != tc.status {
			t.Errorf("HttpError(%d, %q): got status %d", tc.status, tc.msg, w.Code)
		}
	}
}
