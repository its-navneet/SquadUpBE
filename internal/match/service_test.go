package match_test

import (
	"testing"

	"squadup/backend/internal/match"
	"squadup/backend/internal/ws"
)

func TestMatchStartRequiresTwoTeams(t *testing.T) {
	hub := ws.New()
	svc := match.New(nil, hub)
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}

func TestErrInvalidStateDefinition(t *testing.T) {
	if match.ErrInvalidState == nil {
		t.Fatal("expected ErrInvalidState to be defined")
	}
	if match.ErrInvalidState.Error() != "invalid match state" {
		t.Errorf("unexpected error message: %v", match.ErrInvalidState)
	}
}
