package harbor

import "sort"

// CompareRow sums up one model in one arm of one task. Pass rate, time,
// tokens and cost count graded attempts only (pass or fail): an error says
// nothing about the model.
type CompareRow struct {
	Task          string   `json:"task"` // the base name
	Model         string   `json:"model"`
	Arm           string   `json:"arm"`
	Passes        int      `json:"passes"`
	Attempts      int      `json:"attempts"`
	Errors        int      `json:"errors"`
	Pending       int      `json:"pending"` // queued or running
	MedianSeconds *float64 `json:"median_seconds"`
	MedianFresh   *int64   `json:"median_fresh_tokens"` // Tokens.Fresh
	MedianCached  *int64   `json:"median_cached_tokens"`
	MeanCost      *float64 `json:"mean_cost"`
}

// Compare groups trials by task, model and arm.
func Compare(trials []Trial) []CompareRow {
	type key struct{ task, model, arm string }
	type group struct {
		row     CompareRow
		seconds []float64
		fresh   []int64
		cached  []int64
		cost    float64
		costed  int
	}
	groups := map[key]*group{}
	var order []key
	for _, t := range trials {
		base, _ := SplitArm(t.Task)
		k := key{base, t.Model, t.Arm}
		g := groups[k]
		if g == nil {
			g = &group{row: CompareRow{Task: base, Model: t.Model, Arm: t.Arm}}
			groups[k] = g
			order = append(order, k)
		}
		switch t.Status {
		case Queued, Running:
			g.row.Pending++
			continue
		case Error:
			g.row.Errors++
			continue
		case Pass:
			g.row.Passes++
		}
		g.row.Attempts++
		if t.Seconds != nil {
			g.seconds = append(g.seconds, *t.Seconds)
		}
		g.fresh = append(g.fresh, t.Tokens.Fresh())
		g.cached = append(g.cached, t.Tokens.Cached)
		if t.Cost != nil {
			g.cost += *t.Cost
			g.costed++
		}
	}
	sort.Slice(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.task != b.task {
			return a.task < b.task
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return CompareArms(a.arm, b.arm)
	})
	rows := []CompareRow{}
	for _, k := range order {
		g := groups[k]
		if len(g.seconds) > 0 {
			m := median(g.seconds)
			g.row.MedianSeconds = &m
		}
		if len(g.fresh) > 0 {
			f, c := median(g.fresh), median(g.cached)
			g.row.MedianFresh, g.row.MedianCached = &f, &c
		}
		if g.costed > 0 {
			m := g.cost / float64(g.costed)
			g.row.MeanCost = &m
		}
		rows = append(rows, g.row)
	}
	return rows
}

// median of an even count is the mean of the middle two.
func median[T int64 | float64](v []T) T {
	s := append([]T(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
