package api

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"
	"squadup/backend/internal/storage"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func (s *Server) computeCareerStats(u *models.User) *models.CareerStats {
	var stats []models.PlayerStatistics
	s.db.Where("user_id = ?", u.ID).Find(&stats)

	cs := &models.CareerStats{}

	for _, st := range stats {
		cs.Matches += st.Matches
		cs.Wins += st.Wins
		cs.Draws += st.Draws
		cs.Losses += st.Losses
		cs.Goals += st.Goals
		cs.Assists += st.Assists
		cs.MVP += st.MVP
		cs.CleanSheets += st.CleanSheets
		cs.Saves += st.Saves
		cs.Fouls += st.Fouls
	}

	if cs.Matches > 0 {
		cs.WinRate = math.Round((float64(cs.Wins)/float64(cs.Matches))*1000) / 10
	}
	return cs
}

func (s *Server) populateAthleteRatings(u *models.User) {
	var count int64
	var avg float64
	s.db.Model(&models.PlayerRating{}).Where("rated_user_id = ?", u.ID).Count(&count)
	if count > 0 {
		s.db.Model(&models.PlayerRating{}).Where("rated_user_id = ?", u.ID).Select("COALESCE(AVG(overall),0)").Scan(&avg)
		u.OverallRating = math.Round(avg*10) / 10
		u.RatingsCount = int(count)

		type attrAvg struct {
			Attribute string  `gorm:"column:attribute"`
			AvgValue  float64 `gorm:"column:avg_value"`
		}
		var res []attrAvg
		err := s.db.Table("player_rating_attributes").
			Joins("JOIN player_ratings ON player_ratings.id = player_rating_attributes.player_rating_id").
			Where("player_ratings.rated_user_id = ?", u.ID).
			Select("player_rating_attributes.attribute, AVG(player_rating_attributes.value) as avg_value").
			Group("player_rating_attributes.attribute").
			Scan(&res).Error
		if err == nil && len(res) > 0 {
			attrs := make(map[string]float64)
			for _, r := range res {
				attrs[r.Attribute] = math.Round(r.AvgValue*10) / 10
			}
			u.SkillAttributes = attrs
		}
	}
}

func (s *Server) GetMe(c *gin.Context) {
	uid := mustUUID(auth.UserID(c))
	var u models.User
	if e := s.db.First(&u, uid).Error; e != nil {
		c.JSON(404, err("user not found"))
		return
	}
	u.CareerStats = s.computeCareerStats(&u)
	s.populateAthleteRatings(&u)
	c.JSON(200, gin.H{"success": true, "data": u})
}

func (s *Server) GetUser(c *gin.Context) {
	uid := mustUUID(c.Param("id"))
	if uid == uuid.Nil {
		c.JSON(400, err("invalid user id"))
		return
	}
	var u models.User
	if e := s.db.First(&u, uid).Error; e != nil {
		c.JSON(404, err("user not found"))
		return
	}
	u.CareerStats = s.computeCareerStats(&u)
	s.populateAthleteRatings(&u)
	c.JSON(200, gin.H{"success": true, "data": u})
}

