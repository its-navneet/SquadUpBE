package storage

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"squadup/backend/internal/config"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Storage interface {
	Upload(ctx context.Context, key string, data []byte, contentType string) (string, error)
	Delete(ctx context.Context, key string) error
	DeleteByURL(ctx context.Context, imageURL string) error
	IsConfigured() bool
}

var managedKeyPrefixes = []string{"profiles/", "posters/", "groups/", "logos/"}

func isManagedKey(key string) bool {
	for _, p := range managedKeyPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// ExtractKeyFromURL safely extracts the storage object key from an absolute or relative image URL.
// It handles virtual-hosted S3 URLs, path-style S3 URLs, local URLs, and raw paths.
// Returns empty string if the URL is not managed or if it contains path traversal.
func ExtractKeyFromURL(rawURL string, bucketName string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return ""
	}

	// Direct managed key (e.g. "profiles/abc.jpg")
	if isManagedKey(rawURL) && !strings.Contains(rawURL, "..") {
		return rawURL
	}

	u, err := url.Parse(rawURL)
	var path string
	if err != nil || u.Path == "" {
		path = rawURL
	} else {
		path = u.Path
	}

	path = strings.TrimLeft(path, "/")

	// Strip bucket prefix if path-style URL: e.g. "my-bucket/profiles/abc.jpg"
	if bucketName != "" && strings.HasPrefix(path, bucketName+"/") {
		path = strings.TrimPrefix(path, bucketName+"/")
	}

	// Strip "uploads/" prefix if local storage URL: e.g. "uploads/profiles/abc.jpg"
	if strings.HasPrefix(path, "uploads/") {
		path = strings.TrimPrefix(path, "uploads/")
	}

	// Prevent path traversal
	cleanPath := filepath.Clean(path)
	if strings.Contains(cleanPath, "..") {
		return ""
	}

	if isManagedKey(cleanPath) {
		return cleanPath
	}

	return ""
}

// ============================================================
// Amazon S3 Storage
// ============================================================

type S3Storage struct {
	client     *s3.Client
	bucketName string
	region     string
	endpoint   string
}

func NewS3Storage(cfg config.Config) (*S3Storage, error) {
	if cfg.AWSAccessKeyID == "" || cfg.AWSSecretAccessKey == "" || cfg.AWSS3Bucket == "" {
		return nil, fmt.Errorf("missing AWS S3 credentials (AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_S3_BUCKET)")
	}

	region := strings.TrimSpace(cfg.AWSRegion)
	if region == "" {
		region = "ap-south-1"
	}

	var optFns []func(*awsconfig.LoadOptions) error
	optFns = append(optFns, awsconfig.WithRegion(region))
	optFns = append(optFns, awsconfig.WithCredentialsProvider(
		credentials.NewStaticCredentialsProvider(cfg.AWSAccessKeyID, cfg.AWSSecretAccessKey, ""),
	))

	endpoint := strings.TrimSpace(cfg.AWSS3Endpoint)
	if endpoint != "" {
		if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
			endpoint = "https://" + endpoint
		}
		customResolver := aws.EndpointResolverWithOptionsFunc(func(service, reg string, options ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{
				URL:               endpoint,
				SigningRegion:     region,
				HostnameImmutable: true,
			}, nil
		})
		optFns = append(optFns, awsconfig.WithEndpointResolverWithOptions(customResolver))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), optFns...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.AWSS3ForcePathStyle || endpoint != "" {
			o.UsePathStyle = true
		}
	})

	return &S3Storage{
		client:     s3Client,
		bucketName: cfg.AWSS3Bucket,
		region:     region,
		endpoint:   endpoint,
	}, nil
}

func (s *S3Storage) Upload(ctx context.Context, key string, data []byte, contentType string) (string, error) {
	key = strings.TrimLeft(key, "/")

	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(s.bucketName),
		Key:          aws.String(key),
		Body:         bytes.NewReader(data),
		ContentType:  aws.String(contentType),
		CacheControl: aws.String("public, max-age=31536000, immutable"),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload to Amazon S3: %w", err)
	}

	// Custom endpoint (e.g. LocalStack or MinIO)
	if s.endpoint != "" {
		cleanEndpoint := strings.TrimRight(s.endpoint, "/")
		return fmt.Sprintf("%s/%s/%s", cleanEndpoint, s.bucketName, key), nil
	}

	// Standard Amazon S3 URL
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", s.bucketName, s.region, key), nil
}

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	key = strings.TrimLeft(key, "/")
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucketName),
		Key:    aws.String(key),
	})
	return err
}

