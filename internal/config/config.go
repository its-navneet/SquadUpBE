package config

import (
	"os"
	"strings"
)

type Config struct {
	Port               string
	DatabaseURL        string
	JWTSecret          string
	CORSOrigins        string
	ImageGenURL        string
	ImageGenKey        string
	RateLimitEnabled   bool
	RateLimitRPS       float64
	RateLimitBurst     int
	AuthRateLimitRPM   int
	AuthRateLimitBurst int
	B2Endpoint         string
	B2Region           string
	B2KeyID            string
	B2ApplicationKey   string
	B2BucketName       string
	B2PublicURL        string
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

func Load() Config {
	loadEnvFile(".env")
	loadEnvFile("SquadUpBE/.env")
	loadEnvFile("../SquadUpBE/.env")
	loadEnvFile("backend/.env")
	loadEnvFile("../backend/.env")
	loadEnvFile("../.env")

	c := Config{
		Port:               os.Getenv("PORT"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		JWTSecret:          os.Getenv("JWT_SECRET"),
		CORSOrigins:        os.Getenv("CORS_ORIGINS"),
		ImageGenURL:        os.Getenv("IMAGE_GENERATION_URL"),
		ImageGenKey:        os.Getenv("IMAGE_GENERATION_API_KEY"),
		RateLimitEnabled:   parseBool(os.Getenv("RATE_LIMIT_ENABLED"), true),
		RateLimitRPS:       parseFloat(os.Getenv("RATE_LIMIT_RPS"), 2.0),
		RateLimitBurst:     parseInt(os.Getenv("RATE_LIMIT_BURST"), 30),
		AuthRateLimitRPM:   parseInt(os.Getenv("AUTH_RATE_LIMIT_RPM"), 10),
		AuthRateLimitBurst: parseInt(os.Getenv("AUTH_RATE_LIMIT_BURST"), 5),
		B2Endpoint:         os.Getenv("B2_ENDPOINT"),
		B2Region:           os.Getenv("B2_REGION"),
		B2KeyID:            os.Getenv("B2_KEY_ID"),
		B2ApplicationKey:   os.Getenv("B2_APPLICATION_KEY"),
		B2BucketName:       os.Getenv("B2_BUCKET_NAME"),
		B2PublicURL:        os.Getenv("B2_PUBLIC_URL"),
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
