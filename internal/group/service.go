package group

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"squadup/backend/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct{ DB *gorm.DB }

func New(db *gorm.DB) *Service { return &Service{db} }

func cleanInviteCode(raw string) string {
	raw = strings.TrimSpace(raw)
	if idx := strings.LastIndex(strings.ToLower(raw), "invite code:"); idx != -1 {
		raw = strings.TrimSpace(raw[idx+len("invite code:"):])
	} else if idx := strings.LastIndex(strings.ToLower(raw), "code:"); idx != -1 {
		raw = strings.TrimSpace(raw[idx+len("code:"):])
	}
	raw = strings.Trim(raw, `"'“‘”’.,;:!?`)
	return strings.TrimSpace(raw)
}

func (s *Service) Create(name, desc, city, privacy string, sportID, owner uuid.UUID) (models.Group, error) {
	var g models.Group
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		g = models.Group{Name: name, Description: desc, City: city, Privacy: privacy, SportID: sportID, OwnerID: owner, InviteCode: uuid.NewString()}
		if e := tx.Create(&g).Error; e != nil {
			return e
		}
		m := models.GroupMember{GroupID: g.ID, UserID: owner, Role: "OWNER", Status: "ACTIVE", JoinedAt: time.Now()}
		return tx.Create(&m).Error
	})
	return g, err
}

func (s *Service) JoinByCode(code string, uid uuid.UUID) (models.GroupJoinRequest, error) {
	code = cleanInviteCode(code)
	if code == "" {
		return models.GroupJoinRequest{}, errors.New("invite code is required")
	}

	var g models.Group
	var r models.GroupJoinRequest
	e := s.DB.Transaction(func(tx *gorm.DB) error {
		// Lock the group row so concurrent taps cannot create duplicate requests.
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("LOWER(invite_code) = LOWER(?)", code).
			First(&g).Error; e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				return errors.New("invalid or expired squad invite code")
			}
			return e
		}
		var c int64
		tx.Model(&models.GroupMember{}).Where("group_id=? AND user_id=? AND status='ACTIVE'", g.ID, uid).Count(&c)
		if c > 0 {
			return errors.New("already a member of this squad")
		}

		var requester models.User
		if e := tx.First(&requester, uid).Error; e == nil && requester.KitNumber > 0 {
			var conflictUser models.User
			err := tx.Table("group_members").
				Joins("JOIN users ON users.id = group_members.user_id").
				Where("group_members.group_id = ? AND group_members.status = 'ACTIVE' AND group_members.user_id != ? AND users.kit_number = ?", g.ID, uid, requester.KitNumber).
				Select("users.*").
				First(&conflictUser).Error
			if err == nil {
				return fmt.Errorf("jersey number #%d is already taken by %s in this squad. Please change your jersey number in profile before joining.", requester.KitNumber, conflictUser.Name)
			}
		}

		// If the group is PUBLIC, join directly as ACTIVE member!
		if strings.ToUpper(g.Privacy) == "PUBLIC" {
			var mem models.GroupMember
			if e := tx.Where("group_id=? AND user_id=?", g.ID, uid).First(&mem).Error; e == nil {
				mem.Status = "ACTIVE"
				mem.Role = "MEMBER"
				if e := tx.Save(&mem).Error; e != nil {
					return e
				}
			} else {
				mem = models.GroupMember{
					GroupID:  g.ID,
					UserID:   uid,
					Role:     "MEMBER",
					Status:   "ACTIVE",
					JoinedAt: time.Now(),
				}
				if e := tx.Create(&mem).Error; e != nil {
					return e
				}
			}
			r = models.GroupJoinRequest{
				GroupID: g.ID,
				UserID:  uid,
				Status:  "APPROVED",
			}
			return nil
		}

		var existing models.GroupJoinRequest
		if e := tx.Where("group_id=? AND user_id=? AND status='PENDING'", g.ID, uid).Order("created_at ASC").First(&existing).Error; e == nil {
			r = existing
			return nil
		}
		r = models.GroupJoinRequest{GroupID: g.ID, UserID: uid, Status: "PENDING"}
		if e := tx.Create(&r).Error; e != nil {
			return e
		}
		adminUserIDs := make(map[uuid.UUID]bool)
		if g.OwnerID != uuid.Nil {
			adminUserIDs[g.OwnerID] = true
		}
		var admins []models.GroupMember
		tx.Where("group_id=? AND status='ACTIVE' AND role IN ?", g.ID, []string{"OWNER", "ADMIN"}).Find(&admins)
		for _, admin := range admins {
			adminUserIDs[admin.UserID] = true
		}
		for adminUID := range adminUserIDs {
			requestID, groupID := r.ID, g.ID
			n := models.Notification{UserID: adminUID, GroupID: &groupID, Type: "JOIN_REQUEST", Title: "New join request", Message: requester.Name + " wants to join " + g.Name, EntityType: "GROUP_JOIN_REQUEST", EntityID: &requestID}
			if e := tx.Create(&n).Error; e != nil {
				return e
			}
		}
		return nil
	})
	if e != nil {
		return r, e
	}
	return r, nil
}

