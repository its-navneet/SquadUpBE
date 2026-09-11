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

func TestLocalStorageDeleteByURL(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "squadup_storage_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	s := NewLocalStorage(tempDir, "http://localhost:8080")
	testData := []byte("fake-image-bytes")
	url, err := s.Upload(context.Background(), "profiles/delete-me.jpg", testData, "image/jpeg")
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}

	savedFile := filepath.Join(tempDir, "profiles", "delete-me.jpg")
	if _, err := os.Stat(savedFile); err != nil {
		t.Fatalf("Expected file to exist before delete: %v", err)
	}

	// Delete by URL
	if err := s.DeleteByURL(context.Background(), url); err != nil {
		t.Fatalf("DeleteByURL failed: %v", err)
	}
	if _, err := os.Stat(savedFile); !os.IsNotExist(err) {
		t.Errorf("File should have been deleted by URL")
	}
}

func TestExtractKeyFromURL(t *testing.T) {
	bucket := "my-squadup-bucket"

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Virtual hosted S3 URL",
			input:    "https://my-squadup-bucket.s3.ap-south-1.amazonaws.com/profiles/avatar123.jpg",
			expected: "profiles/avatar123.jpg",
		},
		{
			name:     "Path style S3 URL",
			input:    "https://s3.ap-south-1.amazonaws.com/my-squadup-bucket/profiles/avatar123.jpg",
			expected: "profiles/avatar123.jpg",
		},
		{
			name:     "Local absolute URL",
			input:    "http://localhost:8080/uploads/profiles/avatar123.jpg",
			expected: "profiles/avatar123.jpg",
		},
		{
			name:     "Local relative URL",
			input:    "/uploads/profiles/avatar123.jpg",
			expected: "profiles/avatar123.jpg",
		},
		{
			name:     "Direct key",
			input:    "profiles/avatar123.jpg",
			expected: "profiles/avatar123.jpg",
		},
		{
			name:     "Posters key",
			input:    "https://my-squadup-bucket.s3.ap-south-1.amazonaws.com/posters/match-123.png",
			expected: "posters/match-123.png",
		},
		{
			name:     "Group crest key",
			input:    "https://my-squadup-bucket.s3.ap-south-1.amazonaws.com/groups/crest-123.png",
			expected: "groups/crest-123.png",
		},
		{
			name:     "External unmanaged URL",
			input:    "https://lh3.googleusercontent.com/a/random-avatar.jpg",
			expected: "",
		},
		{
			name:     "Path traversal attempt",
			input:    "https://my-squadup-bucket.s3.ap-south-1.amazonaws.com/profiles/../../etc/passwd",
			expected: "",
		},
		{
			name:     "Empty input",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractKeyFromURL(tt.input, bucket)
			if got != tt.expected {
				t.Errorf("ExtractKeyFromURL(%q, %q) = %q; want %q", tt.input, bucket, got, tt.expected)
			}
		})
	}
}

func TestNewFallbackWhenUnconfigured(t *testing.T) {
	cfg := config.Config{}
	s := New(cfg)
	if s == nil {
		t.Fatalf("Storage should not be nil")
	}
	if s.IsConfigured() {
		t.Errorf("Should fall back to LocalStorage when credentials are empty")
	}
}

func TestLocalStoragePathTraversal(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "squadup_storage_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	s := NewLocalStorage(tempDir, "")
	_, err = s.Upload(context.Background(), "../outside.txt", []byte("bad"), "text/plain")
	if err == nil {
		t.Errorf("Expected error on path traversal upload, got nil")
	}
	err = s.Delete(context.Background(), "../outside.txt")
	if err == nil {
		t.Errorf("Expected error on path traversal delete, got nil")
	}
}
