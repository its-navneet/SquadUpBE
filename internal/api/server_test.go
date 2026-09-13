package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"squadup/backend/internal/config"

	"github.com/google/uuid"
)

func TestHealthEndpoint(t *testing.T) {
	cfg := &config.Config{
		CORSOrigins: "*",
	}
	server := NewServer(cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	r := server.SetupRouter()

	req, err := http.NewRequest(http.MethodGet, "/health", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", w.Code)
	}

	expected := `{"status":"ok"}`
	if w.Body.String() != expected {
		t.Fatalf("expected body %s, got %s", expected, w.Body.String())
	}
}

func TestHelpers(t *testing.T) {
	// mustUUID
	validID := uuid.New()
	if mustUUID(validID.String()) != validID {
		t.Errorf("expected %v, got %v", validID, mustUUID(validID.String()))
	}
	if mustUUID("invalid-uuid") != uuid.Nil {
		t.Errorf("expected Nil UUID on invalid input, got %v", mustUUID("invalid-uuid"))
	}

	// defaultStr and defaultInt
	if defaultStr("", "fallback") != "fallback" || defaultStr("value", "fallback") != "value" {
		t.Errorf("defaultStr logic failed")
	}
	if defaultInt(0, 5) != 5 || defaultInt(10, 5) != 10 {
		t.Errorf("defaultInt logic failed")
	}

	// teamName and teamCode
	if teamName(0) != "Red" || teamName(1) != "Blue" {
		t.Errorf("teamName failed")
	}
	if teamCode(0) != "RED" || teamCode(1) != "BLU" {
		t.Errorf("teamCode failed")
	}

	// normalizePosition
	if normalizePosition("goalkeeper") != "GK" {
		t.Errorf("expected GK, got %s", normalizePosition("goalkeeper"))
	}
	if normalizePosition("defender") != "DEF" || normalizePosition("cb") != "DEF" {
		t.Errorf("expected DEF, got %s", normalizePosition("defender"))
	}
	if normalizePosition("midfielder") != "MID" {
		t.Errorf("expected MID, got %s", normalizePosition("midfielder"))
	}
	if normalizePosition("striker") != "ST" {
		t.Errorf("expected ST, got %s", normalizePosition("striker"))
	}

	// resolveVenue
	venue, mapURL := resolveVenue("Turf Park https://maps.google.com/?q=1,2", "")
	if venue != "Turf Park" || mapURL != "https://maps.google.com/?q=1,2" {
		t.Errorf("resolveVenue failed: venue=%s, mapURL=%s", venue, mapURL)
	}
}

func TestServerWithRedisComponents(t *testing.T) {
	cfg := &config.Config{
		CORSOrigins: "*",
	}
	server := NewServer(cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if server.locker == nil || server.cache == nil || server.queue == nil {
		t.Fatal("expected default fallback locker, cache, and queue to be initialized")
	}

	ctx := t.Context()
	// Test locker
	tok, acquired, err := server.locker.Acquire(ctx, "test:key", 1)
	if err != nil || !acquired || tok == "" {
		t.Fatalf("expected lock acquisition to succeed: err=%v, acquired=%v", err, acquired)
	}
	released, err := server.locker.Release(ctx, "test:key", tok)
	if err != nil || !released {
		t.Fatalf("expected lock release to succeed")
	}

	// Test cache
	type TestData struct {
		Name string `json:"name"`
	}
	err = server.cache.Set(ctx, "test:data", TestData{Name: "SquadUp"}, 10*time.Second)
	if err != nil {
		t.Fatalf("cache.Set failed: %v", err)
	}
	var res TestData
	found, err := server.cache.Get(ctx, "test:data", &res)
	if err != nil || !found || res.Name != "SquadUp" {
		t.Fatalf("expected cached data to be retrieved: found=%v, err=%v, res=%+v", found, err, res)
	}

	_ = server.cache.Delete(ctx, "test:data")
	foundAfterDel, _ := server.cache.Get(ctx, "test:data", &res)
	if foundAfterDel {
		t.Fatal("expected data to be deleted from cache")
	}
}
