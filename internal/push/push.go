package push

import (
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"squadup/backend/internal/models"
)

type serviceAccountJSON struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
}

type Service struct {
	db          *gorm.DB
	projectID   string
	clientEmail string
	privateKey  *rsa.PrivateKey
	mu          sync.RWMutex
	cachedToken string
	tokenExpiry time.Time
	httpClient  *http.Client
}

func New(db *gorm.DB, projectID, credsPath, credsJSON string) *Service {
	s := &Service{
		db:         db,
		projectID:  projectID,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}

	var raw []byte
	if strings.TrimSpace(credsJSON) != "" {
		raw = []byte(credsJSON)
	} else if strings.TrimSpace(credsPath) != "" {
		data, err := os.ReadFile(credsPath)
		if err == nil {
			raw = data
		} else {
			log.Printf("[Push] Failed to read Firebase credentials file at %s: %v", credsPath, err)
		}
	}

	if len(raw) > 0 {
		var sa serviceAccountJSON
		if err := json.Unmarshal(raw, &sa); err == nil && sa.PrivateKey != "" && sa.ClientEmail != "" {
			pk, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(sa.PrivateKey))
			if err == nil {
				s.clientEmail = sa.ClientEmail
				s.privateKey = pk
				if sa.ProjectID != "" {
					s.projectID = sa.ProjectID
				}
				log.Printf("[Push] Initialized Firebase FCM HTTP v1 service for project: %s (client: %s)", s.projectID, s.clientEmail)
				return s
			} else {
				log.Printf("[Push] Failed to parse private key: %v", err)
			}
		} else {
			log.Printf("[Push] Failed to unmarshal service account JSON: %v", err)
		}
	}

	log.Printf("[Push] Running in dry-run/mock mode (no valid service account provided). Device tokens will be stored in database.")
	return s
}

func (s *Service) getAccessToken() (string, error) {
	s.mu.RLock()
	if s.cachedToken != "" && time.Now().Before(s.tokenExpiry.Add(-1*time.Minute)) {
		token := s.cachedToken
		s.mu.RUnlock()
		return token, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	// Double-check under write lock
	if s.cachedToken != "" && time.Now().Before(s.tokenExpiry.Add(-1*time.Minute)) {
		return s.cachedToken, nil
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   s.clientEmail,
		"sub":   s.clientEmail,
		"aud":   "https://oauth2.googleapis.com/token",
		"scope": "https://www.googleapis.com/auth/firebase.messaging",
		"iat":   now.Unix(),
		"exp":   now.Add(1 * time.Hour).Unix(),
	}

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signedAssertion, err := token.SignedString(s.privateKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign jwt: %w", err)
	}

	formData := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {signedAssertion},
	}

	resp, err := s.httpClient.PostForm("https://oauth2.googleapis.com/token", formData)
	if err != nil {
		return "", fmt.Errorf("oauth2 token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read oauth2 response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth2 token failed with code %d: %s", resp.StatusCode, string(body))
	}

	var tokenRes struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenRes); err != nil {
		return "", fmt.Errorf("failed to parse oauth2 response: %w", err)
	}

	s.cachedToken = tokenRes.AccessToken
	s.tokenExpiry = time.Now().Add(time.Duration(tokenRes.ExpiresIn) * time.Second)
	return s.cachedToken, nil
}

func (s *Service) RegisterToken(userID uuid.UUID, token, platform string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("token cannot be empty")
	}
	if s.db == nil {
		return nil
	}

	devToken := models.DeviceToken{
		UserID:   userID,
		Token:    token,
		Platform: platform,
	}

	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "token"}},
		DoUpdates: clause.AssignmentColumns([]string{"user_id", "platform", "updated_at"}),
	}).Create(&devToken).Error
}

func (s *Service) DeleteToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" || s.db == nil {
		return nil
	}
	return s.db.Where("token = ?", token).Delete(&models.DeviceToken{}).Error
}

func (s *Service) SendPush(userIDs []uuid.UUID, title, body string, data map[string]string) {
	if len(userIDs) == 0 || s.db == nil {
		return
	}

	// Run in background goroutine to never block the main request
	go func() {
		var tokens []models.DeviceToken
		if err := s.db.Where("user_id IN (?)", userIDs).Find(&tokens).Error; err != nil || len(tokens) == 0 {
			return
		}

		if s.privateKey == nil {
			log.Printf("[Push Mock] Would send push to %d device(s) for %d user(s) (Title: %q)", len(tokens), len(userIDs), title)
			return
		}

		accessToken, err := s.getAccessToken()
		if err != nil {
			log.Printf("[Push] Failed to acquire access token: %v", err)
			return
		}

		fcmURL := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", s.projectID)

		for _, t := range tokens {
			reqBody := map[string]any{
				"message": map[string]any{
					"token": t.Token,
					"notification": map[string]string{
						"title": title,
						"body":  body,
					},
					"data": data,
					"android": map[string]any{
						"priority": "HIGH",
						"notification": map[string]string{
							"channel_id": "squadup_alerts",
						},
					},
				},
			}

			payloadBytes, _ := json.Marshal(reqBody)
			req, err := http.NewRequest(http.MethodPost, fcmURL, bytes.NewReader(payloadBytes))
			if err != nil {
				continue
			}

			req.Header.Set("Authorization", "Bearer "+accessToken)
			req.Header.Set("Content-Type", "application/json")

			resp, err := s.httpClient.Do(req)
			if err != nil {
				log.Printf("[Push] Failed to send message to token %s: %v", t.Token, err)
				continue
			}

			respBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				continue
			}

			// Clean up stale or unregistered tokens
			respStr := string(respBytes)
			if resp.StatusCode == http.StatusNotFound || strings.Contains(respStr, "UNREGISTERED") || strings.Contains(respStr, "NOT_FOUND") {
				log.Printf("[Push] Deleting stale token for user %s", t.UserID)
				s.db.Where("token = ?", t.Token).Delete(&models.DeviceToken{})
			} else {
				log.Printf("[Push] FCM send error (status %d): %s", resp.StatusCode, respStr)
			}
		}
	}()
}
