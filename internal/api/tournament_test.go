package api

import (
	"testing"

	"squadup/backend/internal/models"

	"github.com/google/uuid"
)

func TestCalculateStandings(t *testing.T) {
	teamA := models.Team{
		Base: models.Base{ID: uuid.New()},
		Name: "Red",
		Code: "RED",
	}
	teamB := models.Team{
		Base: models.Base{ID: uuid.New()},
		Name: "Blue",
		Code: "BLU",
	}
	teamC := models.Team{
		Base: models.Base{ID: uuid.New()},
		Name: "Green",
		Code: "GRN",
	}
	teams := []models.Team{teamA, teamB, teamC}

	// MiniMatch 1: Red 2 - 1 Blue (Completed)
	// MiniMatch 2: Red 1 - 1 Green (Completed)
	// MiniMatch 3: Blue 0 - 3 Green (Completed)
	miniMatches := []models.MiniMatch{
		{
			GameNumber: 1,
			HomeTeamID: teamA.ID,
			AwayTeamID: teamB.ID,
			HomeScore:  2,
			AwayScore:  1,
			Status:     "COMPLETED",
		},
		{
			GameNumber: 2,
			HomeTeamID: teamA.ID,
			AwayTeamID: teamC.ID,
			HomeScore:  1,
			AwayScore:  1,
			Status:     "COMPLETED",
		},
		{
			GameNumber: 3,
			HomeTeamID: teamB.ID,
			AwayTeamID: teamC.ID,
			HomeScore:  0,
			AwayScore:  3,
			Status:     "COMPLETED",
		},
	}

	standings := calculateStandings(teams, miniMatches)
	if len(standings) != 3 {
		t.Fatalf("expected 3 standings, got %d", len(standings))
	}

	// Green: Won 1, Drawn 1, Lost 0 -> 4 pts, GD: (1-1) + (3-0) = +3
	// Red: Won 1, Drawn 1, Lost 0 -> 4 pts, GD: (2-1) + (1-1) = +1
	// Blue: Won 0, Drawn 0, Lost 2 -> 0 pts, GD: (1-2) + (0-3) = -4
	if standings[0].TeamID != teamC.ID || standings[0].Points != 4 || standings[0].GoalDifference != 3 {
		t.Errorf("expected 1st place Green with 4 pts and +3 GD, got %+v", standings[0])
	}
	if standings[1].TeamID != teamA.ID || standings[1].Points != 4 || standings[1].GoalDifference != 1 {
		t.Errorf("expected 2nd place Red with 4 pts and +1 GD, got %+v", standings[1])
	}
	if standings[2].TeamID != teamB.ID || standings[2].Points != 0 || standings[2].GoalDifference != -4 {
		t.Errorf("expected 3rd place Blue with 0 pts and -4 GD, got %+v", standings[2])
	}
}

func TestDetermineWinnerStaysPairing(t *testing.T) {
	t1 := models.Team{Base: models.Base{ID: uuid.New()}, Name: "Red"}
	t2 := models.Team{Base: models.Base{ID: uuid.New()}, Name: "Blue"}
	t3 := models.Team{Base: models.Base{ID: uuid.New()}, Name: "Green"}
	teams := []models.Team{t1, t2, t3}

	current := models.MiniMatch{
		HomeTeamID: t1.ID,
		AwayTeamID: t2.ID,
	}

	// Winner is Red (t1.ID). Green (t3.ID) was sitting.
	// Next match should be Red vs Green.
	winnerID := t1.ID
	nextHome, nextAway := determineWinnerStaysPairing(teams, current, &winnerID)
	if nextHome != t1.ID || nextAway != t3.ID {
		t.Errorf("expected next pairing Red vs Green, got home: %v away: %v", nextHome, nextAway)
	}
}
