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
	IsConfigured() bool
}

type B2Storage struct {
	client     *s3.Client
	bucketName string
	endpoint   string
	publicURL  string
}

func NewB2Storage(cfg config.Config) (*B2Storage, error) {
	if cfg.B2KeyID == "" || cfg.B2ApplicationKey == "" || cfg.B2BucketName == "" {
		return nil, fmt.Errorf("missing Backblaze B2 credentials (B2_KEY_ID, B2_APPLICATION_KEY, B2_BUCKET_NAME)")
	}

	endpoint := strings.TrimSpace(cfg.B2Endpoint)
	if endpoint == "" {
		endpoint = "https://s3.us-east-005.backblazeb2.com"
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}

	region := strings.TrimSpace(cfg.B2Region)
	if region == "" {
		u, err := url.Parse(endpoint)
		if err == nil {
			parts := strings.Split(u.Host, ".")
			if len(parts) >= 3 && parts[0] == "s3" {
				region = parts[1]
			}
		}
	}
	if region == "" {
		region = "us-east-005"
	}

	customResolver := aws.EndpointResolverWithOptionsFunc(func(service, reg string, options ...interface{}) (aws.Endpoint, error) {
		return aws.Endpoint{
			URL:               endpoint,
			SigningRegion:     region,
			HostnameImmutable: true,
		}, nil
	})

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.B2KeyID, cfg.B2ApplicationKey, "")),
		awsconfig.WithEndpointResolverWithOptions(customResolver),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config for B2: %w", err)
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	return &B2Storage{
		client:     s3Client,
		bucketName: cfg.B2BucketName,
		endpoint:   endpoint,
		publicURL:  strings.TrimRight(strings.TrimSpace(cfg.B2PublicURL), "/"),
	}, nil
}

func (b *B2Storage) Upload(ctx context.Context, key string, data []byte, contentType string) (string, error) {
	key = strings.TrimLeft(key, "/")

	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(b.bucketName),
		Key:         aws.String(key),
		Body:        bytes.NewReader(data),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload to B2: %w", err)
	}

	if b.publicURL != "" {
		return fmt.Sprintf("%s/%s", b.publicURL, key), nil
	}

	// For private buckets without public URL, generate a presigned GET URL valid for 7 days
	presignClient := s3.NewPresignClient(b.client)
	presignReq, err := presignClient.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucketName),
		Key:    aws.String(key),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = 7 * 24 * time.Hour
	})
	if err == nil && presignReq != nil && presignReq.URL != "" {
		return presignReq.URL, nil
	}

	// Standard S3 URL for Backblaze B2 path-style: https://<endpoint>/<bucket>/<key>
	cleanEndpoint := strings.TrimRight(b.endpoint, "/")
	return fmt.Sprintf("%s/%s/%s", cleanEndpoint, b.bucketName, key), nil
}

func (b *B2Storage) Delete(ctx context.Context, key string) error {
	key = strings.TrimLeft(key, "/")
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucketName),
		Key:    aws.String(key),
	})
	return err
}

func (b *B2Storage) IsConfigured() bool {
	return true
}

// LocalStorage is a fallback for local development/testing when B2 is not configured.
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

func (l *LocalStorage) IsConfigured() bool {
	return false
}

// New creates a Storage provider (Backblaze B2 if configured, LocalStorage fallback otherwise).
func New(cfg config.Config) Storage {
	if cfg.B2KeyID != "" && cfg.B2ApplicationKey != "" && cfg.B2BucketName != "" {
		b2, err := NewB2Storage(cfg)
		if err == nil {
			log.Printf("[Storage] Initialized Backblaze B2 storage (bucket: %s, endpoint: %s)", cfg.B2BucketName, cfg.B2Endpoint)
			return b2
		}
		log.Printf("[Storage] Failed to initialize B2: %v, falling back to local storage", err)
	} else {
		log.Println("[Storage] Backblaze B2 credentials not set. Using local disk fallback (uploads/).")
	}
	return NewLocalStorage("uploads", "")
}
