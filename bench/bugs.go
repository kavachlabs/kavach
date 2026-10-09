// Package bench is Kavach's fix-verification benchmark: ten planted bugs, each
// in a small journal-driven handler, with a correct fix and a plausible but
// too-narrow fix for each. See BENCHMARKS.md for what it measures.
package bench

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	kavach "github.com/kavachlabs/kavach/sdk/go"
)

// Mode selects which build of a handler is under test.
type Mode int

const (
	Buggy  Mode = iota // the planted bug
	Fixed              // a fix that handles every input of the failing kind
	Narrow             // a fix that handles the recorded incident but not its siblings
)

func (m Mode) String() string { return [...]string{"buggy", "fixed", "narrow"}[m] }

// Bug is one planted bug and the events that trigger it.
type Bug struct {
	Name  string
	Class string // how the old build fails: panic, error or invariant
	Story string // what the bug is, and what the narrow fix gets wrong
	// Start is the first clock read of the recording; zero means 2026-01-01.
	Start time.Time
	// Events are JSON inputs; the last one triggers the incident.
	Events []string
	New    func(Mode) kavach.Handler
}

// Bugs returns the ten benchmark bugs.
func Bugs() []Bug {
	return []Bug{
		{
			Name: "nil-email", Class: "panic",
			Story: "signup with \"email\": null dereferences a nil *string. Narrow fix guards only signups; updates still crash.",
			Events: []string{
				`{"id":"e1","type":"signup","user":"ann","email":"ann@example.com"}`,
				`{"id":"e2","type":"signup","user":"bob","email":"bob@example.org"}`,
				`{"id":"e3","type":"update","user":"ann","email":"ann@corp.example.com"}`,
				`{"id":"e4","type":"signup","user":"cy","email":"cy@example.com"}`,
				`{"id":"e5","type":"update","user":"bob","email":"bob@corp.example.org"}`,
				`{"id":"e6","type":"signup","user":"dee","email":null}`,
			},
			New: func(m Mode) kavach.Handler { return &profiles{mode: m} },
		},
		{
			Name: "empty-order", Class: "panic",
			Story: "an order with no items indexes items[0] for its shipping label. Narrow fix guards only orders without a coupon.",
			Events: []string{
				`{"id":"o1","items":["pen","ink"],"coupon":"SAVE5"}`,
				`{"id":"o2","items":["pad"]}`,
				`{"id":"o3","items":["tape","glue"],"coupon":"SAVE5"}`,
				`{"id":"o4","items":["clip"]}`,
				`{"id":"o5","items":[]}`,
			},
			New: func(m Mode) kavach.Handler { return &orders{mode: m} },
		},
		{
			Name: "zero-count", Class: "panic",
			Story: "an average over count 0 divides by zero. Narrow fix guards only the latency metric.",
			Events: []string{
				`{"id":"m1","metric":"latency","total":900,"count":3}`,
				`{"id":"m2","metric":"errors","total":4,"count":2}`,
				`{"id":"m3","metric":"latency","total":150,"count":1}`,
				`{"id":"m4","metric":"errors","total":9,"count":3}`,
				`{"id":"m5","metric":"latency","total":0,"count":0}`,
			},
			New: func(m Mode) kavach.Handler { return &metrics{mode: m, sum: map[string]int64{}} },
		},
		{
			Name: "nil-map", Class: "panic",
			Story: "stock for a warehouse that was never seeded (wh-3) writes into a nil inner map. Narrow fix initializes it only for restocks, not returns.",
			Events: []string{
				`{"id":"s1","op":"restock","wh":"wh-1","sku":"bolt","qty":50}`,
				`{"id":"s2","op":"return","wh":"wh-1","sku":"nut","qty":5}`,
				`{"id":"s3","op":"ship","wh":"wh-1","sku":"bolt","qty":20}`,
				`{"id":"s4","op":"restock","wh":"wh-2","sku":"nut","qty":10}`,
				`{"id":"s5","op":"restock","wh":"wh-3","sku":"bolt","qty":7}`,
			},
			New: func(m Mode) kavach.Handler {
				return &inventory{mode: m, stock: map[string]map[string]int{"wh-1": {}, "wh-2": {}}}
			},
		},
		{
			Name: "string-qty", Class: "panic",
			Story: "\"qty\" arrives as a string and a float64 type assertion panics. Narrow fix converts it only for orders, not returns.",
			Events: []string{
				`{"id":"p1","type":"order","qty":2,"price":300}`,
				`{"id":"p2","type":"return","qty":1,"price":300}`,
				`{"id":"p3","type":"order","qty":5,"price":120}`,
				`{"id":"p4","type":"return","qty":2,"price":120}`,
				`{"id":"p5","type":"order","qty":"12","price":300}`,
			},
			New: func(m Mode) kavach.Handler { return &pricing{mode: m} },
		},
		{
			Name: "group-overbook", Class: "invariant",
			Story: "group bookings skip the capacity check, so a show is oversold and invariant seats_within_capacity breaks. Narrow fix checks only show-b.",
			Events: []string{
				`{"id":"b1","show":"show-a","kind":"single","seats":2}`,
				`{"id":"b2","show":"show-b","kind":"single","seats":2}`,
				`{"id":"b3","show":"show-a","kind":"group","seats":2}`,
				`{"id":"b4","show":"show-b","kind":"single","seats":1}`,
				`{"id":"b5","show":"show-a","kind":"single","seats":1}`,
				`{"id":"b6","show":"show-b","kind":"group","seats":4}`,
			},
			New: func(m Mode) kavach.Handler { return &seats{mode: m, taken: map[string]int{}} },
		},
		{
			Name: "update-before-create", Class: "error",
			Story: "an update for an unknown customer, delivered before its create, returns an error. Narrow fix upserts only pro-plan customers.",
			Events: []string{
				`{"id":"c1","op":"create","cust":"k1","plan":"free"}`,
				`{"id":"c2","op":"create","cust":"k2","plan":"pro"}`,
				`{"id":"c3","op":"update","cust":"k1","plan":"pro"}`,
				`{"id":"c4","op":"update","cust":"k2","plan":"free"}`,
				`{"id":"c5","op":"update","cust":"k3","plan":"pro"}`,
			},
			New: func(m Mode) kavach.Handler { return &customers{mode: m, plan: map[string]string{}} },
		},
		{
			Name: "redelivered-charge", Class: "invariant",
			Story: "an at-least-once source redelivers a charge and it is applied twice, breaking no_duplicate_application. Narrow fix dedupes only charges, not refunds.",
			Events: []string{
				`{"id":"t1","type":"charge","account":"a","cents":500}`,
				`{"id":"t2","type":"refund","account":"a","cents":100}`,
				`{"id":"t3","type":"charge","account":"b","cents":900}`,
				`{"id":"t4","type":"refund","account":"b","cents":300}`,
				`{"id":"t5","type":"charge","account":"a","cents":250}`,
				`{"id":"t3","type":"charge","account":"b","cents":900}`,
			},
			New: func(m Mode) kavach.Handler {
				return &payments{mode: m, applied: map[string]int{}, balance: map[string]int64{}}
			},
		},
		{
			Name: "leap-day", Class: "panic",
			Story: "day 366 of a leap year indexes past a [365] array. Narrow fix resizes only the hits array, not sales.",
			Start: time.Date(2024, 12, 30, 15, 0, 0, 0, time.UTC),
			Events: []string{
				`{"id":"d1","type":"hit","n":3}`,
				`{"id":"d2","type":"sale","n":1}`,
				`{"id":"d3","type":"hit","n":4}`,
				`{"id":"d4","type":"sale","n":2}`,
				`{"id":"d5","type":"hit","n":2}`,
				`{"id":"d6","type":"sale","n":1}`,
				`{"id":"d7","type":"hit","n":1}`,
			},
			New: func(m Mode) kavach.Handler { return &daily{mode: m} },
		},
		{
			Name: "exhausted-pool", Class: "panic",
			Story: "drawing from an empty ticket pool takes a random value modulo zero. Narrow fix guards only the daily pool.",
			Events: []string{
				`{"id":"w1","pool":"daily"}`,
				`{"id":"w2","pool":"weekly"}`,
				`{"id":"w3","pool":"daily"}`,
				`{"id":"w4","pool":"weekly"}`,
				`{"id":"w5","pool":"daily"}`,
				`{"id":"w6","pool":"weekly"}`,
				`{"id":"w7","pool":"daily"}`,
			},
			New: func(m Mode) kavach.Handler { return &lottery{mode: m, pools: map[string][]int{}} },
		},
	}
}

