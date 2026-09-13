package api

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (s *Server) UploadImage(c *gin.Context) {
	file, header, fileErr := c.Request.FormFile("image")
	if fileErr != nil {
		c.JSON(400, gin.H{"error": "image file required in 'image' field"})
		return
	}
	defer file.Close()

	const maxImageBytes = 2 * 1024 * 1024 // 2MB
	if header.Size > maxImageBytes {
		c.JSON(400, gin.H{"error": "image size must be less than 2MB"})
		return
	}

	data, readErr := io.ReadAll(io.LimitReader(file, maxImageBytes+1))
	if readErr != nil {
		c.JSON(500, gin.H{"error": "failed to read uploaded file"})
		return
	}
	if int64(len(data)) > maxImageBytes {
		c.JSON(400, gin.H{"error": "image size must be less than 2MB"})
		return
	}

	contentType := http.DetectContentType(data)
	validTypes := map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/webp": ".webp",
		"image/gif":  ".gif",
	}
	ext, ok := validTypes[contentType]
	if !ok {
		c.JSON(400, gin.H{"error": "only genuine JPEG, PNG, WebP or GIF images are supported"})
		return
	}

	if contentType == "image/webp" {
		if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
			c.JSON(400, gin.H{"error": "corrupted or invalid WebP image data"})
			return
		}
	} else {
		if _, _, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
			c.JSON(400, gin.H{"error": "corrupted or invalid image data"})
			return
		}
	}

	key := fmt.Sprintf("profiles/%s%s", uuid.New().String(), ext)
	imageURL, uploadErr := s.storage.Upload(c.Request.Context(), key, data, contentType)
	if uploadErr != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to upload image: %v", uploadErr)})
		return
	}

	if strings.HasPrefix(imageURL, "/") {
		scheme := "http"
		if c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https" {
			scheme = "https"
		}
		imageURL = fmt.Sprintf("%s://%s%s", scheme, c.Request.Host, imageURL)
	}

	c.JSON(200, gin.H{
		"success": true,
		"data": gin.H{
			"url":    imageURL,
			"key":    key,
			"bucket": s.storage.BucketName(),
		},
	})
}

