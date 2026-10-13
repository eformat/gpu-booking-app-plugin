package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
	"github.com/eformat/gpu-booking-plugin/pkg/kube"
)

// namespaceExistsFn verifies a namespace exists in the cluster for system
// bookings. Skips verification when no Kubernetes client is configured
// (local development). Overridden in tests to simulate cluster lookups.
var namespaceExistsFn = func(ns string) (bool, error) {
	if !kube.K8sAvailable() {
		return true, nil
	}
	return kube.NamespaceExists(ns)
}

// validateSystemBookingTarget validates a system booking target namespace and
// that the user may administer it. Returns an error message and HTTP status on
// failure, or "" on success.
func validateSystemBookingTarget(ctx context.Context, user *UserInfo, target string) (string, int) {
	if !IsValidK8sName(target) {
		return "invalid_namespace", http.StatusBadRequest
	}
	exists, err := namespaceExistsFn(target)
	if err != nil {
		slog.Error("namespace check failed", "namespace", target, "error", err)
		return "namespace_check_failed", http.StatusInternalServerError
	}
	if !exists {
		return "namespace_not_found", http.StatusBadRequest
	}
	allowed, err := namespaceAdminFn(ctx, user, target)
	if err != nil {
		slog.Error("namespace admin check failed", "namespace", target, "user", user.Username, "error", err)
		return "namespace_check_failed", http.StatusInternalServerError
	}
	if !allowed {
		return fmt.Sprintf("you are not an admin of namespace %s", target), http.StatusForbidden
	}
	return "", 0
}

// canManageBooking reports whether the user may cancel/delete the booking
// owned by owner and created by createdBy. Allowed for the booking owner, its
// creator, and cluster admins. System bookings (createdBy non-empty: owner is
// the target namespace, creator is stored in the email column) may also be
// managed by admins of the owning namespace. nsAdminCache memoizes
// namespace-admin lookups across calls (bulk cancel); may be nil.
func canManageBooking(ctx context.Context, user *UserInfo, owner, createdBy string, nsAdminCache map[string]bool) bool {
	if user.IsAdmin || owner == user.Username || (createdBy != "" && createdBy == user.Username) {
		return true
	}
	// Namespace-admin rights only apply to system bookings (createdBy set);
	// user bookings remain manageable by owner and cluster admins only.
	if createdBy == "" {
		return false
	}
	if nsAdminCache != nil {
		if cached, ok := nsAdminCache[owner]; ok {
			return cached
		}
	}
	allowed, err := namespaceAdminFn(ctx, user, owner)
	if err != nil {
		slog.Error("namespace admin check failed", "namespace", owner, "user", user.Username, "error", err)
		allowed = false
	}
	if nsAdminCache != nil {
		nsAdminCache[owner] = allowed
	}
	return allowed
}

