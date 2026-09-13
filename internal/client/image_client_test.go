package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"testing"
)

type mockTransport func(req *http.Request) (*http.Response, error)

func (m mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m(req)
}

func TestImageClient_MissingAPIKey(t *testing.T) {
	c := NewImageClient("", "")
	_, err := c.Generate(context.Background(), "test prompt")
	if err == nil {
		t.Fatal("expected error when API key is missing, got nil")
	}
}

func TestImageClient_ImagenSuccess(t *testing.T) {
	fakeImagePNG := []byte("FAKE_PNG_BYTES_FOR_TESTING")
	b64Data := base64.StdEncoding.EncodeToString(fakeImagePNG)

	httpClient := &http.Client{
		Transport: mockTransport(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("x-goog-api-key") != "test-api-key" {
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Body:       io.NopCloser(bytes.NewBufferString("unauthorized")),
				}, nil
			}
			jsonBody := `{
				"predictions": [
					{
						"bytesBase64Encoded": "` + b64Data + `",
						"mimeType": "image/png"
					}
				]
			}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewBufferString(jsonBody)),
			}, nil
		}),
	}

	c := &ImageClient{
		APIKey: "test-api-key",
		Model:  "imagen-3.0-generate-002",
		Client: httpClient,
	}

	data, err := c.Generate(context.Background(), "a football match poster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != string(fakeImagePNG) {
		t.Fatalf("expected %q, got %q", string(fakeImagePNG), string(data))
	}
}

func TestImageClient_GeminiSuccess(t *testing.T) {
	fakeImagePNG := []byte("GEMINI_IMAGE_BYTES")
	b64Data := base64.StdEncoding.EncodeToString(fakeImagePNG)

	httpClient := &http.Client{
		Transport: mockTransport(func(r *http.Request) (*http.Response, error) {
			jsonBody := `{
				"candidates": [
					{
						"content": {
							"parts": [
								{
									"inlineData": {
										"mimeType": "image/png",
										"data": "` + b64Data + `"
									}
								}
							]
						}
					}
				]
			}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewBufferString(jsonBody)),
			}, nil
		}),
	}

	c := &ImageClient{
		APIKey: "test-api-key",
		Model:  "gemini-2.5-flash",
		Client: httpClient,
	}

	data, err := c.Generate(context.Background(), "a football match poster")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != string(fakeImagePNG) {
		t.Fatalf("expected %q, got %q", string(fakeImagePNG), string(data))
	}
}
