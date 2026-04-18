package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
	"github.com/eformat/gpu-booking-plugin/pkg/kube"
)

func GetBookings(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)
	db := database.DB()

	rows, err := db.Query("SELECT " + database.BookingColumns + " FROM bookings ORDER BY date, slot_type")
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		log.Printf("error querying bookings: %v", err)
		return
	}
	defer rows.Close()

	bookings := []database.Booking{}
	for rows.Next() {
		b, err := database.ScanBooking(rows)
		if err != nil {
			log.Printf("error scanning booking: %v", err)
			continue
		}
		bookings = append(bookings, b)
	}

	// Build active reservations map: user -> clusterqueue name
	activeRes := map[string]string{}
	today := time.Now().Format("2006-01-02")
	for _, b := range bookings {
		if b.Source == "reserved" && b.Date == today {
			if _, ok := activeRes[b.User]; !ok {
				activeRes[b.User] = "user-" + b.User
			}
		}
	}

	JsonResponse(w, map[string]any{
		"bookings":           bookings,
		"activeReservations": activeRes,
		"currentUser":        user.Username,
	})
}

func CreateBooking(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)
	db := database.DB()

	var req struct {
		Resource    string `json:"resource"`
		SlotIndex   int    `json:"slotIndex"`
		Date        string `json:"date"`
		SlotType    string `json:"slotType"`
		Description string `json:"description"`
		StartHour   int    `json:"startHour"`
		EndHour     int    `json:"endHour"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		HttpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	if req.SlotType != "full" {
		HttpError(w, http.StatusBadRequest, "invalid_slot_type")
		return
	}

	// Check for conflicts
	rows, err := db.Query(
		"SELECT id, source FROM bookings WHERE resource = ? AND slot_index = ? AND date = ?",
		req.Resource, req.SlotIndex, req.Date,
	)
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		log.Printf("error checking conflicts: %v", err)
		return
	}
	var conflictIDs []string
	hasReservedConflict := false
	for rows.Next() {
		var cID, cSource string
		if err := rows.Scan(&cID, &cSource); err != nil {
			continue
		}
		if cSource == "reserved" {
			hasReservedConflict = true
		}
		conflictIDs = append(conflictIDs, cID)
	}
	rows.Close()

	if hasReservedConflict {
		JsonResponseStatus(w, http.StatusConflict, map[string]string{"error": "slot_taken"})
		return
	}

	// Evict consumed bookings
	for _, cID := range conflictIDs {
		if _, err := db.Exec("DELETE FROM bookings WHERE id = ?", cID); err != nil {
			log.Printf("error evicting consumed booking %s: %v", cID, err)
		} else {
			log.Printf("evicted consumed booking %s for reservation by %s", cID, user.Username)
		}
	}

	id := fmt.Sprintf("booking-%d", time.Now().UnixNano())
	createdAt := time.Now().UTC().Format(time.RFC3339)

	desc := req.Description
	if len(desc) > 160 {
		desc = desc[:160]
	}

	startHour := req.StartHour
	endHour := req.EndHour
	if startHour < 0 || startHour > 23 {
		startHour = 0
	}
	if endHour < 1 || endHour > 24 {
		endHour = 24
	}

	_, err = db.Exec(
		"INSERT INTO bookings (id, user, email, resource, slot_index, date, slot_type, created_at, source, description, start_hour, end_hour) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'reserved', ?, ?, ?)",
		id, user.Username, "", req.Resource, req.SlotIndex, req.Date, req.SlotType, createdAt, desc, startHour, endHour,
	)
	if err != nil {
		JsonResponseStatus(w, http.StatusConflict, map[string]string{"error": "slot_taken"})
		return
	}

	booking := database.Booking{
		ID:          id,
		User:        user.Username,
		Resource:    req.Resource,
		SlotIndex:   req.SlotIndex,
		Date:        req.Date,
		SlotType:    req.SlotType,
		CreatedAt:   createdAt,
		Source:      "reserved",
		Description: desc,
		StartHour:   startHour,
		EndHour:     endHour,
	}

	JsonResponseStatus(w, http.StatusCreated, booking)
	go kube.SyncReservations()
}

func DeleteBooking(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		HttpError(w, http.StatusBadRequest, "missing_id")
		return
	}

	user := GetUser(r)
	db := database.DB()

	var owner, source string
	err := db.QueryRow("SELECT user, source FROM bookings WHERE id = ?", id).Scan(&owner, &source)
	if err == sql.ErrNoRows {
		HttpError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		return
	}

	if source == "consumed" {
		JsonResponseStatus(w, http.StatusForbidden, map[string]string{"error": "consumed_booking"})
		return
	}

	if owner != user.Username && !user.IsAdmin {
		HttpError(w, http.StatusForbidden, "forbidden")
		return
	}

	_, err = db.Exec("DELETE FROM bookings WHERE id = ?", id)
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		return
	}

	JsonResponse(w, map[string]string{"status": "deleted"})
	go kube.SyncReservations()
}

func BulkCancelHandler(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)
	db := database.DB()

	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
		HttpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	var deleted []string
	var errors []string
	for _, id := range req.IDs {
		var owner, source string
		err := db.QueryRow("SELECT user, source FROM bookings WHERE id = ?", id).Scan(&owner, &source)
		if err == sql.ErrNoRows {
			errors = append(errors, fmt.Sprintf("%s: not_found", id))
			continue
		}
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: database_error", id))
			continue
		}
		if source == "consumed" {
			errors = append(errors, fmt.Sprintf("%s: consumed_booking", id))
			continue
		}
		if owner != user.Username && !user.IsAdmin {
			errors = append(errors, fmt.Sprintf("%s: forbidden", id))
			continue
		}
		if _, err := db.Exec("DELETE FROM bookings WHERE id = ?", id); err != nil {
			errors = append(errors, fmt.Sprintf("%s: database_error", id))
			continue
		}
		deleted = append(deleted, id)
	}

	JsonResponse(w, map[string]any{
		"deleted": deleted,
		"errors":  errors,
	})

	if len(deleted) > 0 {
		go kube.SyncReservations()
	}
}

func BulkBookingHandler(w http.ResponseWriter, r *http.Request) {
	user := GetUser(r)
	db := database.DB()

	var req struct {
		Resources   map[string]int `json:"resources"`
		StartDate   string         `json:"startDate"`
		EndDate     string         `json:"endDate"`
		Description string         `json:"description"`
		StartHour   int            `json:"startHour"`
		EndHour     int            `json:"endHour"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		HttpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	if req.StartDate == "" || req.EndDate == "" || len(req.Resources) == 0 {
		HttpError(w, http.StatusBadRequest, "missing_fields")
		return
	}

	if req.StartDate > req.EndDate {
		HttpError(w, http.StatusBadRequest, "invalid_date_range")
		return
	}

	desc := req.Description
	if len(desc) > 160 {
		desc = desc[:160]
	}

	startHour := req.StartHour
	endHour := req.EndHour
	if startHour < 0 || startHour > 23 {
		startHour = 0
	}
	if endHour < 1 || endHour > 24 {
		endHour = 24
	}

	start, err := time.Parse("2006-01-02", req.StartDate)
	if err != nil {
		HttpError(w, http.StatusBadRequest, "invalid_start_date")
		return
	}
	end, err := time.Parse("2006-01-02", req.EndDate)
	if err != nil {
		HttpError(w, http.StatusBadRequest, "invalid_end_date")
		return
	}

	var dates []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		dates = append(dates, d.Format("2006-01-02"))
	}

	cfg := database.GetConfig(BookingWindowDays)
	var created []database.Booking
	var errors []string

	for resource, count := range req.Resources {
		if count <= 0 {
			continue
		}
		for _, date := range dates {
			slotRows, err := db.Query(
				"SELECT slot_index, source, id FROM bookings WHERE resource = ? AND date = ?",
				resource, date,
			)
			if err != nil {
				errors = append(errors, fmt.Sprintf("%s on %s: database error", resource, date))
				continue
			}
			reserved := map[int]bool{}
			consumedIDs := map[int]string{}
			for slotRows.Next() {
				var idx int
				var src, bid string
				if err := slotRows.Scan(&idx, &src, &bid); err == nil {
					if src == "reserved" {
						reserved[idx] = true
					} else {
						consumedIDs[idx] = bid
					}
				}
			}
			slotRows.Close()

			maxUnits := 0
			for _, gr := range cfg.Resources {
				if gr.Type == resource {
					maxUnits = gr.Count
					break
				}
			}
			if maxUnits == 0 {
				errors = append(errors, fmt.Sprintf("%s: unknown resource type", resource))
				continue
			}

			booked := 0
			for unitIdx := 0; unitIdx < maxUnits && booked < count; unitIdx++ {
				if reserved[unitIdx] {
					continue
				}
				if cID, ok := consumedIDs[unitIdx]; ok {
					if _, err := db.Exec("DELETE FROM bookings WHERE id = ?", cID); err != nil {
						log.Printf("bulk booking: error evicting consumed booking %s: %v", cID, err)
						continue
					}
					log.Printf("bulk booking: evicted consumed booking %s for reservation by %s", cID, user.Username)
				}

				id := fmt.Sprintf("booking-%d", time.Now().UnixNano())
				createdAt := time.Now().UTC().Format(time.RFC3339)

				_, err := db.Exec(
					"INSERT INTO bookings (id, user, email, resource, slot_index, date, slot_type, created_at, source, description, start_hour, end_hour) VALUES (?, ?, ?, ?, ?, ?, 'full', ?, 'reserved', ?, ?, ?)",
					id, user.Username, "", resource, unitIdx, date, createdAt, desc, startHour, endHour,
				)
				if err != nil {
					log.Printf("bulk booking: insert failed for %s unit %d on %s: %v", resource, unitIdx, date, err)
					continue
				}

				created = append(created, database.Booking{
					ID:          id,
					User:        user.Username,
					Resource:    resource,
					SlotIndex:   unitIdx,
					Date:        date,
					SlotType:    "full",
					CreatedAt:   createdAt,
					Source:      "reserved",
					Description: desc,
					StartHour:   startHour,
					EndHour:     endHour,
				})
				booked++
			}
			if booked < count {
				errors = append(errors, fmt.Sprintf("%s on %s: only %d of %d slots available", resource, date, booked, count))
			}
		}
	}

	if len(created) == 0 && len(errors) > 0 {
		JsonResponseStatus(w, http.StatusConflict, map[string]any{"error": "no_slots_available", "details": errors})
		return
	}

	JsonResponseStatus(w, http.StatusCreated, map[string]any{
		"bookings": created,
		"errors":   errors,
	})

	if len(created) > 0 {
		go kube.SyncReservations()
	}
}
