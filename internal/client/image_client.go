package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ImageClient calls the Google Gemini Developer API / Firebase AI Logic endpoint directly.
// It removes any Cloudflare Worker proxy dependency.
type ImageClient struct {
	APIKey string
	Model  string
	Client *http.Client
}

func NewImageClient(apiKey, model string) *ImageClient {
	if model == "" {
		model = "imagen-3.0-generate-002"
	}
	return &ImageClient{
		APIKey: strings.TrimSpace(apiKey),
		Model:  strings.TrimSpace(model),
		Client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

func (c *ImageClient) IsConfigured() bool {
	return c != nil && c.APIKey != ""
}

type imagenRequest struct {
	Instances  []imagenInstance  `json:"instances"`
	Parameters *imagenParameters `json:"parameters,omitempty"`
}

type imagenInstance struct {
	Prompt string `json:"prompt"`
}

type imagenParameters struct {
	SampleCount      int    `json:"sampleCount,omitempty"`
	AspectRatio      string `json:"aspectRatio,omitempty"`
	PersonGeneration string `json:"personGeneration,omitempty"`
}

type imagenResponse struct {
	Predictions []struct {
		BytesBase64Encoded string `json:"bytesBase64Encoded"`
		MimeType           string `json:"mimeType"`
	} `json:"predictions"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

// geminiContentResponse handles models like gemini-2.0-flash / gemini-2.5-flash with image modality
type geminiContentResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text       string `json:"text,omitempty"`
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData,omitempty"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

func (c *ImageClient) Generate(ctx context.Context, prompt string, overrideKey ...string) ([]byte, error) {
	apiKey := c.APIKey
	if len(overrideKey) > 0 && strings.TrimSpace(overrideKey[0]) != "" {
		apiKey = strings.TrimSpace(overrideKey[0])
	}
	if apiKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY is not configured")
	}

	model := c.Model
	if model == "" {
		model = "imagen-3.0-generate-002"
	}

	httpClient := c.Client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 120 * time.Second}
	}

	var endpoint string
	var reqBody []byte
	var err error

	isGeminiGenerative := strings.HasPrefix(model, "gemini-")

	if isGeminiGenerative {
		endpoint = fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", model)
		geminiPayload := map[string]any{
			"contents": []map[string]any{
				{
					"parts": []map[string]any{
						{"text": prompt},
					},
				},
			},
			"generationConfig": map[string]any{
				"responseModalities": []string{"IMAGE"},
			},
		}
		reqBody, err = json.Marshal(geminiPayload)
	} else {
		endpoint = fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:predict", model)
		payload := imagenRequest{
			Instances: []imagenInstance{
				{Prompt: prompt},
			},
			Parameters: &imagenParameters{
				SampleCount:      1,
				AspectRatio:      "4:5",
				PersonGeneration: "allow_adult",
			},
		}
		reqBody, err = json.Marshal(payload)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if reqErr != nil {
			return nil, reqErr
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", apiKey)

		resp, doErr := httpClient.Do(req)
		if doErr != nil {
			lastErr = doErr
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
				continue
			}
		}

		respBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("image generation failed (status %d): %s", resp.StatusCode, string(respBytes))
			if resp.StatusCode >= 500 && attempt < 3 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(time.Duration(attempt) * time.Second):
					continue
				}
			}
			return nil, lastErr
		}

		// Parse response
		if isGeminiGenerative {
			var gResp geminiContentResponse
			if err := json.Unmarshal(respBytes, &gResp); err != nil {
				return nil, fmt.Errorf("failed to parse gemini response: %w", err)
			}
			if gResp.Error != nil {
				return nil, fmt.Errorf("gemini api error: %s", gResp.Error.Message)
			}
			for _, cand := range gResp.Candidates {
				for _, part := range cand.Content.Parts {
					if part.InlineData != nil && part.InlineData.Data != "" {
						return base64.StdEncoding.DecodeString(part.InlineData.Data)
					}
				}
			}
			return nil, fmt.Errorf("gemini response contained no image parts")
		}

		var imgResp imagenResponse
		if err := json.Unmarshal(respBytes, &imgResp); err != nil {
			return nil, fmt.Errorf("failed to parse imagen response: %w", err)
		}
		if imgResp.Error != nil {
			return nil, fmt.Errorf("imagen api error: %s", imgResp.Error.Message)
		}
		if len(imgResp.Predictions) == 0 || imgResp.Predictions[0].BytesBase64Encoded == "" {
			return nil, fmt.Errorf("imagen response contained no image predictions")
		}

		return base64.StdEncoding.DecodeString(imgResp.Predictions[0].BytesBase64Encoded)
	}

	return nil, lastErr
}
