package config

import (
	"os"
	"strings"
)

type Config struct {
	Port                    string
	DatabaseURL             string
	JWTSecret               string
	CORSOrigins             string
	ImageGenURL             string
	ImageGenKey             string
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
	var n int
	for _, ch := range val {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func parseFloat(val string, def float64) float64 {
	if val == "" {
		return def
	}
	var n float64
	var dec float64
	var inDec bool
	decDiv := 1.0
	for _, ch := range val {
		if ch == '.' {
			inDec = true
			continue
		}
		if ch < '0' || ch > '9' {
			return def
		}
		if inDec {
			decDiv *= 10
			dec = dec*10 + float64(ch-'0')
		} else {
			n = n*10 + float64(ch-'0')
		}
	}
	return n + (dec / decDiv)
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
		ImageGenURL:             os.Getenv("IMAGE_GENERATION_URL"),
		ImageGenKey:             os.Getenv("IMAGE_GENERATION_API_KEY"),
		RateLimitEnabled:        parseBool(os.Getenv("RATE_LIMIT_ENABLED"), true),
		RateLimitRPS:            parseFloat(os.Getenv("RATE_LIMIT_RPS"), 2.0),
		RateLimitBurst:          parseInt(os.Getenv("RATE_LIMIT_BURST"), 30),
		AuthRateLimitRPM:        parseInt(os.Getenv("AUTH_RATE_LIMIT_RPM"), 10),
		AuthRateLimitBurst:      parseInt(os.Getenv("AUTH_RATE_LIMIT_BURST"), 5),
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
	if c.CORSOrigins == "" {
		c.CORSOrigins = "*"
	}
	return c
}
