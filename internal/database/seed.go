package database

import (
	"log"

	"squadup/backend/internal/models"

	"gorm.io/gorm"
)

// Seed populates initial sports and position codes, and migrates legacy position strings.
func Seed(db *gorm.DB) {
	var s models.Sport
	if db.Where("name=?", "Football").First(&s).Error != nil {
		s = models.Sport{Name: "Football", Icon: "⚽", Active: true}
		if err := db.Create(&s).Error; err != nil {
			log.Printf("[Seed] Failed to create sport Football: %v", err)
			return
		}
	}

	desired := []struct {
		Name string
		Code string
	}{
		{Name: "Goalkeeper", Code: "GK"},
		{Name: "Defender", Code: "DEF"},
		{Name: "Midfielder", Code: "MID"},
		{Name: "Striker", Code: "ST"},
	}

	validCodes := []string{"GK", "DEF", "MID", "ST"}
	// Keep only the 4 canonical positions for Football
	db.Where("sport_id = ? AND code NOT IN ?", s.ID, validCodes).Delete(&models.SportPosition{})

	for i, p := range desired {
		var sp models.SportPosition
		if err := db.Where("sport_id = ? AND code = ?", s.ID, p.Code).First(&sp).Error; err != nil {
			db.Create(&models.SportPosition{
				SportID:   s.ID,
				Name:      p.Name,
				Code:      p.Code,
				SortOrder: i,
			})
		} else {
			db.Model(&sp).Updates(map[string]interface{}{
				"name":       p.Name,
				"sort_order": i,
			})
		}
	}

	// Migrate legacy position codes in users table
	db.Model(&models.User{}).Where("UPPER(TRIM(position)) IN ?", []string{"MF", "MIDFIELDER", "CM", "CAM", "CDM", "LM", "RM"}).Update("position", "MID")
	db.Model(&models.User{}).Where("UPPER(TRIM(position)) IN ?", []string{"CB", "FB", "DEFENDER", "LB", "RB", "CENTRE BACK", "FULL BACK"}).Update("position", "DEF")
	db.Model(&models.User{}).Where("UPPER(TRIM(position)) IN ?", []string{"WG", "FWD", "FORWARD", "STRIKER", "CF", "ATTACKER", "LW", "RW", "WINGER"}).Update("position", "ST")
	db.Model(&models.User{}).Where("UPPER(TRIM(position)) IN ?", []string{"GOALKEEPER", "GOALIE", "KEEPER"}).Update("position", "GK")
}

