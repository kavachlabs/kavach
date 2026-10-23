package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
)

func TestTrialRetryOverSocket(t *testing.T) {
	// Unix socket paths are short; t.TempDir's are too long on macOS.
	dir, err := os.MkdirTemp("", "adm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "admin.sock")
	os.WriteFile(sock, nil, 0o644)
	if _, err := listenAdmin(sock); err == nil {
		t.Fatal("listenAdmin replaced a file that is not a socket")
	}
	os.Remove(sock)
	ln, err := listenAdmin(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if st, _ := os.Stat(sock); st.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v", st.Mode().Perm())
	}
	var got api.AdminRetry
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		if got.Job == "own" {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":"not an invite run"}`))
			return
		}
		json.NewEncoder(w).Encode([]api.AdminRetried{{Trial: "t__a"}, {Trial: "t__b", Queued: true}, {Trial: "c", Error: "no such attempt"}})
	}))

	var out, errb bytes.Buffer
	code := cmdTrial([]string{"retry", "-socket", sock, "run-1", "a", "b", "c"}, &out, &errb)
	if code != 1 || got.Job != "run-1" || strings.Join(got.Trials, ",") != "a,b,c" {
		t.Errorf("code %d, sent %+v, stderr %s", code, got, errb.String())
	}
	for _, want := range []string{"t__a  started", "t__b  queued", "c  failed: no such attempt"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	errb.Reset()
	if code := cmdTrial([]string{"retry", "-socket", sock, "own", "a"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "not an invite run") {
		t.Errorf("refusal: %d %q", code, errb.String())
	}
	if code := cmdTrial([]string{"retry", "run-1"}, &out, &errb); code != 2 {
		t.Errorf("no attempt: %d", code)
	}
}
