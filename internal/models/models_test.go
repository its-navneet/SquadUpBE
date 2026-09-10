package models_test

import (
	"encoding/json"
	"testing"

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

