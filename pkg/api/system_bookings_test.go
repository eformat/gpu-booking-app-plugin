package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

// withNamespaceChecker overrides the namespace existence check for the duration
// of a test and restores the original afterwards.
func withNamespaceChecker(t *testing.T, fn func(ns string) (bool, error)) {
	t.Helper()
	orig := namespaceExistsFn
	namespaceExistsFn = fn
	t.Cleanup(func() { namespaceExistsFn = orig })
}

// withNamespaceAdminChecker overrides the namespace admin check for the
// duration of a test and restores the original afterwards.
func withNamespaceAdminChecker(t *testing.T, allowed bool, err error) {
	t.Helper()
	orig := namespaceAdminFn
	namespaceAdminFn = func(_ context.Context, _ *UserInfo, _ string) (bool, error) {
		return allowed, err
	}
	t.Cleanup(func() { namespaceAdminFn = orig })
}

func TestCreateBookingSystemByAdmin(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return true, nil })

	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full","description":"maas models","targetNamespace":"prelude-maas"}`

	req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(body))
	req = reqWithUser(req, testAdmin())
	w := httptest.NewRecorder()

	CreateBooking(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var b database.Booking
	if err := json.NewDecoder(w.Body).Decode(&b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.User != "prelude-maas" {
		t.Errorf("User = %q, want prelude-maas (target namespace)", b.User)
	}
	if b.Email != "admin" {
		t.Errorf("Email = %q, want admin (creating admin)", b.Email)
	}
	if b.BookingType != database.BookingTypeSystem {
		t.Errorf("BookingType = %q, want system", b.BookingType)
	}
	if b.Source != database.SourceReserved {
		t.Errorf("Source = %q, want reserved", b.Source)
	}

	// Verify stored row matches
	var user, email, bookingType string
	database.DB().QueryRow("SELECT user, email, booking_type FROM bookings WHERE id = ?", b.ID).Scan(&user, &email, &bookingType)
	if user != "prelude-maas" || email != "admin" || bookingType != "system" {
		t.Errorf("stored row: user=%q email=%q bookingType=%q, want prelude-maas/admin/system", user, email, bookingType)
	}
}

func TestCreateBookingSystemForbiddenForNonAdmin(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return true, nil })
	withNamespaceAdminChecker(t, false, nil)

	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full","targetNamespace":"prelude-maas"}`

	req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(body))
	req = reqWithUser(req, testUser())
	w := httptest.NewRecorder()

	CreateBooking(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if !strings.Contains(w.Body.String(), "not an admin of namespace prelude-maas") {
		t.Errorf("body = %s, want message naming the namespace", w.Body.String())
	}

	var count int
	database.DB().QueryRow("SELECT COUNT(*) FROM bookings").Scan(&count)
	if count != 0 {
		t.Error("no booking should have been created for a non-admin")
	}
}

