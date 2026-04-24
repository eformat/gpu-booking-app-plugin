package api

import (
	"testing"
	"time"
)

func TestIsValidBookingID(t *testing.T) {
	valid := []string{
		"booking-1",
		"booking-123456",
		"kueue-my-ns-gpu-s0-2025-04-24",
		"kueue-team-alpha-mig-3g-s1-2025-12-31",
	}
	for _, id := range valid {
		if !IsValidBookingID(id) {
			t.Errorf("expected valid: %q", id)
		}
	}

	invalid := []string{
		"",
		"booking-",
		"booking-abc",
		"random-string",
		"BOOKING-123",
		"kueue-",
		"kueue-ns-res-0-2025-01-01",        // missing s prefix on slot
		"kueue-NS-res-s0-2025-01-01",        // uppercase namespace
		"booking-1; DROP TABLE bookings; --", // injection attempt
	}
	for _, id := range invalid {
		if IsValidBookingID(id) {
			t.Errorf("expected invalid: %q", id)
		}
	}

	long := "booking-" + string(make([]byte, 300))
	if IsValidBookingID(long) {
		t.Error("expected rejection for ID exceeding 256 chars")
	}
}

func TestIsValidBookingDate(t *testing.T) {
	today := time.Now().Truncate(24 * time.Hour)
	window := 30

	todayStr := today.Format("2006-01-02")
	if !IsValidBookingDate(todayStr, window) {
		t.Errorf("today should be valid: %s", todayStr)
	}

	future := today.AddDate(0, 0, 15).Format("2006-01-02")
	if !IsValidBookingDate(future, window) {
		t.Errorf("15 days from now should be valid: %s", future)
	}

	yesterday := today.AddDate(0, 0, -1).Format("2006-01-02")
	if IsValidBookingDate(yesterday, window) {
		t.Errorf("yesterday should be invalid: %s", yesterday)
	}

	tooFar := today.AddDate(0, 0, window+5).Format("2006-01-02")
	if IsValidBookingDate(tooFar, window) {
		t.Errorf("beyond window should be invalid: %s", tooFar)
	}

	if IsValidBookingDate("not-a-date", window) {
		t.Error("garbage string should be invalid")
	}

	if IsValidBookingDate("", window) {
		t.Error("empty string should be invalid")
	}

	boundaryDate := today.AddDate(0, 0, window).Format("2006-01-02")
	if !IsValidBookingDate(boundaryDate, window) {
		t.Errorf("last day of window should be valid: %s", boundaryDate)
	}

	pastBoundary := today.AddDate(0, 0, window+1).Format("2006-01-02")
	if IsValidBookingDate(pastBoundary, window) {
		t.Errorf("one day past window should be invalid: %s", pastBoundary)
	}
}
