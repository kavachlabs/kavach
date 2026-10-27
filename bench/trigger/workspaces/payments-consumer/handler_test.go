package main

import "testing"

func amt(n int64) *int64 { return &n }

func TestDeposit(t *testing.T) {
	s := NewStore()
	if err := s.Apply(Event{ID: "e1", Type: "deposit", Account: "a", Amount: amt(500)}); err != nil {
		t.Fatal(err)
	}
	if s.balances["a"] != 500 {
		t.Fatalf("balance = %d", s.balances["a"])
	}
}
