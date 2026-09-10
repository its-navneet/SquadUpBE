package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"squadup/backend/internal/config"
)

func TestLocalStorageUploadAndDelete(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "squadup_storage_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	s := NewLocalStorage(tempDir, "http://localhost:8080")
	if s.IsConfigured() {
		t.Errorf("LocalStorage should report IsConfigured = false")
	}

	testData := []byte("fake-image-bytes")
	url, err := s.Upload(context.Background(), "profiles/test-user.jpg", testData, "image/jpeg")
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	expectedURL := "http://localhost:8080/" + tempDir + "/profiles/test-user.jpg"
	if url != expectedURL {
		t.Errorf("Expected URL %q, got %q", expectedURL, url)
	}

	savedFile := filepath.Join(tempDir, "profiles", "test-user.jpg")
	content, err := os.ReadFile(savedFile)
	if err != nil {
		t.Fatalf("Saved file not found: %v", err)
	}
	if string(content) != string(testData) {
		t.Errorf("File content mismatch")
	}

	if err := s.Delete(context.Background(), "profiles/test-user.jpg"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := os.Stat(savedFile); !os.IsNotExist(err) {
		t.Errorf("File should have been deleted")
	}
}

func TestNewFallbackWhenUnconfigured(t *testing.T) {
	cfg := config.Config{}
	s := New(cfg)
	if s == nil {
		t.Fatalf("Storage should not be nil")
	}
	if s.IsConfigured() {
		t.Errorf("Should fall back to LocalStorage when B2 credentials are empty")
	}
}
