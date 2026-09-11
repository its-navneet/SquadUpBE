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
	"time"

	"squadup/backend/internal/config"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Storage interface {
	Upload(ctx context.Context, key string, data []byte, contentType string) (string, error)
	Delete(ctx context.Context, key string) error
	DeleteObject(ctx context.Context, bucket, key string) error
	DeleteByURL(ctx context.Context, imageURL string) error
	Presign(ctx context.Context, bucket, key string) (string, error)
	PresignURL(ctx context.Context, rawURL string) string
	BucketName() string
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
	if isManagedKey(rawURL) && !strings.Contains(rawURL, "..") && !strings.Contains(rawURL, "?") {
		return rawURL
	}

	u, err := url.Parse(rawURL)
	var path string
	if err != nil || u.Path == "" {
		path = strings.Split(rawURL, "?")[0]
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

// ExtractBucketAndKey extracts the S3 bucket and object key from an S3 URL, s3:// URI, or raw key.
func ExtractBucketAndKey(rawURL string, defaultBucket string) (string, string) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", ""
	}

	// Case 1: s3://bucket/key
	if strings.HasPrefix(rawURL, "s3://") {
		trimmed := strings.TrimPrefix(rawURL, "s3://")
		parts := strings.SplitN(trimmed, "/", 2)
		if len(parts) == 2 {
			cleanKey := filepath.Clean(parts[1])
			if !strings.Contains(cleanKey, "..") {
				return parts[0], cleanKey
			}
		}
		return parts[0], ""
	}

	u, err := url.Parse(rawURL)
	if err == nil && u.Host != "" {
		// Virtual-hosted: <bucket>.s3.<region>.amazonaws.com/<key>
		hostParts := strings.Split(u.Host, ".")
		if len(hostParts) >= 4 && (hostParts[1] == "s3" || strings.HasPrefix(hostParts[1], "s3-")) {
			bucket := hostParts[0]
			key := strings.TrimLeft(u.Path, "/")
			cleanKey := filepath.Clean(key)
			if !strings.Contains(cleanKey, "..") {
				return bucket, cleanKey
			}
		}
		// Path-style: s3.<region>.amazonaws.com/<bucket>/<key>
		if len(hostParts) >= 3 && (hostParts[0] == "s3" || strings.HasPrefix(hostParts[0], "s3-")) {
			pathParts := strings.SplitN(strings.TrimLeft(u.Path, "/"), "/", 2)
			if len(pathParts) == 2 {
				cleanKey := filepath.Clean(pathParts[1])
				if !strings.Contains(cleanKey, "..") {
					return pathParts[0], cleanKey
				}
			}
		}
	}

	// Fallback using ExtractKeyFromURL
	key := ExtractKeyFromURL(rawURL, defaultBucket)
	if key != "" {
		return defaultBucket, key
	}

	return defaultBucket, ""
}

// ============================================================
// Amazon S3 Storage
// ============================================================

type S3Storage struct {
	client        *s3.Client
	presignClient *s3.PresignClient
	bucketName    string
	region        string
	endpoint      string
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

	presignClient := s3.NewPresignClient(s3Client)

	return &S3Storage{
		client:        s3Client,
		presignClient: presignClient,
		bucketName:    cfg.AWSS3Bucket,
		region:        region,
		endpoint:      endpoint,
	}, nil
}

func (s *S3Storage) BucketName() string {
	return s.bucketName
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

	// Generate and return presigned URL valid for 7 days
	presigned, presignErr := s.Presign(ctx, s.bucketName, key)
	if presignErr == nil && presigned != "" {
		return presigned, nil
	}

	// Custom endpoint fallback
	if s.endpoint != "" {
		cleanEndpoint := strings.TrimRight(s.endpoint, "/")
		return fmt.Sprintf("%s/%s/%s", cleanEndpoint, s.bucketName, key), nil
	}

	// Standard Amazon S3 URL
	return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", s.bucketName, s.region, key), nil
}

func (s *S3Storage) Presign(ctx context.Context, bucket, key string) (string, error) {
	if bucket == "" {
		bucket = s.bucketName
	}
	key = strings.TrimLeft(key, "/")
	if key == "" {
		return "", nil
	}

	presignReq, err := s.presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = 7 * 24 * time.Hour // 7 days (maximum allowed for SigV4)
	})
	if err != nil {
		return "", err
	}
	return presignReq.URL, nil
}

func (s *S3Storage) PresignURL(ctx context.Context, rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || strings.HasPrefix(rawURL, "data:") {
		return rawURL
	}
	bucket, key := ExtractBucketAndKey(rawURL, s.bucketName)
	if key == "" {
		return rawURL
	}
	signed, err := s.Presign(ctx, bucket, key)
	if err != nil || signed == "" {
		return rawURL
	}
	return signed
}

func (s *S3Storage) Delete(ctx context.Context, key string) error {
	return s.DeleteObject(ctx, s.bucketName, key)
}

func (s *S3Storage) DeleteObject(ctx context.Context, bucket, key string) error {
	if bucket == "" {
		bucket = s.bucketName
	}
	key = strings.TrimLeft(key, "/")
	if key == "" {
		return nil
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	return err
}

func (s *S3Storage) DeleteByURL(ctx context.Context, imageURL string) error {
	bucket, key := ExtractBucketAndKey(imageURL, s.bucketName)
	if key == "" {
		return nil
	}
	return s.DeleteObject(ctx, bucket, key)
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

func (l *LocalStorage) BucketName() string {
	return ""
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

func (l *LocalStorage) Presign(_ context.Context, _, key string) (string, error) {
	key = strings.TrimLeft(key, "/")
	if l.baseURL != "" {
		return fmt.Sprintf("%s/%s/%s", l.baseURL, l.baseDir, key), nil
	}
	return fmt.Sprintf("/%s/%s", l.baseDir, key), nil
}

func (l *LocalStorage) PresignURL(_ context.Context, rawURL string) string {
	return rawURL
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

func (l *LocalStorage) DeleteObject(ctx context.Context, _, key string) error {
	return l.Delete(ctx, key)
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