func (s *S3Storage) DeleteByURL(ctx context.Context, imageURL string) error {
	key := ExtractKeyFromURL(imageURL, s.bucketName)
	if key == "" {
		return nil
	}
	return s.Delete(ctx, key)
}

func (s *S3Storage) IsConfigured() bool {
	return true
}

// ============================================================
// LocalStorage (Fallback when no cloud storage is configured)
// ============================================================

type LocalStorage struct {
	baseDir string
	baseURL string
}

func NewLocalStorage(baseDir, baseURL string) *LocalStorage {
	if baseDir == "" {
		baseDir = "uploads"
	}
	_ = os.MkdirAll(baseDir, 0755)
	return &LocalStorage{
		baseDir: baseDir,
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

func (l *LocalStorage) Upload(_ context.Context, key string, data []byte, _ string) (string, error) {
	key = strings.TrimLeft(key, "/")
	cleanBase := filepath.Clean(l.baseDir)
	targetPath := filepath.Clean(filepath.Join(cleanBase, key))
	if !strings.HasPrefix(targetPath, cleanBase+string(filepath.Separator)) && targetPath != cleanBase {
		return "", fmt.Errorf("invalid path traversal in key: %s", key)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(targetPath, data, 0644); err != nil {
		return "", err
	}

	if l.baseURL != "" {
		return fmt.Sprintf("%s/%s/%s", l.baseURL, l.baseDir, key), nil
	}
	return fmt.Sprintf("/%s/%s", l.baseDir, key), nil
}

func (l *LocalStorage) Delete(_ context.Context, key string) error {
	key = strings.TrimLeft(key, "/")
	cleanBase := filepath.Clean(l.baseDir)
	targetPath := filepath.Clean(filepath.Join(cleanBase, key))
	if !strings.HasPrefix(targetPath, cleanBase+string(filepath.Separator)) && targetPath != cleanBase {
		return fmt.Errorf("invalid path traversal in key: %s", key)
	}
	return os.Remove(targetPath)
}

func (l *LocalStorage) DeleteByURL(ctx context.Context, imageURL string) error {
	key := ExtractKeyFromURL(imageURL, "")
	if key == "" && l.baseDir != "" {
		u, err := url.Parse(strings.TrimSpace(imageURL))
		var path string
		if err != nil || u.Path == "" {
			path = strings.TrimSpace(imageURL)
		} else {
			path = u.Path
		}
		path = strings.TrimLeft(path, "/")
		cleanBase := strings.TrimLeft(filepath.Clean(l.baseDir), "/")
		if strings.HasPrefix(path, cleanBase+"/") {
			path = strings.TrimPrefix(path, cleanBase+"/")
		}
		if isManagedKey(path) && !strings.Contains(filepath.Clean(path), "..") {
			key = path
		}
	}
	if key == "" {
		return nil
	}
	return l.Delete(ctx, key)
}

func (l *LocalStorage) IsConfigured() bool {
	return false
}

// New creates a Storage provider (Amazon S3 if configured, or LocalStorage fallback).
func New(cfg config.Config) Storage {
	// 1. Amazon S3
	if cfg.AWSAccessKeyID != "" && cfg.AWSSecretAccessKey != "" && cfg.AWSS3Bucket != "" {
		s3Store, err := NewS3Storage(cfg)
		if err == nil {
			log.Printf("[Storage] Initialized Amazon S3 storage (bucket: %s, region: %s)", cfg.AWSS3Bucket, cfg.AWSRegion)
			return s3Store
		}
		log.Printf("[Storage] Failed to initialize Amazon S3: %v, falling back to local storage", err)
	} else {
		log.Println("[Storage] Amazon S3 credentials not set. Using local disk fallback (uploads/).")
	}

	// 2. Local disk (Offline/Testing fallback)
	return NewLocalStorage("uploads", "")
}
