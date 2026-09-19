package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"squadup/backend/internal/config"
	"squadup/backend/internal/models"
)

func TestUserRatingSerialization(t *testing.T) {
	u := models.User{
		Name:          "Lionel Messi",
		Email:         "messi@intermiami.com",
		OverallRating: 9.4,
		RatingsCount:  12,
		SkillAttributes: map[string]float64{
			"pace":      8.8,
			"shooting":  9.6,
			"passing":   9.8,
			"dribbling": 9.9,
			"defending": 5.2,
			"physical":  7.4,
		},
	}

	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("failed to marshal user with athlete rating: %v", err)
	}

	var res map[string]any
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if res["overall_rating"] != 9.4 {
		t.Errorf("expected overall_rating 9.4, got %v", res["overall_rating"])
	}
	if res["ratings_count"] != float64(12) {
		t.Errorf("expected ratings_count 12, got %v", res["ratings_count"])
	}
	attrs, ok := res["skill_attributes"].(map[string]any)
	if !ok {
		t.Fatalf("expected skill_attributes map, got %v", res["skill_attributes"])
	}
	if attrs["pace"] != 8.8 || attrs["shooting"] != 9.6 || attrs["passing"] != 9.8 {
		t.Errorf("unexpected skill_attributes values: %v", attrs)
	}
}

func TestNewEndpointsAuthRequirement(t *testing.T) {
	cfg := &config.Config{
		CORSOrigins: "*",
		JWTSecret:   "test-secret-at-least-32-bytes-long!!",
	}
	server := NewServer(cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	r := server.SetupRouter()

	gid := uuid.New().String()
	vid := uuid.New().String()
	mid := uuid.New().String()

	// 1. PUT venue
	req, _ := http.NewRequest(http.MethodPut, "/api/groups/"+gid+"/venues/"+vid, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusInternalServerError {
		t.Errorf("expected 401/500 for unauth PUT venue, got %d", w.Code)
	}

	// 2. DELETE venue
	req, _ = http.NewRequest(http.MethodDelete, "/api/groups/"+gid+"/venues/"+vid, nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusInternalServerError {
		t.Errorf("expected 401/500 for unauth DELETE venue, got %d", w.Code)
	}

	// 3. DELETE group
	req, _ = http.NewRequest(http.MethodDelete, "/api/groups/"+gid, nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusInternalServerError {
		t.Errorf("expected 401/500 for unauth DELETE group, got %d", w.Code)
	}

	// 4. DELETE chat message
	req, _ = http.NewRequest(http.MethodDelete, "/api/groups/"+gid+"/chat/"+mid, nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusInternalServerError {
		t.Errorf("expected 401/500 for unauth DELETE chat, got %d", w.Code)
	}

	// 5. PUT password
	req, _ = http.NewRequest(http.MethodPut, "/api/users/me/password", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusInternalServerError {
		t.Errorf("expected 401/500 for unauth PUT password, got %d", w.Code)
	}
}

func TestResetPasswordValidation(t *testing.T) {
	cfg := &config.Config{
		CORSOrigins: "*",
	}
	server := NewServer(cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	r := server.SetupRouter()

	// Short password (< 8 chars)
	payload := map[string]string{
		"email":        "test@example.com",
		"new_password": "short",
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, "/api/auth/reset-password", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for short password, got %d", w.Code)
	}

	// Empty email
	payload = map[string]string{
		"email":        "",
		"new_password": "validpassword123",
	}
	body, _ = json.Marshal(payload)
	req, _ = http.NewRequest(http.MethodPost, "/api/auth/reset-password", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for empty email, got %d", w.Code)
	}
}

func TestUpsertUserRatingPayloadBinding(t *testing.T) {
	// 1. Verify rated_user_id snake_case binding
	snakeJSON := []byte(`{
		"rated_user_id": "b11a9172-8d76-4d2b-9366-0775e7a9b011",
		"overall": 8.5,
		"attributes": {"pace": 8.0, "shooting": 9.0}
	}`)
	var inSnake struct {
		RatedUserID      string             `json:"rated_user_id"`
		RatedUserIDCamel string             `json:"ratedUserId"`
		Overall          float64            `json:"overall"`
		Attributes       map[string]float64 `json:"attributes"`
	}
	if err := json.Unmarshal(snakeJSON, &inSnake); err != nil {
		t.Fatalf("failed to unmarshal snake_case JSON: %v", err)
	}
	if inSnake.RatedUserID != "b11a9172-8d76-4d2b-9366-0775e7a9b011" {
		t.Errorf("expected rated_user_id to be populated, got '%s'", inSnake.RatedUserID)
	}
	if inSnake.Overall != 8.5 {
		t.Errorf("expected overall 8.5, got %f", inSnake.Overall)
	}

	// 2. Verify ratedUserId camelCase binding
	camelJSON := []byte(`{
		"ratedUserId": "b11a9172-8d76-4d2b-9366-0775e7a9b011",
		"overall": 7.0,
		"attributes": {"passing": 7.5}
	}`)
	var inCamel struct {
		RatedUserID      string             `json:"rated_user_id"`
		RatedUserIDCamel string             `json:"ratedUserId"`
		Overall          float64            `json:"overall"`
		Attributes       map[string]float64 `json:"attributes"`
	}
	if err := json.Unmarshal(camelJSON, &inCamel); err != nil {
		t.Fatalf("failed to unmarshal camelCase JSON: %v", err)
	}
	if inCamel.RatedUserIDCamel != "b11a9172-8d76-4d2b-9366-0775e7a9b011" {
		t.Errorf("expected ratedUserId to be populated, got '%s'", inCamel.RatedUserIDCamel)
	}
}