func TestCreateBookingSystemByNamespaceAdmin(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return true, nil })
	withNamespaceAdminChecker(t, true, nil)

	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full","description":"team capacity","targetNamespace":"team-x"}`

	req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(body))
	req = reqWithUser(req, &UserInfo{Username: "jdoe", IsAdmin: false})
	w := httptest.NewRecorder()

	CreateBooking(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var b database.Booking
	if err := json.NewDecoder(w.Body).Decode(&b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b.User != "team-x" {
		t.Errorf("User = %q, want team-x (target namespace)", b.User)
	}
	if b.Email != "jdoe" {
		t.Errorf("Email = %q, want jdoe (creating namespace admin)", b.Email)
	}
	if b.BookingType != database.BookingTypeSystem {
		t.Errorf("BookingType = %q, want system", b.BookingType)
	}
}

func TestCreateBookingSystemInvalidNamespace(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return true, nil })

	invalid := []string{"UPPER", "a_b", "a.b", "-lead", "trail-", "a_b_c"}
	for _, ns := range invalid {
		date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
		body := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full","targetNamespace":"` + ns + `"}`
		req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(body))
		req = reqWithUser(req, testAdmin())
		w := httptest.NewRecorder()
		CreateBooking(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("namespace %q: status = %d, want %d", ns, w.Code, http.StatusBadRequest)
		}
	}

	// Over-long namespace (64 chars) should be rejected
	longNS := strings.Repeat("a", 64)
	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full","targetNamespace":"` + longNS + `"}`
	req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(body))
	req = reqWithUser(req, testAdmin())
	w := httptest.NewRecorder()
	CreateBooking(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("64-char namespace: status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestCreateBookingSystemNamespaceNotFound(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return false, nil })

	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full","targetNamespace":"missing-ns"}`

	req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(body))
	req = reqWithUser(req, testAdmin())
	w := httptest.NewRecorder()

	CreateBooking(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	if !strings.Contains(w.Body.String(), "namespace_not_found") {
		t.Errorf("body = %s, want namespace_not_found", w.Body.String())
	}

	var count int
	database.DB().QueryRow("SELECT COUNT(*) FROM bookings").Scan(&count)
	if count != 0 {
		t.Error("no booking should have been created for a missing namespace")
	}
}

func TestCreateBookingSystemNamespaceCheckFailed(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return false, fmt.Errorf("api down") })

	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full","targetNamespace":"prelude-maas"}`

	req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(body))
	req = reqWithUser(req, testAdmin())
	w := httptest.NewRecorder()

	CreateBooking(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestCreateBookingSystemNamespaceAdminCheckFailed(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return true, nil })
	withNamespaceAdminChecker(t, false, fmt.Errorf("api down"))

	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full","targetNamespace":"prelude-maas"}`

	req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(body))
	req = reqWithUser(req, testUser())
	w := httptest.NewRecorder()

	CreateBooking(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

func TestCreateBookingSystemEvictsConsumed(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return true, nil })
	db := database.DB()

	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"consumed-1", "kueue-user", "", "nvidia.com/gpu", 0, date, "full", "now", database.SourceConsumed, "", 0, 24, 0, database.BookingTypeUser,
	)

	body := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full","targetNamespace":"prelude-maas"}`
	req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(body))
	req = reqWithUser(req, testAdmin())
	w := httptest.NewRecorder()

	CreateBooking(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var count int
	db.QueryRow("SELECT COUNT(*) FROM bookings WHERE id = 'consumed-1'").Scan(&count)
	if count != 0 {
		t.Error("consumed booking should have been evicted by system booking")
	}
}

func TestSystemBookingConflictBothDirections(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return true, nil })

	date := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	sysBody := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full","targetNamespace":"prelude-maas"}`
	userBody := `{"resource":"nvidia.com/gpu","slotIndex":0,"date":"` + date + `","slotType":"full"}`

	// System booking first
	req := httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(sysBody))
	req = reqWithUser(req, testAdmin())
	w := httptest.NewRecorder()
	CreateBooking(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("system booking: status = %d", w.Code)
	}

	// User booking on the system-held slot must conflict
	req = httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(userBody))
	req = reqWithUser(req, testUser())
	w = httptest.NewRecorder()
	CreateBooking(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("user booking on system slot: status = %d, want %d", w.Code, http.StatusConflict)
	}

	// Reverse: user books slot 2, then system must conflict on the user-held slot
	sysBody2 := `{"resource":"nvidia.com/gpu","slotIndex":2,"date":"` + date + `","slotType":"full","targetNamespace":"prelude-maas"}`
	userBody2 := `{"resource":"nvidia.com/gpu","slotIndex":2,"date":"` + date + `","slotType":"full"}`

	req = httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(userBody2))
	req = reqWithUser(req, testUser())
	w = httptest.NewRecorder()
	CreateBooking(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("user booking: status = %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/bookings", strings.NewReader(sysBody2))
	req = reqWithUser(req, testAdmin())
	w = httptest.NewRecorder()
	CreateBooking(w, req)
	if w.Code != http.StatusConflict {
		t.Errorf("system booking on user slot: status = %d, want %d", w.Code, http.StatusConflict)
	}
}

