package route

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/vivekdavara/switchboard/internal/config"
)

func targets(spec ...any) []config.Target {
	var out []config.Target
	for i := 0; i < len(spec); i += 2 {
		out = append(out, config.Target{Provider: &config.Provider{Name: spec[i].(string)}, Weight: spec[i+1].(int)})
	}
	return out
}

func names(ts []config.Target) string {
	var s []string
	for _, t := range ts {
		s = append(s, t.Provider.Name)
	}
	return strings.Join(s, ",")
}

func TestEveryPointOfTheWeightsPicksItsTarget(t *testing.T) {
	ts := targets("a", 3, "s", 0, "b", 1, "c", 2)
	// Laid end to end, a's weight covers 0-2, b's 3, c's 4-5; the standby s always comes last.
	for point, want := range []string{"a,b,c,s", "a,b,c,s", "a,b,c,s", "b,a,c,s", "c,a,b,s", "c,a,b,s"} {
		first := true
		got := Order(ts, func(n int) int {
			if first {
				first = false
				if n != 6 {
					t.Errorf("the first draw is over %d, want the total weight 6", n)
				}
				return point
			}
			return 0 // then always the first of what is left
		})
		if names(got) != want {
			t.Errorf("point %d: %s, want %s", point, names(got), want)
		}
	}
}

func TestTheFirstTargetIsPickedInProportionToTheWeights(t *testing.T) {
	ts := targets("a", 3, "b", 1, "standby", 0)
	rng := rand.New(rand.NewPCG(1, 2))
	const n = 40000
	first, second := map[string]int{}, map[string]int{}
	for range n {
		o := Order(ts, rng.IntN)
		if len(o) != 3 || o[2].Provider.Name != "standby" {
			t.Fatalf("order %s", names(o))
		}
		first[o[0].Provider.Name]++
		second[o[1].Provider.Name]++
	}
	// a should be first 3/4 of the time; the binomial's standard deviation over 40,000 draws is about 87, so 5 of
	// them (435) is a bound no correct order misses by chance.
	if d := math.Abs(float64(first["a"]) - 0.75*n); d > 5*math.Sqrt(n*0.75*0.25) {
		t.Errorf("a was first %d times of %d, want about %d", first["a"], n, 3*n/4)
	}
	if first["a"]+first["b"] != n || second["b"] != first["a"] {
		t.Errorf("first %v, second %v: the fallback must be the other weighted target", first, second)
	}
}

func TestOrderLeavesItsInputAlone(t *testing.T) {
	ts := targets("a", 1, "b", 1, "c", 1)
	Order(ts, func(n int) int { return n - 1 })
	if names(ts) != "a,b,c" {
		t.Errorf("the model's targets were reordered: %s", names(ts))
	}
}

func TestTheFallbackFollowsTheWeightsLeft(t *testing.T) {
	// a 3, b 2, c 1: once a is first, b should be second in 2 of 3 orders and c in 1 (the weights left, 2 and 1).
	// Drawing the second over the whole 6 instead would give c 4 of 6 (the day-2 review's survivor).
	ts := targets("a", 3, "b", 2, "c", 1)
	rng := rand.New(rand.NewPCG(3, 4))
	afterA, bSecond := 0, 0
	for range 60000 {
		o := Order(ts, rng.IntN)
		if o[0].Provider.Name == "a" {
			afterA++
			if o[1].Provider.Name == "b" {
				bSecond++
			}
		}
	}
	p := 2.0 / 3
	if d := math.Abs(float64(bSecond) - p*float64(afterA)); d > 5*math.Sqrt(float64(afterA)*p*(1-p)) {
		t.Errorf("b was second in %d of %d orders after a, want about %.0f", bSecond, afterA, p*float64(afterA))
	}
}
