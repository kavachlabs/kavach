package kavach_test

import (
	"strings"
	"testing"

	"github.com/kavachlabs/kavach"
	"github.com/kavachlabs/kavach/journal"
)

// Gateway and config replay are not implemented: such journals must fail to
// run, not replay wrongly. Environment records are skipped.
func TestReplayRefusesGatewayAndConfig(t *testing.T) {
	env := journal.Record{Type: journal.TypeEnvironment, Flags: journal.FlagCritical, Seq: 0}
	in := journal.Record{Type: journal.TypeInput, Seq: 1, Source: "s", Data: []byte("alice:1")}
	meta := journal.Meta{Service: "w", Start: journal.StartGenesis, Compression: journal.CompressionZstd}
	mk := func(recs ...journal.Record) *journal.Journal {
		return &journal.Journal{Header: journal.Header{Major: journal.Major, Minor: journal.Minor, Meta: meta}, Records: recs}
	}

	if _, err := kavach.Replay(mk(env, in), newWallet); err != nil {
		t.Fatalf("environment record: %v", err)
	}
	for _, extra := range []journal.Record{
		{Type: journal.TypeGateway, Flags: journal.FlagCritical, Seq: 2, Gateway: "g"},
		{Type: journal.TypeConfig, Flags: journal.FlagCritical, Seq: 2, Key: "k"},
	} {
		_, err := kavach.Replay(mk(env, in, extra), newWallet)
		if err == nil || !strings.Contains(err.Error(), "not supported yet") {
			t.Errorf("%s: err = %v", extra.Type, err)
		}
	}
}
