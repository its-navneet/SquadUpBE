package api

import (
	"strings"

	"squadup/backend/internal/auth"
	"squadup/backend/internal/models"

	"github.com/gin-gonic/gin"
)

func resolveVenue(venue, mapURL string) (string, string) {
	venue = strings.TrimSpace(venue)
	mapURL = strings.TrimSpace(mapURL)

	isURL := func(s string) bool {
		lower := strings.ToLower(s)
		return strings.HasPrefix(lower, "http://") ||
			strings.HasPrefix(lower, "https://") ||
			strings.HasPrefix(lower, "maps.app.goo.gl") ||
			strings.HasPrefix(lower, "goo.gl/maps") ||
			strings.HasPrefix(lower, "maps.google.") ||
			strings.HasPrefix(lower, "www.google.com/maps")
	}

	normalizeURL := func(s string) string {
		s = strings.TrimSpace(s)
		lower := strings.ToLower(s)
		if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
			return "https://" + s
		}
		return s
	}

	if mapURL != "" && isURL(mapURL) {
		mapURL = normalizeURL(mapURL)
	}

	if isURL(venue) {
		if mapURL == "" {
			mapURL = normalizeURL(venue)
		}
		return venue, mapURL
	}

	words := strings.Fields(venue)
	var textWords []string
	for _, w := range words {
		if isURL(w) {
			if mapURL == "" {
				mapURL = normalizeURL(w)
			}
		} else {
			textWords = append(textWords, w)
		}
	}

	if len(textWords) > 0 && mapURL != "" && len(textWords) < len(words) {
		venue = strings.Join(textWords, " ")
		venue = strings.Trim(venue, " -:,|")
	}

	return venue, mapURL
}

func (s *Server) CreateVenue(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustAdmin(s.db, gid, uid) {
		c.JSON(403, err("admin only"))
		return
	}
	var in struct {
		Name, Address, GoogleMapsURL, Notes string
		Latitude, Longitude                 *float64
		IsDefault                           bool
	}
	if c.BindJSON(&in) != nil || in.Name == "" {
		c.JSON(400, err("venue name required"))
		return
	}
	v := models.Venue{GroupID: gid, Name: in.Name, Address: in.Address, GoogleMapsURL: in.GoogleMapsURL, Notes: in.Notes, Latitude: in.Latitude, Longitude: in.Longitude, IsDefault: in.IsDefault}
	if e := s.db.Create(&v).Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}
	c.JSON(201, gin.H{"success": true, "data": v})
}

func (s *Server) ListVenues(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	uid := mustUUID(auth.UserID(c))
	if !mustMember(s.db, gid, uid) {
		c.JSON(403, err("not a group member"))
		return
	}
	var vs []models.Venue
	s.db.Where("group_id=?", gid).Order("is_default DESC, name ASC").Find(&vs)
	c.JSON(200, gin.H{"success": true, "data": vs})
}

func (s *Server) UpdateVenue(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	vid := mustUUID(c.Param("venueId"))
	uid := mustUUID(auth.UserID(c))
	if !mustAdmin(s.db, gid, uid) {
		c.JSON(403, err("admin only"))
		return
	}
	var v models.Venue
	if e := s.db.Where("id = ? AND group_id = ?", vid, gid).First(&v).Error; e != nil {
		c.JSON(404, err("venue not found"))
		return
	}
	var in struct {
		Name          *string  `json:"name"`
		Address       *string  `json:"address"`
		GoogleMapsURL *string  `json:"google_maps_url"`
		Notes         *string  `json:"notes"`
		Latitude      *float64 `json:"latitude"`
		Longitude     *float64 `json:"longitude"`
		IsDefault     *bool    `json:"is_default"`
	}
	if c.BindJSON(&in) != nil {
		c.JSON(400, err("invalid request"))
		return
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		v.Name = strings.TrimSpace(*in.Name)
	}
	if in.Address != nil {
		v.Address = strings.TrimSpace(*in.Address)
	}
	if in.GoogleMapsURL != nil {
		v.GoogleMapsURL = strings.TrimSpace(*in.GoogleMapsURL)
	}
	if in.Notes != nil {
		v.Notes = strings.TrimSpace(*in.Notes)
	}
	if in.Latitude != nil {
		v.Latitude = in.Latitude
	}
	if in.Longitude != nil {
		v.Longitude = in.Longitude
	}
	if in.IsDefault != nil {
		v.IsDefault = *in.IsDefault
		if v.IsDefault {
			s.db.Model(&models.Venue{}).Where("group_id = ? AND id != ?", gid, vid).Update("is_default", false)
		}
	}
	if e := s.db.Save(&v).Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}
	c.JSON(200, gin.H{"success": true, "data": v})
}

func (s *Server) DeleteVenue(c *gin.Context) {
	gid := mustUUID(c.Param("id"))
	vid := mustUUID(c.Param("venueId"))
	uid := mustUUID(auth.UserID(c))
	if !mustAdmin(s.db, gid, uid) {
		c.JSON(403, err("admin only"))
		return
	}
	var v models.Venue
	if e := s.db.Where("id = ? AND group_id = ?", vid, gid).First(&v).Error; e != nil {
		c.JSON(404, err("venue not found"))
		return
	}
	// Nullify venue_id on matches referencing this venue
	s.db.Model(&models.Match{}).Where("venue_id = ?", vid).Update("venue_id", nil)

	if e := s.db.Delete(&v).Error; e != nil {
		c.JSON(500, err(e.Error()))
		return
	}
	c.JSON(200, gin.H{"success": true})
}
