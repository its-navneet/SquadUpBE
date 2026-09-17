package models_test

import (
	"encoding/json"
	"testing"
	"time"

	"squadup/backend/internal/models"
)

func TestUserJSONSerialization(t *testing.T) {
	rawJSON := `{
		"name": "Cristiano",
		"email": "cr7@squadup.app",
		"age": 39,
		"height_cm": 187.5,
		"weight_kg": 83.0,
		"preferred_foot": "Right",
		"bio": "Football legend"
	}`

	var u models.User
	if err := json.Unmarshal([]byte(rawJSON), &u); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if u.Name != "Cristiano" {
		t.Errorf("expected Cristiano, got %s", u.Name)
	}
	if u.Age != 39 {
		t.Errorf("expected 39, got %d", u.Age)
	}
	if u.HeightCM != 187.5 {
		t.Errorf("expected 187.5, got %f", u.HeightCM)
	}
	if u.WeightKG != 83.0 {
		t.Errorf("expected 83.0, got %f", u.WeightKG)
	}
	if u.PreferredFoot != "Right" {
		t.Errorf("expected Right, got %s", u.PreferredFoot)
	}

	// Test marshaling back to JSON includes snake_case keys
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unexpected unmarshal to map error: %v", err)
	}

	if m["height_cm"] != 187.5 {
		t.Errorf("expected height_cm to be 187.5 in JSON output, got %v", m["height_cm"])
	}
	if m["weight_kg"] != 83.0 {
		t.Errorf("expected weight_kg to be 83 in JSON output, got %v", m["weight_kg"])
	}
	if m["preferred_foot"] != "Right" {
		t.Errorf("expected preferred_foot to be Right in JSON output, got %v", m["preferred_foot"])
	}
}

func TestMatchModelProperties(t *testing.T) {
	rawJSON := `{
		"name": "Sunday League Derby",
		"format": "7v7",
		"duration_minutes": 60,
		"team_count": 2,
		"players_per_team": 7,
		"max_players": 14,
		"notes": "Bring black and white bibs",
		"status": "UPCOMING"
	}`

	var match models.Match
	if err := json.Unmarshal([]byte(rawJSON), &match); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}

	if match.Name != "Sunday League Derby" {
		t.Errorf("expected match name 'Sunday League Derby', got %s", match.Name)
	}
	if match.Format != "7v7" {
		t.Errorf("expected format '7v7', got %s", match.Format)
	}
	if match.Status != "UPCOMING" {
		t.Errorf("expected status 'UPCOMING', got %s", match.Status)
	}
	if match.DurationMinutes != 60 {
		t.Errorf("expected duration 60, got %d", match.DurationMinutes)
	}
	if match.TeamCount != 2 {
		t.Errorf("expected team count 2, got %d", match.TeamCount)
	}
	if match.MaxPlayers != 14 {
		t.Errorf("expected max players 14, got %d", match.MaxPlayers)
	}
}

func TestUserMediaSignerPresignsPhoto(t *testing.T) {
	models.MediaSigner = func(bucket, key, rawURL string) string {
		return "https://presigned.s3.amazonaws.com/" + bucket + "/" + key + "?signature=abc"
	}
	defer func() { models.MediaSigner = nil }()

	u := models.User{
		Name:               "Leo",
		ProfilePhotoBucket: "my-bucket",
		ProfilePhotoKey:    "profiles/leo.jpg",
		ProfilePhotoURL:    "https://original-url.com/profiles/leo.jpg",
	}

	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	expected := "https://presigned.s3.amazonaws.com/my-bucket/profiles/leo.jpg?signature=abc"
	if m["profile_photo_url"] != expected {
		t.Errorf("expected %q, got %q", expected, m["profile_photo_url"])
	}
}

func TestUserPositionAndKitNumberSerialization(t *testing.T) {
	rawJSON := `{
		"name": "Luka Modric",
		"email": "luka@squadup.app",
		"position": "MID",
		"kit_number": 10
	}`

	var u models.User
	if err := json.Unmarshal([]byte(rawJSON), &u); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if u.Position != "MID" {
		t.Errorf("expected position MID, got %s", u.Position)
	}
	if u.KitNumber != 10 {
		t.Errorf("expected kit_number 10, got %d", u.KitNumber)
	}

	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if m["position"] != "MID" {
		t.Errorf("expected position MID in JSON, got %v", m["position"])
	}
	if m["kit_number"] != float64(10) {
		t.Errorf("expected kit_number 10 in JSON, got %v", m["kit_number"])
	}
}

