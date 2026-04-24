package api

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/eformat/gpu-booking-plugin/pkg/database"
)

func setupTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := database.Init(filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("Init test DB: %v", err)
	}
	t.Cleanup(database.Close)
}

func reqWithUser(r *http.Request, user *UserInfo) *http.Request {
	ctx := context.WithValue(r.Context(), userContextKey, user)
	return r.WithContext(ctx)
}

func testUser() *UserInfo {
	return &UserInfo{Username: "testuser", IsAdmin: false}
}

func testAdmin() *UserInfo {
	return &UserInfo{Username: "admin", IsAdmin: true}
}