func (s *Service) Approve(reqID, adminID uuid.UUID) (models.GroupJoinRequest, error) {
	var r models.GroupJoinRequest
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, reqID).Error; e != nil {
			return e
		}
		if r.Status != "PENDING" {
			return errors.New("join request has already been reviewed")
		}
		var m models.GroupMember
		if e := tx.Where("group_id=? AND user_id=? AND status='ACTIVE'", r.GroupID, adminID).First(&m).Error; e != nil || !(m.Role == "OWNER" || m.Role == "ADMIN") {
			return errors.New("forbidden")
		}
		var requester models.User
		if e := tx.First(&requester, r.UserID).Error; e == nil && requester.KitNumber > 0 {
			var conflictUser models.User
			err := tx.Table("group_members").
				Joins("JOIN users ON users.id = group_members.user_id").
				Where("group_members.group_id = ? AND group_members.status = 'ACTIVE' AND group_members.user_id != ? AND users.kit_number = ?", r.GroupID, r.UserID, requester.KitNumber).
				Select("users.*").
				First(&conflictUser).Error
			if err == nil {
				return fmt.Errorf("cannot approve: jersey number #%d is already taken by %s in this squad", requester.KitNumber, conflictUser.Name)
			}
		}

		r.Status = "APPROVED"
		r.ReviewedByID = &adminID
		if e := tx.Save(&r).Error; e != nil {
			return e
		}

		var mem models.GroupMember
		if e := tx.Where("group_id=? AND user_id=?", r.GroupID, r.UserID).First(&mem).Error; e == nil {
			mem.Status = "ACTIVE"
			mem.Role = "MEMBER"
			if e := tx.Save(&mem).Error; e != nil {
				return e
			}
		} else {
			mem = models.GroupMember{GroupID: r.GroupID, UserID: r.UserID, Role: "MEMBER", Status: "ACTIVE", JoinedAt: time.Now()}
			if e := tx.Create(&mem).Error; e != nil {
				return e
			}
		}

		var g models.Group
		if e := tx.First(&g, r.GroupID).Error; e == nil {
			groupID := r.GroupID
			n := models.Notification{
				UserID:     r.UserID,
				GroupID:    &groupID,
				Type:       "JOIN_APPROVED",
				Title:      "Request Approved! ⚽",
				Message:    "Your request to join " + g.Name + " was approved. Welcome to the squad!",
				EntityType: "GROUP",
				EntityID:   &groupID,
			}
			_ = tx.Create(&n).Error
		}
		return nil
	})
	return r, err
}

func (s *Service) Reject(reqID, adminID uuid.UUID) (models.GroupJoinRequest, error) {
	var r models.GroupJoinRequest
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, reqID).Error; e != nil {
			return e
		}
		if r.Status != "PENDING" {
			return errors.New("join request has already been reviewed")
		}
		var m models.GroupMember
		if e := tx.Where("group_id=? AND user_id=? AND status='ACTIVE'", r.GroupID, adminID).First(&m).Error; e != nil || !(m.Role == "OWNER" || m.Role == "ADMIN") {
			return errors.New("forbidden")
		}
		r.Status = "REJECTED"
		r.ReviewedByID = &adminID
		if e := tx.Save(&r).Error; e != nil {
			return e
		}

		var g models.Group
		if e := tx.First(&g, r.GroupID).Error; e == nil {
			groupID := r.GroupID
			n := models.Notification{
				UserID:     r.UserID,
				GroupID:    &groupID,
				Type:       "JOIN_REJECTED",
				Title:      "Request Declined",
				Message:    "Your request to join " + g.Name + " was declined.",
				EntityType: "GROUP",
				EntityID:   &groupID,
			}
			_ = tx.Create(&n).Error
		}
		return nil
	})
	return r, err
}