func TestGetBookingsSystemActiveReservations(t *testing.T) {
	setupTestDB(t)
	db := database.DB()

	today := time.Now().UTC().Format("2006-01-02")
	// System booking for prelude-maas
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"sys-1", "prelude-maas", "admin", "nvidia.com/gpu", 0, today, "full", "now", database.SourceReserved, "", 0, 24, 0, database.BookingTypeSystem,
	)
	// User booking for alice
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"usr-1", "alice", "", "nvidia.com/gpu", 1, today, "full", "now", database.SourceReserved, "", 0, 24, 0, database.BookingTypeUser,
	)

	req := httptest.NewRequest(http.MethodGet, "/bookings", nil)
	req = reqWithUser(req, testUser())
	w := httptest.NewRecorder()

	GetBookings(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var resp struct {
		Bookings           []database.Booking `json:"bookings"`
		ActiveReservations map[string]string  `json:"activeReservations"`
	}
	json.NewDecoder(w.Body).Decode(&resp)

	if v, ok := resp.ActiveReservations["prelude-maas"]; !ok || v != "system-prelude-maas" {
		t.Errorf("activeReservations[prelude-maas] = %q, want system-prelude-maas", v)
	}
	if v, ok := resp.ActiveReservations["alice"]; !ok || v != "user-alice" {
		t.Errorf("activeReservations[alice] = %q, want user-alice", v)
	}

	// Booking payload carries the system type
	for _, b := range resp.Bookings {
		if b.ID == "sys-1" && b.BookingType != database.BookingTypeSystem {
			t.Errorf("sys-1 BookingType = %q, want system", b.BookingType)
		}
	}
}

func TestBulkBookingSystemByAdmin(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return true, nil })

	start := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := `{"resources":{"nvidia.com/gpu":2},"startDate":"` + start + `","endDate":"` + start + `","description":"maas","startHour":0,"endHour":24,"targetNamespace":"prelude-maas"}`

	req := httptest.NewRequest(http.MethodPost, "/bookings/bulk", strings.NewReader(body))
	req = reqWithUser(req, testAdmin())
	w := httptest.NewRecorder()

	BulkBookingHandler(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp struct {
		Bookings []database.Booking `json:"bookings"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Bookings) != 2 {
		t.Fatalf("bookings = %d, want 2", len(resp.Bookings))
	}
	for _, b := range resp.Bookings {
		if b.User != "prelude-maas" {
			t.Errorf("User = %q, want prelude-maas", b.User)
		}
		if b.Email != "admin" {
			t.Errorf("Email = %q, want admin", b.Email)
		}
		if b.BookingType != database.BookingTypeSystem {
			t.Errorf("BookingType = %q, want system", b.BookingType)
		}
	}
}

func TestBulkBookingSystemForbiddenForNonAdmin(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return true, nil })
	withNamespaceAdminChecker(t, false, nil)

	start := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := `{"resources":{"nvidia.com/gpu":1},"startDate":"` + start + `","endDate":"` + start + `","startHour":0,"endHour":24,"targetNamespace":"prelude-maas"}`

	req := httptest.NewRequest(http.MethodPost, "/bookings/bulk", strings.NewReader(body))
	req = reqWithUser(req, testUser())
	w := httptest.NewRecorder()

	BulkBookingHandler(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", w.Code, http.StatusForbidden)
	}

	var count int
	database.DB().QueryRow("SELECT COUNT(*) FROM bookings").Scan(&count)
	if count != 0 {
		t.Error("no booking should have been created for a non-admin")
	}
}

func TestBulkBookingSystemByNamespaceAdmin(t *testing.T) {
	setupTestDB(t)
	withNamespaceChecker(t, func(ns string) (bool, error) { return true, nil })
	withNamespaceAdminChecker(t, true, nil)

	start := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := `{"resources":{"nvidia.com/gpu":2},"startDate":"` + start + `","endDate":"` + start + `","description":"team capacity","startHour":0,"endHour":24,"targetNamespace":"team-x"}`

	req := httptest.NewRequest(http.MethodPost, "/bookings/bulk", strings.NewReader(body))
	req = reqWithUser(req, &UserInfo{Username: "jdoe", IsAdmin: false})
	w := httptest.NewRecorder()

	BulkBookingHandler(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	var resp struct {
		Bookings []database.Booking `json:"bookings"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Bookings) != 2 {
		t.Fatalf("bookings = %d, want 2", len(resp.Bookings))
	}
	for _, b := range resp.Bookings {
		if b.User != "team-x" {
			t.Errorf("User = %q, want team-x", b.User)
		}
		if b.Email != "jdoe" {
			t.Errorf("Email = %q, want jdoe", b.Email)
		}
		if b.BookingType != database.BookingTypeSystem {
			t.Errorf("BookingType = %q, want system", b.BookingType)
		}
	}
}

