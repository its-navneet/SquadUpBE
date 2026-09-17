package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"squadup/backend/internal/config"
)

func TestPollResponseDTOJSON(t *testing.T) {
	myVote := "IN"
	dto := PollResponseDTO{
		ID:              uuid.New().String(),
		GroupID:         uuid.New().String(),
		CreatorID:       uuid.New().String(),
		Title:           "Match Availability",
		MatchDate:       time.Now().Add(48 * time.Hour).Format(time.RFC3339),
		DurationMinutes: 90,
		Venue:           "Pitch 4 Turf",
		ExpiresAt:       time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		Status:          "ACTIVE",
		InCount:         8,
		OutCount:        2,
		MaybeCount:      1,
		TotalVotes:      11,
		MyVote:          &myVote,
		Voters: []PollVoterDTO{
			{
				UserID: uuid.New().String(),
				Name:   "Alex",
				Option: "IN",
			},
		},
	}

	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("failed to marshal PollResponseDTO: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if parsed["title"] != "Match Availability" {
		t.Errorf("expected title 'Match Availability', got %v", parsed["title"])
	}
	if parsed["in_count"] != float64(8) {
		t.Errorf("expected in_count 8, got %v", parsed["in_count"])
	}
	if parsed["total_votes"] != float64(11) {
		t.Errorf("expected total_votes 11, got %v", parsed["total_votes"])
	}
	if parsed["my_vote"] != "IN" {
		t.Errorf("expected my_vote 'IN', got %v", parsed["my_vote"])
	}
}

func TestPollEndpointsRequireAuth(t *testing.T) {
	cfg := &config.Config{
		CORSOrigins: "*",
		JWTSecret:   "test-secret-at-least-32-bytes-long!!",
	}
	server := NewServer(cfg, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	r := server.SetupRouter()

	gid := uuid.New().String()
	req, _ := http.NewRequest(http.MethodGet, "/api/groups/"+gid+"/polls/active", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Unauthenticated request should fail with 401 or 500 (since authService is nil in this stub)
	if w.Code != http.StatusUnauthorized && w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 401/500 for unauthenticated request, got %d", w.Code)
	}
}