func decode(in kavach.Input, v any) error {
	if err := json.Unmarshal(in.Data, v); err != nil {
		return fmt.Errorf("decode %s: %w", in.Position, err)
	}
	return nil
}

func emit(env kavach.Env, sink string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	env.Emit(sink, b)
	return nil
}

func reject(env kavach.Env, id, reason string) error {
	return emit(env, "rejections", map[string]string{"event": id, "reason": reason})
}

// 1. nil-email

type profiles struct{ mode Mode }

func (p *profiles) Handle(env kavach.Env, in kavach.Input) error {
	var ev struct {
		ID, Type, User string
		Email          *string
	}
	if err := decode(in, &ev); err != nil {
		return err
	}
	if ev.Email == nil && (p.mode == Fixed || (p.mode == Narrow && ev.Type == "signup")) {
		return reject(env, ev.ID, "missing email")
	}
	domain := strings.ToLower((*ev.Email)[strings.Index(*ev.Email, "@")+1:])
	return emit(env, "profiles", map[string]string{"user": ev.User, "domain": domain, "at": env.Now().Format(time.RFC3339)})
}

// 2. empty-order

type orders struct{ mode Mode }

func (o *orders) Handle(env kavach.Env, in kavach.Input) error {
	var ev struct {
		ID     string
		Items  []string
		Coupon string
	}
	if err := decode(in, &ev); err != nil {
		return err
	}
	if len(ev.Items) == 0 && (o.mode == Fixed || (o.mode == Narrow && ev.Coupon == "")) {
		return reject(env, ev.ID, "no items")
	}
	return emit(env, "labels", map[string]any{"order": ev.ID, "label": strings.ToUpper(ev.Items[0]), "n": len(ev.Items), "at": env.Now().Unix()})
}

