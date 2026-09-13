package push

import (
	"os"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"squadup/backend/internal/models"
)

func setupTestDB(t *testing.T) *gorm.DB {
	// Attempt local test DB, or skip if not available
	dsn := "postgres://squadup:squadup@localhost:5432/squadup?sslmode=disable"
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Skip("PostgreSQL database not available for integration tests, skipping DB tests")
		return nil
	}
	_ = db.AutoMigrate(&models.DeviceToken{})
	return db
}

func TestPushServiceMockMode(t *testing.T) {
	svc := New(nil, "squadup-32af8", "", "")
	if svc == nil {
		t.Fatal("expected push service instance, got nil")
	}

	if svc.privateKey != nil {
		t.Fatal("expected mock mode without private key")
	}

	// SendPush in mock mode with empty users should return immediately
	svc.SendPush(nil, "Title", "Body", nil)

	// SendPush in mock mode with users should not panic
	svc.SendPush([]uuid.UUID{uuid.New()}, "Title", "Body", map[string]string{"type": "TEST"})
}

func TestDeviceTokenRegistration(t *testing.T) {
	db := setupTestDB(t)
	if db == nil {
		return
	}

	svc := New(db, "squadup-32af8", "", "")
	userID := uuid.New()
	testToken := "fcm-test-token-" + uuid.NewString()

	// 1. Register Token
	if err := svc.RegisterToken(userID, testToken, "android"); err != nil {
		t.Fatalf("failed to register token: %v", err)
	}

	var dt models.DeviceToken
	if err := db.Where("token = ?", testToken).First(&dt).Error; err != nil {
		t.Fatalf("token not found in database: %v", err)
	}
	if dt.UserID != userID || dt.Platform != "android" {
		t.Errorf("unexpected token data: %+v", dt)
	}

	// 2. Register same token with different user or platform (Upsert)
	newUserID := uuid.New()
	if err := svc.RegisterToken(newUserID, testToken, "ios"); err != nil {
		t.Fatalf("failed to upsert token: %v", err)
	}

	if err := db.Where("token = ?", testToken).First(&dt).Error; err != nil {
		t.Fatalf("token not found after upsert: %v", err)
	}
	if dt.UserID != newUserID || dt.Platform != "ios" {
		t.Errorf("token was not updated properly on conflict: %+v", dt)
	}

	// 3. Delete Token
	if err := svc.DeleteToken(testToken); err != nil {
		t.Fatalf("failed to delete token: %v", err)
	}

	var count int64
	db.Model(&models.DeviceToken{}).Where("token = ?", testToken).Count(&count)
	if count != 0 {
		t.Errorf("expected token count to be 0 after delete, got %d", count)
	}
}

func TestLoadLiveServiceAccount(t *testing.T) {
	keyPath := "../../squadup-32af8-firebase-adminsdk-fbsvc-a106e59d41.json"
	if _, err := os.Stat(keyPath); os.IsNotExist(err) {
		t.Skip("Live service account file not found, skipping live credential test")
	}
	svc := New(nil, "squadup-32af8", keyPath, "")
	if svc.privateKey == nil {
		t.Fatalf("expected private key to be parsed from %s, got nil", keyPath)
	}
	if svc.clientEmail == "" {
		t.Fatalf("expected client email to be parsed, got empty string")
	}
	t.Logf("Successfully parsed service account for client email: %s, project: %s", svc.clientEmail, svc.projectID)

	// Test acquiring OAuth2 access token from Google
	token, err := svc.getAccessToken()
	if err != nil {
		t.Logf("Notice: Google OAuth2 token request returned: %v (may require network access)", err)
	} else if token != "" {
		t.Logf("Successfully fetched Google OAuth2 Bearer token from Google!")
	}
}

func TestLoadServiceAccountFromRawJSON(t *testing.T) {
	keyPath := "../../squadup-32af8-firebase-adminsdk-fbsvc-a106e59d41.json"
	data, err := os.ReadFile(keyPath)
	if err != nil {
		t.Skip("Live service account file not found, skipping raw JSON test")
	}

	rawJSON := string(data)
	svc := New(nil, "squadup-32af8", "", rawJSON)
	if svc.privateKey == nil {
		t.Fatalf("expected private key to be parsed from raw JSON, got nil")
	}
	if svc.clientEmail != "firebase-adminsdk-fbsvc@squadup-32af8.iam.gserviceaccount.com" {
		t.Fatalf("unexpected client email: %s", svc.clientEmail)
	}
	t.Logf("Successfully initialized from raw JSON string!")
}
