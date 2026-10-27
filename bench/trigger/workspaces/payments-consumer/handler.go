package main

import (
	"errors"
	"fmt"
	"time"
)

type Event struct {
	ID        string  `json:"id"`
	Type      string  `json:"type"` // deposit, withdraw, transfer, refund
	Account   string  `json:"account"`
	To        string  `json:"to,omitempty"`
	Amount    *int64  `json:"amount"` // minor units
	Currency  string  `json:"currency"`
	RefundFor string  `json:"refund_for,omitempty"`
}

type Store struct {
	balances map[string]int64
	seen     map[string]bool
	payments map[string]Event
	lastSeen map[string]time.Time
}

func NewStore() *Store {
	return &Store{
		balances: map[string]int64{},
		seen:     map[string]bool{},
		payments: map[string]Event{},
		lastSeen: map[string]time.Time{},
	}
}

var ErrInsufficient = errors.New("insufficient funds")

func (s *Store) Apply(ev Event) error {
	if s.seen[ev.ID] {
		return nil
	}
	s.lastSeen[ev.Account] = time.Now()
	switch ev.Type {
	case "deposit":
		s.balances[ev.Account] += *ev.Amount
	case "withdraw":
		if s.balances[ev.Account] < *ev.Amount {
			return ErrInsufficient
		}
		s.balances[ev.Account] -= *ev.Amount
	case "transfer":
		if s.balances[ev.Account] < *ev.Amount {
			return ErrInsufficient
		}
		s.balances[ev.Account] -= *ev.Amount
		s.balances[ev.To] += *ev.Amount
	case "refund":
		orig := s.payments[ev.RefundFor]
		s.balances[orig.Account] += *orig.Amount
	default:
		return fmt.Errorf("unknown event type %q", ev.Type)
	}
	s.payments[ev.ID] = ev
	s.seen[ev.ID] = true
	return nil
}
