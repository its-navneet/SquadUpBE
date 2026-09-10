package team

import (
	"sort"
	"strings"

	"github.com/google/uuid"
)

type Player struct {
	UserID     uuid.UUID
	Name       string
	Rating     float64
	Position   string
	Goalkeeper bool
}

// Generate distributes players across n teams, balancing goalkeepers first,
// followed by outfield players, sorted by rating descending (greedy balance).
// It does not mutate the input slice.
func Generate(players []Player, n int) [][]Player {
	if n < 2 {
		n = 2
	}
	out := make([][]Player, n)
	for i := range out {
		out[i] = []Player{}
	}
	if len(players) == 0 {
		return out
	}

	// Copy slice to preserve caller's slice immutability
	pCopy := make([]Player, len(players))
	copy(pCopy, players)

	var gks []Player
	var outfield []Player
	for _, p := range pCopy {
		isGK := p.Goalkeeper || strings.EqualFold(p.Position, "GK") || strings.EqualFold(p.Position, "GOALKEEPER")
		if isGK {
			gks = append(gks, p)
		} else {
			outfield = append(outfield, p)
		}
	}

	sort.Slice(gks, func(i, j int) bool { return gks[i].Rating > gks[j].Rating })
	sort.Slice(outfield, func(i, j int) bool { return outfield[i].Rating > outfield[j].Rating })

	sum := make([]float64, n)

	// Distribute goalkeepers first across teams
	for _, gk := range gks {
		minIdx := 0
		for i := 1; i < n; i++ {
			if len(out[i]) < len(out[minIdx]) || (len(out[i]) == len(out[minIdx]) && sum[i] < sum[minIdx]) {
				minIdx = i
			}
		}
		out[minIdx] = append(out[minIdx], gk)
		sum[minIdx] += gk.Rating
	}

	// Distribute outfield players
	for _, p := range outfield {
		minIdx := 0
		for i := 1; i < n; i++ {
			if sum[i] < sum[minIdx] {
				minIdx = i
			}
		}
		out[minIdx] = append(out[minIdx], p)
		sum[minIdx] += p.Rating
	}

	return out
}

func Strength(players []Player) float64 {
	if len(players) == 0 {
		return 0
	}
	var s float64
	for _, p := range players {
		s += p.Rating
	}
	return s / float64(len(players))
}
