// Package route decides the order in which one request tries a model's targets.
package route

import "github.com/vivekdavara/switchboard/internal/config"

// Order returns the targets in the order a request tries them: first those with a weight, each next one picked at
// random in proportion to the weights not yet picked (so the first attempt goes to a target with probability
// weight / total, and a fallback prefers the heavier of the rest); then the standbys (weight 0), in the order the
// configuration lists them. intn(n) must return a uniform random integer in [0, n), as math/rand/v2's IntN does.
func Order(targets []config.Target, intn func(n int) int) []config.Target {
	out := make([]config.Target, 0, len(targets))
	var weighted, standby []config.Target
	total := 0
	for _, t := range targets {
		if t.Weight > 0 {
			weighted = append(weighted, t)
			total += t.Weight
		} else {
			standby = append(standby, t)
		}
	}
	for len(weighted) > 0 {
		i := pick(weighted, intn(total))
		out = append(out, weighted[i])
		total -= weighted[i].Weight
		weighted = append(weighted[:i:i], weighted[i+1:]...)
	}
	return append(out, standby...)
}

// pick returns the index of the target that point falls on, when the weights are laid end to end.
func pick(targets []config.Target, point int) int {
	for i, t := range targets {
		if point < t.Weight {
			return i
		}
		point -= t.Weight
	}
	return len(targets) - 1 // unreachable for point in [0, total)
}