func TestDeleteBookingSystemForbiddenForNonAdmin(t *testing.T) {
	setupTestDB(t)
	withNamespaceAdminChecker(t, false, nil)
	db := database.DB()

	today := time.Now().Format("2006-01-02")
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"booking-777", "prelude-maas", "admin", "nvidia.com/gpu", 0, today, "full", "now", database.SourceReserved, "", 0, 24, 0, database.BookingTypeSystem,
	)

	req := httptest.NewRequest(http.MethodDelete, "/bookings?id=booking-777", nil)
	req = reqWithUser(req, testUser())
	w := httptest.NewRecorder()

	DeleteBooking(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d (owner is namespace, not the requesting user)", w.Code, http.StatusForbidden)
	}
}

func TestDeleteBookingSystemByCreator(t *testing.T) {
	setupTestDB(t)
	db := database.DB()

	today := time.Now().Format("2006-01-02")
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"booking-777", "team-x", "jdoe", "nvidia.com/gpu", 0, today, "full", "now", database.SourceReserved, "", 0, 24, 0, database.BookingTypeSystem,
	)

	req := httptest.NewRequest(http.MethodDelete, "/bookings?id=booking-777", nil)
	req = reqWithUser(req, &UserInfo{Username: "jdoe", IsAdmin: false})
	w := httptest.NewRecorder()

	DeleteBooking(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (creator may cancel their own system booking)", w.Code, http.StatusOK)
	}
}

func TestDeleteBookingSystemByNamespaceAdmin(t *testing.T) {
	setupTestDB(t)
	withNamespaceAdminChecker(t, true, nil)
	db := database.DB()

	today := time.Now().Format("2006-01-02")
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"booking-777", "team-x", "alice", "nvidia.com/gpu", 0, today, "full", "now", database.SourceReserved, "", 0, 24, 0, database.BookingTypeSystem,
	)

	req := httptest.NewRequest(http.MethodDelete, "/bookings?id=booking-777", nil)
	req = reqWithUser(req, &UserInfo{Username: "jdoe", IsAdmin: false})
	w := httptest.NewRecorder()

	DeleteBooking(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (namespace admin may cancel the reservation)", w.Code, http.StatusOK)
	}
}

func TestDeleteBookingSystemByAdmin(t *testing.T) {
	setupTestDB(t)
	db := database.DB()

	today := time.Now().Format("2006-01-02")
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"booking-777", "prelude-maas", "admin", "nvidia.com/gpu", 0, today, "full", "now", database.SourceReserved, "", 0, 24, 0, database.BookingTypeSystem,
	)

	req := httptest.NewRequest(http.MethodDelete, "/bookings?id=booking-777", nil)
	req = reqWithUser(req, testAdmin())
	w := httptest.NewRecorder()

	DeleteBooking(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestBulkCancelByCreator(t *testing.T) {
	setupTestDB(t)
	withNamespaceAdminChecker(t, false, nil)
	db := database.DB()

	today := time.Now().Format("2006-01-02")
	// Own system booking plus one created by alice in the same namespace
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"booking-777", "team-x", "jdoe", "nvidia.com/gpu", 0, today, "full", "now", database.SourceReserved, "", 0, 24, 0, database.BookingTypeSystem,
	)
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"booking-778", "team-x", "alice", "nvidia.com/gpu", 1, today, "full", "now", database.SourceReserved, "", 0, 24, 0, database.BookingTypeSystem,
	)

	body := `{"ids":["booking-777","booking-778"]}`
	req := httptest.NewRequest(http.MethodPost, "/bookings/bulk-cancel", strings.NewReader(body))
	req = reqWithUser(req, &UserInfo{Username: "jdoe", IsAdmin: false})
	w := httptest.NewRecorder()

	BulkCancelHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Deleted []string `json:"deleted"`
		Errors  []string `json:"errors"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Deleted) != 1 || resp.Deleted[0] != "booking-777" {
		t.Errorf("deleted = %v, want [booking-777] (creator may cancel their own)", resp.Deleted)
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0], "booking-778: forbidden") {
		t.Errorf("errors = %v, want booking-778: forbidden", resp.Errors)
	}
}

func TestBulkCancelByNamespaceAdmin(t *testing.T) {
	setupTestDB(t)
	withNamespaceAdminChecker(t, true, nil)
	db := database.DB()

	today := time.Now().Format("2006-01-02")
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"booking-777", "team-x", "alice", "nvidia.com/gpu", 0, today, "full", "now", database.SourceReserved, "", 0, 24, 0, database.BookingTypeSystem,
	)
	db.Exec(
		"INSERT INTO bookings ("+database.BookingColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		"booking-778", "team-x", "bob", "nvidia.com/gpu", 1, today, "full", "now", database.SourceReserved, "", 0, 24, 0, database.BookingTypeSystem,
	)

	body := `{"ids":["booking-777","booking-778"]}`
	req := httptest.NewRequest(http.MethodPost, "/bookings/bulk-cancel", strings.NewReader(body))
	req = reqWithUser(req, &UserInfo{Username: "jdoe", IsAdmin: false})
	w := httptest.NewRecorder()

	BulkCancelHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp struct {
		Deleted []string `json:"deleted"`
		Errors  []string `json:"errors"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Deleted) != 2 {
		t.Errorf("deleted = %v, want both (namespace admin manages the shared reservation)", resp.Deleted)
	}
	if len(resp.Errors) != 0 {
		t.Errorf("errors = %v, want none", resp.Errors)
	}
}

