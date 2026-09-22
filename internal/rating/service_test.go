package rating_test

import (
	"testing"

	"squadup/backend/internal/rating"

	"github.com/google/uuid"
)

func TestServiceNew(t *testing.T) {
	svc := rating.New(nil)
	if svc == nil {
		t.Fatal("expected non-nil rating service")
	}
}

func TestBatchAveragesEmptyInput(t *testing.T) {
	svc := rating.New(nil)
	res, err := svc.BatchAverages(uuid.New(), []uuid.UUID{})
	if err != nil {
		t.Fatalf("unexpected error for empty user list: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("expected empty map, got %v", res)
	}
}