func (s *Server) UpdateMe(c *gin.Context) {
	uid := mustUUID(auth.UserID(c))
	var in struct {
		Name               *string  `json:"name"`
		DateOfBirth        *string  `json:"date_of_birth"`
		Age                *int     `json:"age"`
		HeightCM           *float64 `json:"height_cm"`
		WeightKG           *float64 `json:"weight_kg"`
		PreferredFoot      *string  `json:"preferred_foot"`
		Bio                *string  `json:"bio"`
		ProfilePhotoURL    *string  `json:"profile_photo_url"`
		ProfilePhotoBucket *string  `json:"profile_photo_bucket"`
		ProfilePhotoKey    *string  `json:"profile_photo_key"`
		Position           *string  `json:"position"`
		KitNumber          *int     `json:"kit_number"`
		FavouriteClub      *string  `json:"favourite_club"`
		FavouritePlayer    *string  `json:"favourite_player"`
	}
	if c.BindJSON(&in) != nil {
		c.JSON(400, err("invalid request"))
		return
	}
	var u models.User
	if e := s.db.First(&u, uid).Error; e != nil {
		c.JSON(404, err("user not found"))
		return
	}
	updates := map[string]interface{}{}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		updates["name"] = strings.TrimSpace(*in.Name)
	}
	if in.DateOfBirth != nil && strings.TrimSpace(*in.DateOfBirth) != "" {
		cleanDOB := strings.TrimSpace(*in.DateOfBirth)
		if u.DateOfBirth != "" && cleanDOB != u.DateOfBirth {
			c.JSON(400, err("date of birth cannot be modified"))
			return
		}
		if u.DateOfBirth == "" {
			calcAge, errVal := models.CalculateAgeFromDOB(cleanDOB)
			if errVal != nil {
				c.JSON(400, err("invalid date of birth format, expected YYYY-MM-DD"))
				return
			}
			if calcAge < 5 || calcAge > 120 {
				c.JSON(400, err("age calculated from date of birth must be between 5 and 120"))
				return
			}
			updates["date_of_birth"] = cleanDOB
			updates["age"] = calcAge
		}
	}
	if in.Age != nil && u.DateOfBirth == "" && updates["date_of_birth"] == nil {
		updates["age"] = *in.Age
	}
	if in.HeightCM != nil {
		updates["height_cm"] = *in.HeightCM
	}
	if in.WeightKG != nil {
		updates["weight_kg"] = *in.WeightKG
	}
	if in.PreferredFoot != nil {
		updates["preferred_foot"] = strings.TrimSpace(*in.PreferredFoot)
	}
	if in.Bio != nil {
		updates["bio"] = strings.TrimSpace(*in.Bio)
	}
	var oldKeyToDelete, oldBucketToDelete string
	if in.ProfilePhotoURL != nil || in.ProfilePhotoKey != nil {
		var newBucket, newKey, newCleanURL string
		if in.ProfilePhotoKey != nil && *in.ProfilePhotoKey != "" {
			newKey = strings.TrimSpace(*in.ProfilePhotoKey)
			newBucket = s.storage.BucketName()
			if in.ProfilePhotoBucket != nil && *in.ProfilePhotoBucket != "" {
				newBucket = strings.TrimSpace(*in.ProfilePhotoBucket)
			}
			newCleanURL = newKey
		} else if in.ProfilePhotoURL != nil {
			raw := strings.TrimSpace(*in.ProfilePhotoURL)
			if raw != "" {
				newBucket, newKey = storage.ExtractBucketAndKey(raw, s.storage.BucketName())
				newCleanURL = cleanStoredURL(raw)
			}
		}

		// Determine old bucket and key
		oldKey := u.ProfilePhotoKey
		oldBucket := u.ProfilePhotoBucket
		if oldKey == "" && u.ProfilePhotoURL != "" {
			oldBucket, oldKey = storage.ExtractBucketAndKey(u.ProfilePhotoURL, s.storage.BucketName())
		}
		if oldBucket == "" {
			oldBucket = s.storage.BucketName()
		}

		if oldKey != "" && oldKey != newKey {
			oldKeyToDelete = oldKey
			oldBucketToDelete = oldBucket
		}

		updates["profile_photo_bucket"] = newBucket
		updates["profile_photo_key"] = newKey
		updates["profile_photo_url"] = newCleanURL
	}
	if in.Position != nil {
		updates["position"] = normalizePosition(*in.Position)
	}
	if in.KitNumber != nil {
		newKit := *in.KitNumber
		if newKit < 0 || newKit > 99 {
			c.JSON(400, err("jersey number must be between 1 and 99"))
			return
		}
		if newKit > 0 && newKit != u.KitNumber {
			var activeGroupIDs []uuid.UUID
			s.db.Model(&models.GroupMember{}).
				Where("user_id = ? AND status = 'ACTIVE'", uid).
				Pluck("group_id", &activeGroupIDs)

			if len(activeGroupIDs) > 0 {
				type ConflictInfo struct {
					GroupName string
					UserName  string
				}
				var conflict ConflictInfo
				cErr := s.db.Table("group_members").
					Joins("JOIN users ON users.id = group_members.user_id").
					Joins("JOIN groups ON groups.id = group_members.group_id").
					Where("group_members.group_id IN ? AND group_members.status = 'ACTIVE' AND group_members.user_id != ? AND users.kit_number = ?", activeGroupIDs, uid, newKit).
					Select("groups.name as group_name, users.name as user_name").
					First(&conflict).Error
				if cErr == nil {
					c.JSON(409, err(fmt.Sprintf("jersey number #%d is already taken by %s in %s", newKit, conflict.UserName, conflict.GroupName)))
					return
				}
			}
		}
		updates["kit_number"] = newKit
	}
	if in.FavouriteClub != nil {
		updates["favourite_club"] = strings.TrimSpace(*in.FavouriteClub)
	}
	if in.FavouritePlayer != nil {
		updates["favourite_player"] = strings.TrimSpace(*in.FavouritePlayer)
	}
	if len(updates) > 0 {
		if e := s.db.Model(&u).Updates(updates).Error; e != nil {
			c.JSON(500, err("failed to update profile"))
			return
		}
		if oldKeyToDelete != "" {
			go func(b, k string) {
				delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if delErr := s.storage.DeleteObject(delCtx, b, k); delErr != nil {
					log.Printf("[Storage] Failed to delete previous profile photo %s/%s: %v", b, k, delErr)
				} else {
					log.Printf("[Storage] Deleted previous profile photo %s/%s", b, k)
				}
			}(oldBucketToDelete, oldKeyToDelete)
		}
		if e := s.db.First(&u, uid).Error; e != nil {
			c.JSON(500, err("failed to reload profile"))
			return
		}
	}
	u.CareerStats = s.computeCareerStats(&u)
	s.populateAthleteRatings(&u)
	c.JSON(200, gin.H{"success": true, "data": u})
}

func (s *Server) RegisterDeviceToken(c *gin.Context) {
	uid := mustUUID(auth.UserID(c))
	var in struct {
		Token    string `json:"token"`
		Platform string `json:"platform"`
	}
	if e := c.BindJSON(&in); e != nil || strings.TrimSpace(in.Token) == "" {
		c.JSON(400, err("valid token is required"))
		return
	}
	if in.Platform == "" {
		in.Platform = "android"
	}
	if e := s.pushService.RegisterToken(uid, in.Token, in.Platform); e != nil {
		c.JSON(500, err("failed to register device token"))
		return
	}
	c.JSON(200, gin.H{"success": true, "message": "device token registered"})
}

func (s *Server) DeleteDeviceToken(c *gin.Context) {
	var in struct {
		Token string `json:"token"`
	}
	_ = c.BindJSON(&in)
	if strings.TrimSpace(in.Token) != "" {
		_ = s.pushService.DeleteToken(in.Token)
	}
	c.JSON(200, gin.H{"success": true, "message": "device token removed"})
}
