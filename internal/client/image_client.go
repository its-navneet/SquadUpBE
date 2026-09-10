package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type ImageClient struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

func NewImageClient(baseURL, apiKey string) *ImageClient {
	return &ImageClient{
		BaseURL: baseURL,
		APIKey:  apiKey,
		Client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

type imageRequest struct {
	Prompt string `json:"prompt"`
}

func (c *ImageClient) Generate(ctx context.Context, prompt string) ([]byte, error) {
	body, err := json.Marshal(imageRequest{
		Prompt: prompt,
	})
	if err != nil {
		return nil, err
	}

	httpClient := c.Client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 120 * time.Second}
	}

	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(
			ctx,
			http.MethodPost,
			c.BaseURL,
			bytes.NewReader(body),
		)
		if err != nil {
			return nil, err
		}

		if c.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.APIKey)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
				continue
			}
		}

		if resp.StatusCode != http.StatusOK {
			responseBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			lastErr = fmt.Errorf(
				"image generation failed: status=%d body=%s",
				resp.StatusCode,
				string(responseBody),
			)
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

		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		return data, readErr
	}

	return nil, lastErr
}
