package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

func TestConfigHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	w := httptest.NewRecorder()

	ConfigHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var cfg database.Config
	if err := json.NewDecoder(w.Body).Decode(&cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.BookingWindowDays != BookingWindowDays {
		t.Errorf("BookingWindowDays = %d, want %d", cfg.BookingWindowDays, BookingWindowDays)
	}
	if len(cfg.Resources) == 0 {
		t.Error("expected non-empty resources")
	}
	if cfg.TotalCPU <= 0 {
		t.Error("expected positive TotalCPU")
	}
	if cfg.TotalMemory <= 0 {
		t.Error("expected positive TotalMemory")
	}
}
