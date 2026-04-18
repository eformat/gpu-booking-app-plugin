package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
	"github.com/eformat/gpu-booking-plugin/pkg/kube"
)

func AdminListBookings(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)
	if !user.IsAdmin {
		HttpError(w, http.StatusForbidden, "admin_required")
		return
	}

	db := database.DB()
	rows, err := db.Query("SELECT " + database.BookingColumns + " FROM bookings ORDER BY date, slot_type")
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		return
	}
	defer rows.Close()

	bookings := []database.Booking{}
	for rows.Next() {
		b, err := database.ScanBooking(rows)
		if err != nil {
			continue
		}
		bookings = append(bookings, b)
	}

	JsonResponse(w, map[string]any{
		"bookings":               bookings,
		"config":                 database.GetConfig(BookingWindowDays),
		"totalSlots":             40,
		"reservationSyncEnabled": kube.ReservationSyncEnabled,
	})
}

func AdminDeleteBooking(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)
	if !user.IsAdmin {
		HttpError(w, http.StatusForbidden, "admin_required")
		return
	}

	db := database.DB()
	id := r.URL.Query().Get("id")

	// Delete all bookings when no id
	if id == "" {
		result, err := db.Exec("DELETE FROM bookings")
		if err != nil {
			HttpError(w, http.StatusInternalServerError, "database_error")
			return
		}
		rows, _ := result.RowsAffected()
		JsonResponse(w, map[string]any{"status": "deleted", "count": rows})
		go kube.SyncReservations()
		return
	}

	result, err := db.Exec("DELETE FROM bookings WHERE id = ?", id)
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		HttpError(w, http.StatusNotFound, "not_found")
		return
	}

	JsonResponse(w, map[string]string{"status": "deleted"})
	go kube.SyncReservations()
}

func AdminReservationToggleHandler(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)
	if !user.IsAdmin {
		HttpError(w, http.StatusForbidden, "admin_required")
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		HttpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	kube.ReservationSyncEnabled = req.Enabled
	log.Printf("admin: reservation sync set to enabled=%v", req.Enabled)

	JsonResponse(w, map[string]any{"reservationSyncEnabled": kube.ReservationSyncEnabled})
}

func AdminExportDatabase(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)
	if !user.IsAdmin {
		HttpError(w, http.StatusForbidden, "admin_required")
		return
	}

	db := database.DB()

	// Flush WAL
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		log.Printf("admin export: WAL checkpoint failed: %v", err)
		HttpError(w, http.StatusInternalServerError, "checkpoint_failed")
		return
	}

	f, err := os.Open(database.DBFilePath)
	if err != nil {
		log.Printf("admin export: failed to open db file: %v", err)
		HttpError(w, http.StatusInternalServerError, "file_open_failed")
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "file_stat_failed")
		return
	}

	w.Header().Set("Content-Disposition", "attachment; filename=bookings.db")
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "bookings.db", stat.ModTime(), f)
}

func AdminImportDatabase(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)
	if !user.IsAdmin {
		HttpError(w, http.StatusForbidden, "admin_required")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 100<<20)

	file, _, err := r.FormFile("database")
	if err != nil {
		HttpError(w, http.StatusBadRequest, "missing_database_field")
		return
	}
	defer file.Close()

	tmpFile, err := os.CreateTemp("", "bookings-import-*.db")
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "temp_file_failed")
		return
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmpFile, file); err != nil {
		tmpFile.Close()
		HttpError(w, http.StatusInternalServerError, "upload_copy_failed")
		return
	}
	tmpFile.Close()

	database.DBMu.Lock()
	defer database.DBMu.Unlock()

	database.Close()

	src, err := os.Open(tmpPath)
	if err != nil {
		database.OpenDB(database.DBFilePath)
		HttpError(w, http.StatusInternalServerError, "import_open_failed")
		return
	}
	dst, err := os.Create(database.DBFilePath)
	if err != nil {
		src.Close()
		database.OpenDB(database.DBFilePath)
		HttpError(w, http.StatusInternalServerError, "import_create_failed")
		return
	}
	if _, err := io.Copy(dst, src); err != nil {
		src.Close()
		dst.Close()
		database.OpenDB(database.DBFilePath)
		HttpError(w, http.StatusInternalServerError, "import_copy_failed")
		return
	}
	src.Close()
	dst.Close()

	os.Remove(database.DBFilePath + "-wal")
	os.Remove(database.DBFilePath + "-shm")

	if err := database.OpenDB(database.DBFilePath); err != nil {
		log.Printf("admin import: failed to reopen database: %v", err)
		HttpError(w, http.StatusInternalServerError, "reopen_failed")
		return
	}

	log.Println("admin: database imported successfully")
	go kube.SyncReservations()

	JsonResponse(w, map[string]string{"status": "imported"})
}