// 3. zero-count

type metrics struct {
	mode Mode
	sum  map[string]int64
}

func (m *metrics) Handle(env kavach.Env, in kavach.Input) error {
	var ev struct {
		ID, Metric   string
		Total, Count int64
	}
	if err := decode(in, &ev); err != nil {
		return err
	}
	if ev.Count == 0 && (m.mode == Fixed || (m.mode == Narrow && ev.Metric == "latency")) {
		return reject(env, ev.ID, "empty window")
	}
	avg := ev.Total / ev.Count
	m.sum[ev.Metric] += avg
	return emit(env, "averages", map[string]any{"metric": ev.Metric, "avg": avg, "running": m.sum[ev.Metric]})
}

// 4. nil-map

type inventory struct {
	mode  Mode
	stock map[string]map[string]int
}

func (s *inventory) Handle(env kavach.Env, in kavach.Input) error {
	var ev struct {
		ID, Op, WH, SKU string
		Qty             int
	}
	if err := decode(in, &ev); err != nil {
		return err
	}
	if s.stock[ev.WH] == nil && (s.mode == Fixed || (s.mode == Narrow && ev.Op == "restock")) {
		s.stock[ev.WH] = map[string]int{}
	}
	switch ev.Op {
	case "restock", "return":
		s.stock[ev.WH][ev.SKU] += ev.Qty
	case "ship":
		if s.stock[ev.WH][ev.SKU] < ev.Qty {
			return reject(env, ev.ID, "insufficient stock")
		}
		s.stock[ev.WH][ev.SKU] -= ev.Qty
	default:
		return reject(env, ev.ID, "unknown op "+ev.Op)
	}
	return emit(env, "stock", map[string]any{"wh": ev.WH, "sku": ev.SKU, "on_hand": s.stock[ev.WH][ev.SKU]})
}

// 5. string-qty

type pricing struct{ mode Mode }

func (p *pricing) Handle(env kavach.Env, in kavach.Input) error {
	var ev map[string]any
	if err := decode(in, &ev); err != nil {
		return err
	}
	id, typ := fmt.Sprint(ev["id"]), fmt.Sprint(ev["type"])
	q := ev["qty"]
	if s, ok := q.(string); ok && (p.mode == Fixed || (p.mode == Narrow && typ == "order")) {
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return reject(env, id, "qty is not a number")
		}
		q = n
	}
	if p.mode == Fixed {
		_, qok := q.(float64)
		_, pok := ev["price"].(float64)
		if !qok || !pok {
			return reject(env, id, "qty and price must be numbers")
		}
	}
	qty := q.(float64)
	price := ev["price"].(float64)
	total := qty * price
	if typ == "return" {
		total = -total
	}
	return emit(env, "totals", map[string]any{"event": id, "cents": total})
}

// 6. group-overbook

const showCapacity = 5

type seats struct {
	mode  Mode
	taken map[string]int
}

