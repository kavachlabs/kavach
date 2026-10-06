package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/kavachlabs/kavach"
)

// Event is one wallet event from upstream.
type Event struct {
	ID      string `json:"id"`
	Type    string `json:"type"` // deposit, withdraw or transfer
	Account string `json:"account"`
	To      string `json:"to,omitempty"`
	Amount  *int64 `json:"amount"`
}

// Entry is one ledger line, emitted to the "ledger.entries" sink.
type Entry struct {
	Txn     string `json:"txn"`
	Event   string `json:"event"`
	Account string `json:"account"`
	Delta   int64  `json:"delta"`
	Balance int64  `json:"balance"`
	At      string `json:"at"`
}

// Rejection is emitted to the "ledger.rejections" sink.
type Rejection struct {
	Event  string `json:"event"`
	Reason string `json:"reason"`
}

// Ledger is a single-writer wallet ledger: it folds events into balances.
type Ledger struct {
	Balances map[string]int64 `json:"balances"`
	Net      int64            `json:"net"` // deposits minus withdrawals
}

func NewLedger() *Ledger { return &Ledger{Balances: map[string]int64{}} }

func (l *Ledger) Handle(env kavach.Env, in kavach.Input) error {
	var ev Event
	if err := json.Unmarshal(in.Data, &ev); err != nil {
		return fmt.Errorf("decode event at %s: %w", in.Position, err)
	}
	if fixNullAmount && ev.Amount == nil {
		return reject(env, ev, "missing amount")
	}
	amount := *ev.Amount
	if amount <= 0 {
		return reject(env, ev, "amount must be positive")
	}
	at := env.Now()

	switch ev.Type {
	case "deposit":
		l.Net += amount
		return l.post(env, ev, ev.Account, amount, at)
	case "withdraw":
		if l.Balances[ev.Account] < amount {
			return reject(env, ev, "insufficient funds")
		}
		l.Net -= amount
		return l.post(env, ev, ev.Account, -amount, at)
	case "transfer":
		if l.Balances[ev.Account] < amount {
			return reject(env, ev, "insufficient funds")
		}
		if err := l.post(env, ev, ev.Account, -amount, at); err != nil {
			return err
		}
		return l.post(env, ev, ev.To, amount, at)
	}
	return reject(env, ev, "unknown event type "+ev.Type)
}

func (l *Ledger) post(env kavach.Env, ev Event, account string, delta int64, at time.Time) error {
	l.Balances[account] += delta
	var txn [8]byte
	if _, err := env.Read(txn[:]); err != nil {
		return err
	}
	b, err := json.Marshal(Entry{
		Txn: hex.EncodeToString(txn[:]), Event: ev.ID, Account: account,
		Delta: delta, Balance: l.Balances[account], At: at.Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	env.Emit("ledger.entries", b)
	return nil
}

func reject(env kavach.Env, ev Event, reason string) error {
	b, err := json.Marshal(Rejection{Event: ev.ID, Reason: reason})
	if err != nil {
		return err
	}
	env.Emit("ledger.rejections", b)
	return nil
}

// Invariants are checked after every step, live and during replay.
func (l *Ledger) Invariants() []kavach.Invariant {
	return []kavach.Invariant{
		{Name: "balances_non_negative", Check: func() error {
			for _, a := range sortedAccounts(l.Balances) {
				if l.Balances[a] < 0 {
					return fmt.Errorf("account %s has balance %d", a, l.Balances[a])
				}
			}
			return nil
		}},
		{Name: "money_conserved", Check: func() error {
			var sum int64
			for _, b := range l.Balances {
				sum += b
			}
			if sum != l.Net {
				return fmt.Errorf("balances sum to %d, deposits minus withdrawals is %d", sum, l.Net)
			}
			return nil
		}},
	}
}

func (l *Ledger) Snapshot() ([]byte, error) { return json.Marshal(l) }
func (l *Ledger) Restore(b []byte) error    { return json.Unmarshal(b, l) }

func sortedAccounts(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
