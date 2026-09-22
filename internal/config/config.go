package config

import (
	"log"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Port                    string
	DatabaseURL             string
	JWTSecret               string
	CORSOrigins             string
	GeminiAPIKey            string
	GeminiImageModel        string
	RateLimitEnabled        bool
	RateLimitRPS            float64
	RateLimitBurst          int
	AuthRateLimitRPM        int
	AuthRateLimitBurst      int
	AWSRegion               string
	AWSAccessKeyID          string
	AWSSecretAccessKey      string
	AWSS3Bucket             string
	AWSS3Endpoint           string
	AWSS3ForcePathStyle     bool
	FirebaseProjectID       string
	FirebaseCredentialsPath string
	FirebaseCredentialsJSON string
	RedisURL                string
	RedisAddr               string
	RedisPassword           string
	RedisDB                 int
	RedisEnabled            bool
}

func parseBool(val string, def bool) bool {
	if val == "" {
		return def
	}
	val = strings.ToLower(val)
	return val == "true" || val == "1" || val == "yes"
}

func parseInt(val string, def int) int {
	if val == "" {
		return def
	}
	if n, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
		return n
	}
	return def
}

func parseFloat(val string, def float64) float64 {
	if val == "" {
		return def
	}
	if n, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
		return n
	}
	return def
}

func loadEnvFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			k := strings.TrimSpace(parts[0])
			v := strings.TrimSpace(parts[1])
			v = strings.Trim(v, `"'`)
			if os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
	}
}

func getEnvFirst(keys ...string) string {
	for _, k := range keys {
		if val := strings.TrimSpace(os.Getenv(k)); val != "" {
			return val
		}
	}
	return ""
}

func Load() Config {
	loadEnvFile(".env")
	loadEnvFile("SquadUpBE/.env")
	loadEnvFile("../SquadUpBE/.env")
	loadEnvFile("backend/.env")
	loadEnvFile("../backend/.env")
	loadEnvFile("../.env")

	c := Config{
		Port:                    os.Getenv("PORT"),
		DatabaseURL:             os.Getenv("DATABASE_URL"),
		JWTSecret:               os.Getenv("JWT_SECRET"),
		CORSOrigins:             os.Getenv("CORS_ORIGINS"),
		GeminiAPIKey:            getEnvFirst("GEMINI_API_KEY", "FIREBASE_AI_API_KEY", "IMAGE_GENERATION_API_KEY"),
		GeminiImageModel:        getEnvFirst("GEMINI_IMAGE_MODEL", "imagen-3.0-generate-002"),
		RateLimitEnabled:        parseBool(os.Getenv("RATE_LIMIT_ENABLED"), true),
		RateLimitRPS:            parseFloat(os.Getenv("RATE_LIMIT_RPS"), 20.0),
		RateLimitBurst:          parseInt(os.Getenv("RATE_LIMIT_BURST"), 100),
		AuthRateLimitRPM:        parseInt(os.Getenv("AUTH_RATE_LIMIT_RPM"), 60),
		AuthRateLimitBurst:      parseInt(os.Getenv("AUTH_RATE_LIMIT_BURST"), 20),
		AWSRegion:               getEnvFirst("AWS_REGION", "AWS_DEFAULT_REGION", "S3_REGION"),
		AWSAccessKeyID:          getEnvFirst("AWS_ACCESS_KEY_ID", "S3_ACCESS_KEY_ID"),
		AWSSecretAccessKey:      getEnvFirst("AWS_SECRET_ACCESS_KEY", "S3_SECRET_ACCESS_KEY"),
		AWSS3Bucket:             getEnvFirst("AWS_S3_BUCKET", "AWS_BUCKET_NAME", "S3_BUCKET_NAME"),
		AWSS3Endpoint:           getEnvFirst("AWS_S3_ENDPOINT", "S3_ENDPOINT"),
		AWSS3ForcePathStyle:     parseBool(getEnvFirst("AWS_S3_FORCE_PATH_STYLE", "S3_FORCE_PATH_STYLE"), false),
		FirebaseProjectID:       getEnvFirst("FIREBASE_PROJECT_ID", "GOOGLE_CLOUD_PROJECT"),
		FirebaseCredentialsPath: getEnvFirst("FIREBASE_CREDENTIALS_PATH", "FIREBASE_SERVICE_ACCOUNT_KEY_PATH", "GOOGLE_APPLICATION_CREDENTIALS"),
		FirebaseCredentialsJSON: getEnvFirst("FIREBASE_CREDENTIALS_JSON", "FIREBASE_SERVICE_ACCOUNT_JSON"),
		RedisURL:                getEnvFirst("REDIS_URL"),
		RedisAddr:               getEnvFirst("REDIS_ADDR", "REDIS_HOST"),
		RedisPassword:           os.Getenv("REDIS_PASSWORD"),
		RedisDB:                 parseInt(os.Getenv("REDIS_DB"), 0),
		RedisEnabled:            parseBool(os.Getenv("REDIS_ENABLED"), true),
	}
	if c.RedisURL == "" && c.RedisAddr == "" {
		// If neither REDIS_URL nor REDIS_ADDR is configured, Redis is disabled by default
		c.RedisEnabled = false
	}
	if c.FirebaseProjectID == "" {
		c.FirebaseProjectID = "squadup-32af8"
	}
	if c.AWSRegion == "" {
		c.AWSRegion = "ap-south-1"
	}
	if c.Port == "" {
		c.Port = "8080"
	}
	if c.DatabaseURL == "" {
		c.DatabaseURL = "postgres://squadup:squadup@localhost:5432/squadup?sslmode=disable"
	}
	if c.JWTSecret == "" {
		c.JWTSecret = "SQUADUP"
	}
	if (os.Getenv("ENV") == "production" || os.Getenv("GIN_MODE") == "release") && (c.JWTSecret == "SQUADUP" || len(c.JWTSecret) < 16) {
		log.Println("[WARNING] JWT_SECRET is set to default or is too short for a production environment! Please configure a strong, random 32+ char secret.")
	}
	if c.CORSOrigins == "" {
		c.CORSOrigins = "*"
	}
	return c
}
