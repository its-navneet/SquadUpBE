package rating

import (
	"errors"
	"math"
	"squadup/backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Service struct{ DB *gorm.DB }

func New(db *gorm.DB) *Service { return &Service{db} }
func (s *Service) Upsert(g, rater, rated uuid.UUID, overall float64, attrs map[string]float64) (models.PlayerRating, error) {
	if rater == rated {
		return models.PlayerRating{}, errors.New("cannot rate yourself")
	}
	if overall < 1 || overall > 10 {
		return models.PlayerRating{}, errors.New("rating must be 1-10")
	}
	var p models.PlayerRating
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var a, b int64
		tx.Model(&models.GroupMember{}).Where("group_id=? AND user_id=? AND (TRIM(UPPER(status))='ACTIVE' OR status='' OR status IS NULL)", g, rater).Count(&a)
		tx.Model(&models.GroupMember{}).Where("group_id=? AND user_id=? AND (TRIM(UPPER(status))='ACTIVE' OR status='' OR status IS NULL)", g, rated).Count(&b)
		if a == 0 {
			var ownerCount int64
			tx.Model(&models.Group{}).Where("id=? AND owner_id=?", g, rater).Count(&ownerCount)
			if ownerCount > 0 {
				a = 1
			}
		}
		if b == 0 {
			var ownerCount int64
			tx.Model(&models.Group{}).Where("id=? AND owner_id=?", g, rated).Count(&ownerCount)
			if ownerCount > 0 {
				b = 1
			}
		}
		if a == 0 || b == 0 {
			return errors.New("both players must be active members")
		}
		e := tx.Where("group_id=? AND rater_user_id=? AND rated_user_id=?", g, rater, rated).First(&p).Error
		if e == gorm.ErrRecordNotFound {
			p = models.PlayerRating{GroupID: g, RaterUserID: rater, RatedUserID: rated, Overall: overall}
			if e = tx.Create(&p).Error; e != nil {
				return e
			}
		} else if e != nil {
			return e
		} else {
			p.Overall = overall
			if e = tx.Save(&p).Error; e != nil {
				return e
			}
			if e = tx.Where("player_rating_id=?", p.ID).Delete(&models.PlayerRatingAttribute{}).Error; e != nil {
				return e
			}
		}
		for k, v := range attrs {
			attr := models.PlayerRatingAttribute{PlayerRatingID: p.ID, Attribute: k, Value: v}
			if e := tx.Create(&attr).Error; e != nil {
				return e
			}
		}
		return nil
	})
	return p, err
}
func (s *Service) Average(g, u uuid.UUID) (float64, int64, error) {
	var avg float64
	var count int64
	if e := s.DB.Model(&models.PlayerRating{}).Where("group_id=? AND rated_user_id=?", g, u).Select("COALESCE(AVG(overall),0)").Scan(&avg).Error; e != nil {
		return 0, 0, e
	}
	if e := s.DB.Model(&models.PlayerRating{}).Where("group_id=? AND rated_user_id=?", g, u).Count(&count).Error; e != nil {
		return 0, 0, e
	}
	return avg, count, nil
}

func (s *Service) AverageWithAttributes(g, u uuid.UUID) (float64, int64, map[string]float64, error) {
	avg, count, err := s.Average(g, u)
	if err != nil {
		return 0, 0, nil, err
	}
	attrs := make(map[string]float64)
	if count == 0 {
		return avg, 0, attrs, nil
	}

	type attrAvg struct {
		Attribute string  `gorm:"column:attribute"`
		AvgValue  float64 `gorm:"column:avg_value"`
	}
	var res []attrAvg
	err = s.DB.Table("player_rating_attributes").
		Joins("JOIN player_ratings ON player_ratings.id = player_rating_attributes.player_rating_id").
		Where("player_ratings.group_id = ? AND player_ratings.rated_user_id = ?", g, u).
		Select("player_rating_attributes.attribute, AVG(player_rating_attributes.value) as avg_value").
		Group("player_rating_attributes.attribute").
		Scan(&res).Error
	if err != nil {
		return avg, count, attrs, err
	}

	for _, r := range res {
		attrs[r.Attribute] = math.Round(r.AvgValue*10) / 10
	}
	return math.Round(avg*10) / 10, count, attrs, nil
}