func TestCalculateAgeFromDOB(t *testing.T) {
	// Empty DOB
	if _, err := models.CalculateAgeFromDOB(""); err == nil {
		t.Error("expected error for empty DOB")
	}

	// Invalid format
	if _, err := models.CalculateAgeFromDOB("not-a-date"); err == nil {
		t.Error("expected error for invalid DOB string")
	}

	// Exactly 20 years ago
	now := time.Now()
	dob20 := now.AddDate(-20, 0, 0).Format("2006-01-02")
	age20, err := models.CalculateAgeFromDOB(dob20)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if age20 != 20 {
		t.Errorf("expected 20, got %d", age20)
	}

	// Birthday was yesterday (25 years ago + 1 day older)
	dobYesterday := now.AddDate(-25, 0, -1).Format("2006-01-02")
	ageYesterday, err := models.CalculateAgeFromDOB(dobYesterday)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ageYesterday != 25 {
		t.Errorf("expected 25, got %d", ageYesterday)
	}

	// Birthday is tomorrow (turning 25 tomorrow, so currently 24)
	dobTomorrow := now.AddDate(-25, 0, 1).Format("2006-01-02")
	ageTomorrow, err := models.CalculateAgeFromDOB(dobTomorrow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ageTomorrow != 24 {
		t.Errorf("expected 24, got %d", ageTomorrow)
	}
}

func TestUserDateOfBirthDynamicAgeSerialization(t *testing.T) {
	now := time.Now()
	dobStr := now.AddDate(-22, 0, 0).Format("2006-01-02")

	u := models.User{
		Name:        "Jude Bellingham",
		Email:       "jude@squadup.app",
		DateOfBirth: dobStr,
		Age:         0, // Age in struct is 0, but MarshalJSON should calculate 22
	}

	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if m["date_of_birth"] != dobStr {
		t.Errorf("expected date_of_birth %s, got %v", dobStr, m["date_of_birth"])
	}
	if m["age"] != float64(22) {
		t.Errorf("expected age 22 dynamically calculated in JSON, got %v", m["age"])
	}
}

func TestGroupJoinRequestJSONSerialization(t *testing.T) {
	reqID := "550e8400-e29b-41d4-a716-446655440000"
	groupID := "550e8400-e29b-41d4-a716-446655440001"
	userID := "550e8400-e29b-41d4-a716-446655440002"

	raw := `{
		"id": "` + reqID + `",
		"group_id": "` + groupID + `",
		"user_id": "` + userID + `",
		"status": "PENDING"
	}`

	var r models.GroupJoinRequest
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if r.Status != "PENDING" {
		t.Errorf("expected PENDING, got %s", r.Status)
	}
	if r.GroupID.String() != groupID {
		t.Errorf("expected %s, got %s", groupID, r.GroupID.String())
	}
	if r.UserID.String() != userID {
		t.Errorf("expected %s, got %s", userID, r.UserID.String())
	}
}

func TestPlayerStatisticsAndCareerStatsSavesAndFouls(t *testing.T) {
	raw := `{
		"matches": 5,
		"goals": 2,
		"clean_sheets": 3,
		"saves": 14,
		"fouls": 4
	}`

	var ps models.PlayerStatistics
	if err := json.Unmarshal([]byte(raw), &ps); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if ps.Saves != 14 {
		t.Errorf("expected 14 saves, got %d", ps.Saves)
	}
	if ps.Fouls != 4 {
		t.Errorf("expected 4 fouls, got %d", ps.Fouls)
	}

	var cs models.CareerStats
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if cs.Saves != 14 {
		t.Errorf("expected 14 saves, got %d", cs.Saves)
	}
	if cs.Fouls != 4 {
		t.Errorf("expected 4 fouls, got %d", cs.Fouls)
	}
}
