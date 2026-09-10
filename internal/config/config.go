package config

import (
	"os"
	"strings"
)

type Config struct {
	Port        string
	DatabaseURL string
	JWTSecret   string
	CORSOrigins string
	ImageGenURL string
	ImageGenKey string
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
		Port:        os.Getenv("PORT"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		JWTSecret:   os.Getenv("JWT_SECRET"),
		CORSOrigins: os.Getenv("CORS_ORIGINS"),
		ImageGenURL: os.Getenv("IMAGE_GENERATION_URL"),
		ImageGenKey: os.Getenv("IMAGE_GENERATION_API_KEY"),
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
