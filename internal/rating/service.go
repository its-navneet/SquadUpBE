package rating

import (
	"errors"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"squadup/backend/internal/models"
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
	var a, b int64
	s.DB.Model(&models.GroupMember{}).Where("group_id=? AND user_id=? AND status='ACTIVE'", g, rater).Count(&a)
	s.DB.Model(&models.GroupMember{}).Where("group_id=? AND user_id=? AND status='ACTIVE'", g, rated).Count(&b)
	if a == 0 || b == 0 {
		return models.PlayerRating{}, errors.New("both players must be active members")
	}
	var p models.PlayerRating
	e := s.DB.Where("group_id=? AND rater_user_id=? AND rated_user_id=?", g, rater, rated).First(&p).Error
	if e == gorm.ErrRecordNotFound {
		p = models.PlayerRating{GroupID: g, RaterUserID: rater, RatedUserID: rated, Overall: overall}
		if e = s.DB.Create(&p).Error; e != nil {
			return p, e
		}
	} else if e != nil {
		return p, e
	} else {
		p.Overall = overall
		if e = s.DB.Save(&p).Error; e != nil {
			return p, e
		}
		s.DB.Where("player_rating_id=?", p.ID).Delete(&models.PlayerRatingAttribute{})
	}
	for k, v := range attrs {
		s.DB.Create(&models.PlayerRatingAttribute{PlayerRatingID: p.ID, Attribute: k, Value: v})
	}
	return p, nil
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
