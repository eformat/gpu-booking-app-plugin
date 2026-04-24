package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetUserFromContext(t *testing.T) {
	user := &UserInfo{Username: "alice", Groups: []string{"team-a"}, IsAdmin: true}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = reqWithUser(req, user)

	got := GetUser(req)
	if got.Username != "alice" {
		t.Errorf("Username = %q, want alice", got.Username)
	}
	if !got.IsAdmin {
		t.Error("expected IsAdmin = true")
	}
	if len(got.Groups) != 1 || got.Groups[0] != "team-a" {
		t.Errorf("Groups = %v, want [team-a]", got.Groups)
	}
}

func TestGetUserMissingContext(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	got := GetUser(req)
	if got.Username != "" {
		t.Errorf("Username = %q, want empty", got.Username)
	}
	if got.IsAdmin {
		t.Error("expected IsAdmin = false for missing context")
	}
}

func TestMeHandler(t *testing.T) {
	user := &UserInfo{Username: "bob", Groups: []string{"devs"}, IsAdmin: false}
	req := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	req = reqWithUser(req, user)
	w := httptest.NewRecorder()

	MeHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var got UserInfo
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Username != "bob" {
		t.Errorf("Username = %q, want bob", got.Username)
	}
	if got.IsAdmin {
		t.Error("expected IsAdmin = false")
	}
}

func TestAuthMiddlewareHealthBypass(t *testing.T) {
	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("health endpoint should bypass auth, got %d", w.Code)
	}
}

func TestAuthMiddlewareNoTokenNoDevMode(t *testing.T) {
	origDevMode := DevMode
	DevMode = false
	defer func() { DevMode = origDevMode }()

	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/bookings", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestAuthMiddlewareDevModeNoToken(t *testing.T) {
	origDevMode := DevMode
	DevMode = true
	defer func() { DevMode = origDevMode }()

	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := GetUser(r)
		JsonResponse(w, user)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/bookings", nil)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var got UserInfo
	json.NewDecoder(w.Body).Decode(&got)
	if got.Username != "dev-admin" {
		t.Errorf("Username = %q, want dev-admin", got.Username)
	}
	if !got.IsAdmin {
		t.Error("expected IsAdmin = true in DevMode")
	}
}

func TestAuthMiddlewareDevModeWithBearerToken(t *testing.T) {
	origDevMode := DevMode
	DevMode = true
	defer func() { DevMode = origDevMode }()

	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := GetUser(r)
		JsonResponse(w, user)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/bookings", nil)
	req.Header.Set("Authorization", "Bearer some-fake-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var got UserInfo
	json.NewDecoder(w.Body).Decode(&got)
	if got.Username != "dev-admin" {
		t.Errorf("Username = %q, want dev-admin", got.Username)
	}
}

func TestAuthMiddlewareCacheHit(t *testing.T) {
	origDevMode := DevMode
	DevMode = false
	defer func() { DevMode = origDevMode }()

	token := "test-cache-token-12345"
	hash := sha256.Sum256([]byte(token))
	cacheKey := hex.EncodeToString(hash[:])

	authCache.Store(cacheKey, &cachedUser{
		info:      &UserInfo{Username: "cached-user", Groups: []string{"team-x"}, IsAdmin: true},
		expiresAt: time.Now().Add(5 * time.Minute),
	})
	defer authCache.Delete(cacheKey)

	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := GetUser(r)
		JsonResponse(w, user)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/bookings", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var got UserInfo
	json.NewDecoder(w.Body).Decode(&got)
	if got.Username != "cached-user" {
		t.Errorf("Username = %q, want cached-user", got.Username)
	}
	if !got.IsAdmin {
		t.Error("expected IsAdmin = true from cache")
	}
}

func TestAuthMiddlewareExpiredCache(t *testing.T) {
	origDevMode := DevMode
	DevMode = false
	defer func() { DevMode = origDevMode }()

	token := "test-expired-token-99999"
	hash := sha256.Sum256([]byte(token))
	cacheKey := hex.EncodeToString(hash[:])

	authCache.Store(cacheKey, &cachedUser{
		info:      &UserInfo{Username: "stale-user"},
		expiresAt: time.Now().Add(-1 * time.Minute),
	})
	defer authCache.Delete(cacheKey)

	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/bookings", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	// Expired cache + no authClient + no DevMode = 401
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expired cache should not authenticate, got %d", w.Code)
	}
}

func TestAuthMiddlewareInvalidAuthHeader(t *testing.T) {
	origDevMode := DevMode
	DevMode = false
	defer func() { DevMode = origDevMode }()

	handler := AuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/bookings", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("non-Bearer auth should be rejected, got %d", w.Code)
	}
}
