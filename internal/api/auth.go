package api

import (
	"strings"

	"squadup/backend/internal/models"
	"squadup/backend/internal/storage"

	"github.com/gin-gonic/gin"
)

func normalizePosition(pos string) string {
	clean := strings.ToUpper(strings.TrimSpace(pos))
	switch {
	case clean == "GK" || strings.Contains(clean, "GOAL") || clean == "KEEPER":
		return "GK"
	case clean == "DEF" || strings.Contains(clean, "DEF") || clean == "CB" || clean == "FB" || clean == "LB" || clean == "RB":
		return "DEF"
	case clean == "MID" || clean == "MF" || strings.Contains(clean, "MID") || clean == "CM" || clean == "CAM" || clean == "CDM":
		return "MID"
	case clean == "ST" || clean == "FWD" || clean == "WG" || strings.Contains(clean, "STRIK") || strings.Contains(clean, "FOR") || strings.Contains(clean, "ATT") || strings.Contains(clean, "WING"):
		return "ST"
	default:
		return clean
	}
}

func (s *Server) Register(c *gin.Context) {
	var in struct {
		Name               string  `json:"name"`
		Email              string  `json:"email"`
		Password           string  `json:"password"`
		DateOfBirth        string  `json:"date_of_birth"`
		Age                int     `json:"age"`
		HeightCM           float64 `json:"height_cm"`
		WeightKG           float64 `json:"weight_kg"`
		PreferredFoot      string  `json:"preferred_foot"`
		Bio                string  `json:"bio"`
		ProfilePhotoURL    string  `json:"profile_photo_url"`
		ProfilePhotoBucket string  `json:"profile_photo_bucket"`
		ProfilePhotoKey    string  `json:"profile_photo_key"`
		Position           string  `json:"position"`
		KitNumber          int     `json:"kit_number"`
	}
	if c.BindJSON(&in) != nil || in.Name == "" || len(in.Password) < 8 {
		c.JSON(400, err("name/email/password(8+) required"))
		return
	}
	hash, e := s.authService.Hash(in.Password)
	if e != nil {
		c.JSON(500, err("password hashing failed"))
		return
	}

	cleanDOB := strings.TrimSpace(in.DateOfBirth)
	computedAge := in.Age
	if cleanDOB != "" {
		calcAge, errVal := models.CalculateAgeFromDOB(cleanDOB)
		if errVal != nil {
			c.JSON(400, err("invalid date of birth format, expected YYYY-MM-DD"))
			return
		}
		if calcAge < 5 || calcAge > 120 {
			c.JSON(400, err("age calculated from date of birth must be between 5 and 120"))
			return
		}
		computedAge = calcAge
	} else if computedAge < 5 || computedAge > 120 {
		c.JSON(400, err("date of birth is required"))
		return
	}

	if in.KitNumber < 0 || in.KitNumber > 99 {
		c.JSON(400, err("jersey number must be between 1 and 99"))
		return
	}

	pos := normalizePosition(in.Position)
	if pos == "" {
		pos = "MID"
	}

	photoURL := cleanStoredURL(in.ProfilePhotoURL)
	photoBucket := strings.TrimSpace(in.ProfilePhotoBucket)
	photoKey := strings.TrimSpace(in.ProfilePhotoKey)
	if photoKey == "" && photoURL != "" {
		photoBucket, photoKey = storage.ExtractBucketAndKey(photoURL, s.storage.BucketName())
	}
	if photoBucket == "" && photoKey != "" {
		photoBucket = s.storage.BucketName()
	}

	u := models.User{
		Name:               in.Name,
		Email:              strings.ToLower(strings.TrimSpace(in.Email)),
		PasswordHash:       hash,
		DateOfBirth:        cleanDOB,
		Age:                computedAge,
		HeightCM:           in.HeightCM,
		WeightKG:           in.WeightKG,
		PreferredFoot:      in.PreferredFoot,
		Bio:                in.Bio,
		ProfilePhotoURL:    photoURL,
		ProfilePhotoBucket: photoBucket,
		ProfilePhotoKey:    photoKey,
		Position:           pos,
		KitNumber:          in.KitNumber,
	}
	if e = s.db.Create(&u).Error; e != nil {
		c.JSON(409, err("email already registered"))
		return
	}
	tok, _ := s.authService.Token(u.ID.String())
	c.JSON(201, gin.H{"success": true, "data": gin.H{"token": tok, "user": u}})
}

func (s *Server) Login(c *gin.Context) {
	var in struct{ Email, Password string }
	if c.BindJSON(&in) != nil {
		c.JSON(400, err("invalid request"))
		return
	}
	var u models.User
	if e := s.db.Where("email=?", strings.ToLower(strings.TrimSpace(in.Email))).First(&u).Error; e != nil || !s.authService.Check(u.PasswordHash, in.Password) {
		c.JSON(401, err("invalid credentials"))
		return
	}
	tok, _ := s.authService.Token(u.ID.String())
	c.JSON(200, gin.H{"success": true, "data": gin.H{"token": tok, "user": u}})
}