func TestCanManageBooking(t *testing.T) {
	orig := namespaceAdminFn
	namespaceAdminFn = func(_ context.Context, _ *UserInfo, ns string) (bool, error) {
		return ns == "team-x", nil
	}
	t.Cleanup(func() { namespaceAdminFn = orig })

	admin := &UserInfo{Username: "admin", IsAdmin: true}
	jdoe := &UserInfo{Username: "jdoe", IsAdmin: false}
	alice := &UserInfo{Username: "alice", IsAdmin: false}

	cases := []struct {
		name      string
		user      *UserInfo
		owner     string
		createdBy string
		want      bool
	}{
		{"cluster admin", admin, "team-x", "jdoe", true},
		{"owner match", jdoe, "jdoe", "", true},
		{"creator match", jdoe, "team-x", "jdoe", true},
		{"namespace admin", alice, "team-x", "jdoe", true},
		{"namespace denied", alice, "team-y", "jdoe", false},
		{"unrelated user", alice, "team-y", "bob", false},
		{"user booking strict for others", jdoe, "team-x", "", false},
	}
	for _, tc := range cases {
		if got := canManageBooking(context.Background(), tc.user, tc.owner, tc.createdBy, nil); got != tc.want {
			t.Errorf("%s: canManageBooking = %v, want %v", tc.name, got, tc.want)
		}
	}

	// Cache path: memoized namespace-admin result is honored
	cache := map[string]bool{"team-x": true}
	if !canManageBooking(context.Background(), alice, "team-x", "bob", cache) {
		t.Error("cached namespace admin should be allowed")
	}

	// Error path denies
	namespaceAdminFn = func(_ context.Context, _ *UserInfo, _ string) (bool, error) {
		return false, fmt.Errorf("api down")
	}
	if canManageBooking(context.Background(), alice, "team-x", "bob", nil) {
		t.Error("error from namespace admin check should deny")
	}
}

func TestNamespaceAdminFn(t *testing.T) {
	// Cluster admins always pass
	if ok, err := namespaceAdminFn(context.Background(), &UserInfo{Username: "admin", IsAdmin: true}, "any-ns"); err != nil || !ok {
		t.Errorf("cluster admin: ok=%v err=%v, want true/nil", ok, err)
	}
	// No auth client (tests/local dev) — allowed, no SAR performed
	if ok, err := namespaceAdminFn(context.Background(), &UserInfo{Username: "dev-admin", IsAdmin: false}, "any-ns"); err != nil || !ok {
		t.Errorf("no auth client: ok=%v err=%v, want true/nil", ok, err)
	}
}
