package rating

import (
	"errors"
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
		tx.Model(&models.GroupMember{}).Where("group_id=? AND user_id=? AND status='ACTIVE'", g, rater).Count(&a)
		tx.Model(&models.GroupMember{}).Where("group_id=? AND user_id=? AND status='ACTIVE'", g, rated).Count(&b)
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