func GetBookings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := GetUser(r)
	db := database.DB()

	startDate := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	endDate := time.Now().AddDate(0, 0, BookingWindowDays+1).Format("2006-01-02")
	rows, err := db.QueryContext(ctx,
		"SELECT "+database.BookingColumns+" FROM bookings WHERE date >= ? AND date < ? ORDER BY date, slot_type",
		startDate, endDate)
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		slog.Error("failed to query bookings", "error", err)
		return
	}
	defer rows.Close()

	bookings := []database.Booking{}
	for rows.Next() {
		b, err := database.ScanBooking(rows)
		if err != nil {
			slog.Error("failed to scan booking", "error", err)
			continue
		}
		bookings = append(bookings, b)
	}
	if err := rows.Err(); err != nil {
		slog.Error("failed iterating bookings", "error", err)
	}

	// Build active reservations map: user -> clusterqueue name
	activeRes := map[string]string{}
	now := time.Now().UTC()
	for _, b := range bookings {
		if b.Source != database.SourceReserved {
			continue
		}
		base, err := time.Parse("2006-01-02", b.Date)
		if err != nil {
			continue
		}
		utcStart := base.Add(time.Duration((float64(b.StartHour) - b.UtcOffset) * float64(time.Hour)))
		utcEnd := base.Add(time.Duration((float64(b.EndHour) - b.UtcOffset) * float64(time.Hour)))
		if !now.Before(utcStart) && now.Before(utcEnd) {
			if _, ok := activeRes[b.User]; !ok {
				if b.BookingType == database.BookingTypeSystem {
					activeRes[b.User] = "system-" + b.User
				} else {
					activeRes[b.User] = "user-" + b.User
				}
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
	ctx := r.Context()
	user := GetUser(r)
	db := database.DB()

	var req struct {
		Resource       string  `json:"resource"`
		SlotIndex      int     `json:"slotIndex"`
		Date           string  `json:"date"`
		SlotType       string  `json:"slotType"`
		Description    string  `json:"description"`
		StartHour      int     `json:"startHour"`
		EndHour        int     `json:"endHour"`
		UtcOffset      float64 `json:"utcOffset"`
		TargetNamespace string `json:"targetNamespace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		HttpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	// System booking: creating a reservation for a namespace (e.g. shared
	// model-serving capacity) rather than for oneself. Allowed for cluster
	// admins and admins of the target namespace.
	bookingType := database.BookingTypeUser
	bookingOwner := user.Username
	createdBy := ""
	if req.TargetNamespace != "" {
		if msg, status := validateSystemBookingTarget(ctx, user, req.TargetNamespace); msg != "" {
			HttpError(w, status, msg)
			return
		}
		bookingType = database.BookingTypeSystem
		bookingOwner = req.TargetNamespace
		createdBy = user.Username
	}

	if req.SlotType != database.SlotTypeFull {
		HttpError(w, http.StatusBadRequest, "invalid_slot_type")
		return
	}

	// Validate resource type exists
	spec, ok := database.GPUSpecByType(req.Resource)
	if !ok {
		HttpError(w, http.StatusBadRequest, "invalid_resource")
		return
	}

	// Validate slot index is within resource bounds
	if req.SlotIndex < 0 || req.SlotIndex >= spec.Count {
		HttpError(w, http.StatusBadRequest, "invalid_slot_index")
		return
	}

	// Validate date format and bounds
	if !IsValidBookingDate(req.Date, BookingWindowDays, req.UtcOffset) {
		HttpError(w, http.StatusBadRequest, "invalid_date")
		return
	}

	// Check for conflicts
	rows, err := db.QueryContext(ctx,
		"SELECT id, source FROM bookings WHERE resource = ? AND slot_index = ? AND date = ?",
		req.Resource, req.SlotIndex, req.Date,
	)
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		slog.Error("failed to check booking conflicts", "error", err)
		return
	}
	var conflictIDs []string
	hasReservedConflict := false
	for rows.Next() {
		var cID, cSource string
		if err := rows.Scan(&cID, &cSource); err != nil {
			continue
		}
		if cSource == database.SourceReserved {
			hasReservedConflict = true
		}
		conflictIDs = append(conflictIDs, cID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		slog.Error("failed iterating conflict check", "error", err)
	}

	if hasReservedConflict {
		JsonResponseStatus(w, http.StatusConflict, map[string]string{"error": "slot_taken"})
		return
	}

	// Evict consumed bookings
	for _, cID := range conflictIDs {
		if _, err := db.ExecContext(ctx, "DELETE FROM bookings WHERE id = ?", cID); err != nil {
			slog.Error("failed to evict consumed booking", "bookingId", cID, "error", err)
		} else {
			slog.Info("evicted consumed booking for reservation", "bookingId", cID, "user", user.Username)
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

	utcOffset := req.UtcOffset
	if utcOffset < -12 || utcOffset > 14 {
		utcOffset = 0
	}

	_, err = db.ExecContext(ctx,
		"INSERT INTO bookings (id, user, email, resource, slot_index, date, slot_type, created_at, source, description, start_hour, end_hour, utc_offset, booking_type) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		id, bookingOwner, createdBy, req.Resource, req.SlotIndex, req.Date, req.SlotType, createdAt, database.SourceReserved, desc, startHour, endHour, utcOffset, bookingType,
	)
	if err != nil {
		JsonResponseStatus(w, http.StatusConflict, map[string]string{"error": "slot_taken"})
		return
	}

	booking := database.Booking{
		ID:          id,
		User:        bookingOwner,
		Email:       createdBy,
		Resource:    req.Resource,
		SlotIndex:   req.SlotIndex,
		Date:        req.Date,
		SlotType:    req.SlotType,
		CreatedAt:   createdAt,
		Source:      database.SourceReserved,
		Description: desc,
		StartHour:   startHour,
		EndHour:     endHour,
		UtcOffset:   utcOffset,
		BookingType: bookingType,
	}

	slog.Info("AUDIT: booking created", "user", user.Username, "bookingId", id, "resource", req.Resource, "slot", req.SlotIndex, "date", req.Date, "bookingType", bookingType, "remote_addr", r.RemoteAddr)
	JsonResponseStatus(w, http.StatusCreated, booking)
	kube.TriggerSyncReservations()
}

func DeleteBooking(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		HttpError(w, http.StatusBadRequest, "missing_id")
		return
	}
	if !IsValidBookingID(id) {
		HttpError(w, http.StatusBadRequest, "invalid_id")
		return
	}

	user := GetUser(r)
	db := database.DB()

	var owner, source, createdBy string
	err := db.QueryRowContext(ctx, "SELECT user, source, email FROM bookings WHERE id = ?", id).Scan(&owner, &source, &createdBy)
	if err == sql.ErrNoRows {
		HttpError(w, http.StatusNotFound, "not_found")
		return
	}
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		return
	}

	if source == database.SourceConsumed {
		JsonResponseStatus(w, http.StatusForbidden, map[string]string{"error": "consumed_booking"})
		return
	}

	if !canManageBooking(ctx, user, owner, createdBy, nil) {
		HttpError(w, http.StatusForbidden, "forbidden")
		return
	}

	_, err = db.ExecContext(ctx, "DELETE FROM bookings WHERE id = ?", id)
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		return
	}

	slog.Info("AUDIT: booking deleted", "user", user.Username, "bookingId", id, "owner", owner, "remote_addr", r.RemoteAddr)
	JsonResponse(w, map[string]string{"status": "deleted"})
	kube.TriggerSyncReservations()
}

func BulkCancelHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := GetUser(r)
	db := database.DB()

	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.IDs) == 0 {
		HttpError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	for _, id := range req.IDs {
		if !IsValidBookingID(id) {
			HttpError(w, http.StatusBadRequest, "invalid_id")
			return
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		slog.Error("failed to start transaction", "error", err)
		return
	}
	defer tx.Rollback()

	nsAdminCache := map[string]bool{}
	var deleted []string
	var errors []string
	for _, id := range req.IDs {
		var owner, source, createdBy string
		err := tx.QueryRowContext(ctx, "SELECT user, source, email FROM bookings WHERE id = ?", id).Scan(&owner, &source, &createdBy)
		if err == sql.ErrNoRows {
			errors = append(errors, fmt.Sprintf("%s: not_found", id))
			continue
		}
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: database_error", id))
			continue
		}
		if source == database.SourceConsumed {
			errors = append(errors, fmt.Sprintf("%s: consumed_booking", id))
			continue
		}
		if !canManageBooking(ctx, user, owner, createdBy, nsAdminCache) {
			errors = append(errors, fmt.Sprintf("%s: forbidden", id))
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM bookings WHERE id = ?", id); err != nil {
			errors = append(errors, fmt.Sprintf("%s: database_error", id))
			continue
		}
		deleted = append(deleted, id)
	}

	if err := tx.Commit(); err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		slog.Error("failed to commit bulk cancel", "error", err)
		return
	}

	if len(deleted) > 0 {
		slog.Info("AUDIT: bulk cancel", "user", user.Username, "deleted_count", len(deleted), "error_count", len(errors), "remote_addr", r.RemoteAddr)
	}

	JsonResponse(w, map[string]any{
		"deleted": deleted,
		"errors":  errors,
	})

	if len(deleted) > 0 {
		kube.TriggerSyncReservations()
	}
}

func BulkBookingHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := GetUser(r)
	db := database.DB()

	var req struct {
		Resources       map[string]int `json:"resources"`
		StartDate       string         `json:"startDate"`
		EndDate         string         `json:"endDate"`
		Description     string         `json:"description"`
		StartHour       int            `json:"startHour"`
		EndHour         int            `json:"endHour"`
		UtcOffset       float64        `json:"utcOffset"`
		TargetNamespace string         `json:"targetNamespace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		HttpError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	// System booking: creating a reservation for a namespace rather than for
	// oneself. Allowed for cluster admins and admins of the target namespace.
	bookingType := database.BookingTypeUser
	bookingOwner := user.Username
	createdBy := ""
	if req.TargetNamespace != "" {
		if msg, status := validateSystemBookingTarget(ctx, user, req.TargetNamespace); msg != "" {
			HttpError(w, status, msg)
			return
		}
		bookingType = database.BookingTypeSystem
		bookingOwner = req.TargetNamespace
		createdBy = user.Username
	}

	if req.StartDate == "" || req.EndDate == "" || len(req.Resources) == 0 {
		HttpError(w, http.StatusBadRequest, "missing_fields")
		return
	}

	if req.StartDate > req.EndDate {
		HttpError(w, http.StatusBadRequest, "invalid_date_range")
		return
	}

	// Validate all resource types exist
	for resource := range req.Resources {
		if _, ok := database.GPUSpecByType(resource); !ok {
			HttpError(w, http.StatusBadRequest, fmt.Sprintf("invalid_resource: %s", resource))
			return
		}
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

	utcOffset := req.UtcOffset
	if utcOffset < -12 || utcOffset > 14 {
		utcOffset = 0
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

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		slog.Error("failed to start transaction", "error", err)
		return
	}
	defer tx.Rollback()

	var created []database.Booking
	var errors []string

	// Collect requested resources (skip zero counts)
	var resources []string
	for resource, count := range req.Resources {
		if count > 0 {
			resources = append(resources, resource)
		}
	}

	// Batch fetch all existing bookings for requested resources and dates in one query
	type slotKey struct {
		resource string
		date     string
	}
	type slotInfo struct {
		reserved    map[int]bool
		consumedIDs map[int]string
	}
	slotMap := map[slotKey]*slotInfo{}

	if len(resources) > 0 && len(dates) > 0 {
		args := make([]any, 0, len(resources)+len(dates))
		resPlaceholders := make([]byte, 0, len(resources)*2)
		for i, r := range resources {
			if i > 0 {
				resPlaceholders = append(resPlaceholders, ',')
			}
			resPlaceholders = append(resPlaceholders, '?')
			args = append(args, r)
		}
		datePlaceholders := make([]byte, 0, len(dates)*2)
		for i, d := range dates {
			if i > 0 {
				datePlaceholders = append(datePlaceholders, ',')
			}
			datePlaceholders = append(datePlaceholders, '?')
			args = append(args, d)
		}

		query := fmt.Sprintf(
			"SELECT resource, date, slot_index, source, id FROM bookings WHERE resource IN (%s) AND date IN (%s)",
			string(resPlaceholders), string(datePlaceholders),
		)
		slotRows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			HttpError(w, http.StatusInternalServerError, "database_error")
			slog.Error("failed to batch query bookings", "error", err)
			return
		}
		for slotRows.Next() {
			var res, date, src, bid string
			var idx int
			if err := slotRows.Scan(&res, &date, &idx, &src, &bid); err != nil {
				slog.Error("failed scanning slot row", "error", err)
				continue
			}
			key := slotKey{resource: res, date: date}
			info, ok := slotMap[key]
			if !ok {
				info = &slotInfo{reserved: map[int]bool{}, consumedIDs: map[int]string{}}
				slotMap[key] = info
			}
			if src == database.SourceReserved {
				info.reserved[idx] = true
			} else {
				info.consumedIDs[idx] = bid
			}
		}
		slotRows.Close()
		if err := slotRows.Err(); err != nil {
			slog.Error("failed iterating batch slot query", "error", err)
		}
	}

	// Process each resource/date using pre-fetched data
	for _, resource := range resources {
		count := req.Resources[resource]
		for _, date := range dates {
			key := slotKey{resource: resource, date: date}
			info := slotMap[key]
			if info == nil {
				info = &slotInfo{reserved: map[int]bool{}, consumedIDs: map[int]string{}}
			}

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
				if info.reserved[unitIdx] {
					continue
				}
				if cID, ok := info.consumedIDs[unitIdx]; ok {
					if _, err := tx.ExecContext(ctx, "DELETE FROM bookings WHERE id = ?", cID); err != nil {
						slog.Error("bulk booking: failed to evict consumed booking", "bookingId", cID, "error", err)
						continue
					}
					slog.Info("bulk booking: evicted consumed booking", "bookingId", cID, "user", user.Username)
				}

				id := fmt.Sprintf("booking-%d", time.Now().UnixNano())
				createdAt := time.Now().UTC().Format(time.RFC3339)

				_, err := tx.ExecContext(ctx,
					"INSERT INTO bookings (id, user, email, resource, slot_index, date, slot_type, created_at, source, description, start_hour, end_hour, utc_offset, booking_type) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
					id, bookingOwner, createdBy, resource, unitIdx, date, database.SlotTypeFull, createdAt, database.SourceReserved, desc, startHour, endHour, utcOffset, bookingType,
				)
				if err != nil {
					slog.Error("bulk booking: insert failed", "resource", resource, "slot", unitIdx, "date", date, "error", err)
					continue
				}

				created = append(created, database.Booking{
					ID:          id,
					User:        bookingOwner,
					Email:       createdBy,
					Resource:    resource,
					SlotIndex:   unitIdx,
					Date:        date,
					SlotType:    database.SlotTypeFull,
					CreatedAt:   createdAt,
					Source:      database.SourceReserved,
					Description: desc,
					StartHour:   startHour,
					EndHour:     endHour,
					UtcOffset:   utcOffset,
					BookingType: bookingType,
				})
				booked++
			}
			if booked < count {
				errors = append(errors, fmt.Sprintf("%s on %s: only %d of %d slots available", resource, date, booked, count))
			}
		}
	}

	if len(created) == 0 && len(errors) > 0 {
		// Rollback is handled by defer
		JsonResponseStatus(w, http.StatusConflict, map[string]any{"error": "no_slots_available", "details": errors})
		return
	}

	if err := tx.Commit(); err != nil {
		HttpError(w, http.StatusInternalServerError, "database_error")
		slog.Error("failed to commit bulk booking", "error", err)
		return
	}

	slog.Info("AUDIT: bulk booking created", "user", user.Username, "created_count", len(created), "error_count", len(errors), "start_date", req.StartDate, "end_date", req.EndDate, "bookingType", bookingType, "remote_addr", r.RemoteAddr)

	JsonResponseStatus(w, http.StatusCreated, map[string]any{
		"bookings": created,
		"errors":   errors,
	})

	if len(created) > 0 {
		kube.TriggerSyncReservations()
	}
}