func (s *seats) Handle(env kavach.Env, in kavach.Input) error {
	var ev struct {
		ID, Show, Kind string
		Seats          int
	}
	if err := decode(in, &ev); err != nil {
		return err
	}
	check := ev.Kind != "group" || s.mode == Fixed || (s.mode == Narrow && ev.Show == "show-b")
	if check && s.taken[ev.Show]+ev.Seats > showCapacity {
		return reject(env, ev.ID, "sold out")
	}
	s.taken[ev.Show] += ev.Seats
	return emit(env, "bookings", map[string]any{"show": ev.Show, "seats": ev.Seats, "left": showCapacity - s.taken[ev.Show]})
}

func (s *seats) Invariants() []kavach.Invariant {
	return []kavach.Invariant{{Name: "seats_within_capacity", Check: func() error {
		for show, n := range s.taken {
			if n > showCapacity {
				return fmt.Errorf("%s has %d of %d seats taken", show, n, showCapacity)
			}
		}
		return nil
	}}}
}

// 7. update-before-create

type customers struct {
	mode Mode
	plan map[string]string
}

func (c *customers) Handle(env kavach.Env, in kavach.Input) error {
	var ev struct{ ID, Op, Cust, Plan string }
	if err := decode(in, &ev); err != nil {
		return err
	}
	if _, ok := c.plan[ev.Cust]; !ok && ev.Op == "update" {
		if c.mode == Fixed || (c.mode == Narrow && ev.Plan == "pro") {
			ev.Op = "create" // upsert
		} else {
			return fmt.Errorf("update %s: unknown customer %q", ev.ID, ev.Cust)
		}
	}
	c.plan[ev.Cust] = ev.Plan
	return emit(env, "customers", map[string]string{"cust": ev.Cust, "plan": ev.Plan, "op": ev.Op})
}

// 8. redelivered-charge

type payments struct {
	mode    Mode
	applied map[string]int
	balance map[string]int64
}

func (p *payments) Handle(env kavach.Env, in kavach.Input) error {
	var ev struct {
		ID, Type, Account string
		Cents             int64
	}
	if err := decode(in, &ev); err != nil {
		return err
	}
	if p.applied[ev.ID] > 0 && (p.mode == Fixed || (p.mode == Narrow && ev.Type == "charge")) {
		return emit(env, "duplicates", map[string]string{"event": ev.ID})
	}
	p.applied[ev.ID]++
	if ev.Type == "refund" {
		p.balance[ev.Account] += ev.Cents
	} else {
		p.balance[ev.Account] -= ev.Cents
	}
	return emit(env, "ledger", map[string]any{"event": ev.ID, "account": ev.Account, "balance": p.balance[ev.Account]})
}

func (p *payments) Invariants() []kavach.Invariant {
	return []kavach.Invariant{{Name: "no_duplicate_application", Check: func() error {
		for id, n := range p.applied {
			if n > 1 {
				return fmt.Errorf("event %s applied %d times", id, n)
			}
		}
		return nil
	}}}
}

// 9. leap-day

type daily struct {
	mode        Mode
	hits, sales [365]int
}

func (d *daily) Handle(env kavach.Env, in kavach.Input) error {
	var ev struct {
		ID, Type string
		N        int
	}
	if err := decode(in, &ev); err != nil {
		return err
	}
	day := env.Now().YearDay() - 1
	if day >= len(d.hits) && (d.mode == Fixed || (d.mode == Narrow && ev.Type == "hit")) {
		return reject(env, ev.ID, "no bucket for day "+strconv.Itoa(day+1))
	}
	if ev.Type == "hit" {
		d.hits[day] += ev.N
		return emit(env, "daily", map[string]int{"day": day + 1, "hits": d.hits[day]})
	}
	d.sales[day] += ev.N
	return emit(env, "daily", map[string]int{"day": day + 1, "sales": d.sales[day]})
}

// 10. exhausted-pool

type lottery struct {
	mode  Mode
	pools map[string][]int
}

func (l *lottery) Handle(env kavach.Env, in kavach.Input) error {
	var ev struct{ ID, Pool string }
	if err := decode(in, &ev); err != nil {
		return err
	}
	pool, ok := l.pools[ev.Pool]
	if !ok && ev.Pool != "" {
		pool = []int{101, 102, 103}
	}
	var b [1]byte
	if _, err := env.Read(b[:]); err != nil {
		return err
	}
	if len(pool) == 0 && (l.mode == Fixed || (l.mode == Narrow && ev.Pool == "daily")) {
		return reject(env, ev.ID, "pool exhausted")
	}
	i := int(b[0]) % len(pool)
	ticket := pool[i]
	l.pools[ev.Pool] = append(pool[:i:i], pool[i+1:]...)
	return emit(env, "tickets", map[string]any{"event": ev.ID, "ticket": ticket})
}
