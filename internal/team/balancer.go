package team

import (
	"github.com/google/uuid"
	"sort"
)

type Player struct {
	UserID     uuid.UUID
	Name       string
	Rating     float64
	Position   string
	Goalkeeper bool
}

func Generate(players []Player, n int) [][]Player {
	if n < 2 {
		n = 2
	}
	sort.Slice(players, func(i, j int) bool { return players[i].Rating > players[j].Rating })
	out := make([][]Player, n)
	sum := make([]float64, n)
	for _, p := range players {
		idx := 0
		for i := 1; i < n; i++ {
			if sum[i] < sum[idx] {
				idx = i
			}
		}
		out[idx] = append(out[idx], p)
		sum[idx] += p.Rating
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
