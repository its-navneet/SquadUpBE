package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
