package team_test

import (
	"testing"

	"github.com/google/uuid"
	"squadup/backend/internal/team"
)

func TestGenerate_SliceImmutability(t *testing.T) {
	p1 := team.Player{UserID: uuid.New(), Name: "Low", Rating: 4.0}
	p2 := team.Player{UserID: uuid.New(), Name: "High", Rating: 9.0}
	p3 := team.Player{UserID: uuid.New(), Name: "Mid", Rating: 6.5}
	orig := []team.Player{p1, p2, p3}

	teams := team.Generate(orig, 2)
	if len(teams) != 2 {
		t.Fatalf("expected 2 teams, got %d", len(teams))
	}

	// Verify original slice order was preserved
	if orig[0].Name != "Low" || orig[1].Name != "High" || orig[2].Name != "Mid" {
		t.Errorf("original slice was mutated: %+v", orig)
	}
}

func TestGenerate_GoalkeeperDistribution(t *testing.T) {
	gk1 := team.Player{UserID: uuid.New(), Name: "GK1", Rating: 8.0, Goalkeeper: true}
	gk2 := team.Player{UserID: uuid.New(), Name: "GK2", Rating: 7.0, Position: "GK"}
	f1 := team.Player{UserID: uuid.New(), Name: "F1", Rating: 9.0, Position: "ST"}
	f2 := team.Player{UserID: uuid.New(), Name: "F2", Rating: 6.0, Position: "DEF"}

	players := []team.Player{gk1, gk2, f1, f2}
	teams := team.Generate(players, 2)

	// Both teams should have exactly 1 goalkeeper
	for i, tm := range teams {
		gkCount := 0
		for _, p := range tm {
			if p.Goalkeeper || p.Position == "GK" {
				gkCount++
			}
		}
		if gkCount != 1 {
			t.Errorf("team %d expected 1 goalkeeper, got %d", i, gkCount)
		}
	}
}
