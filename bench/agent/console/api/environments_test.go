package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

func clearProviderKeys(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, k := range []string{"DAYTONA_API_KEY", "MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET", "VERCEL_TOKEN"} {
		t.Setenv(k, "")
	}
}

func TestStatusEnvironments(t *testing.T) {
	clearProviderKeys(t)
	envPath := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(envPath, []byte("MODAL_TOKEN_ID=ak-secret-id\nVERCEL_TOKEN=vt-secret\n"), 0o600)
	s, ts := newServer(t, Config{EnvFile: envPath})
	s.runner.Extra = func(harbor.Provider) error { return nil }
	resp, err := http.Get(ts.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "secret") {
		t.Fatalf("status leaks a value: %s", raw)
	}
	var st Status
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	ready := map[string]bool{}
	for _, e := range st.Environments {
		ready[e.Name] = e.Ready
	}
	if len(st.Environments) != 3 || ready["daytona"] || ready["modal"] || !ready["vercel"] {
		t.Fatalf("%+v", st.Environments)
	}
	if m := st.Environments[1]; m.Name != "modal" || !m.Keys[0].Set || m.Keys[1].Set || m.Keys[1].Name != "MODAL_TOKEN_SECRET" {
		t.Fatalf("%+v", m)
	}
}

func TestStatusViaFileAndMissingExtra(t *testing.T) {
	clearProviderKeys(t)
	home, _ := os.UserHomeDir()
	os.WriteFile(filepath.Join(home, ".modal.toml"), []byte("[default]\n"), 0o600)
	s, ts := newServer(t, Config{EnvFile: filepath.Join(t.TempDir(), ".env")})
	s.runner.Extra = func(p harbor.Provider) error {
		if p.Name == "vercel" {
			return errors.New("lacks vercel support")
		}
		return nil
	}
	var st Status
	if code := get(t, ts.URL+"/api/status", &st); code != 200 {
		t.Fatal(code)
	}
	byName := map[string]EnvironmentStatus{}
	for _, e := range st.Environments {
		byName[e.Name] = e
	}
	if m := byName["modal"]; !m.Ready || m.Via != "~/.modal.toml" {
		t.Errorf("modal %+v", m)
	}
	if v := byName["vercel"]; v.Ready || v.Problem == "" {
		t.Errorf("vercel %+v", v)
	}
	if d := byName["daytona"]; d.Ready || !strings.Contains(d.Need, "DAYTONA_JWT_TOKEN and DAYTONA_ORGANIZATION_ID") {
		t.Errorf("daytona %+v", d)
	}
}

func TestProviderKeysMaskedToLastFour(t *testing.T) {
	if got := maskSecret("VERCEL_TOKEN", "vcl_abcdefghijklmnop1234"); got != "••••1234" {
		t.Error(got)
	}
	if got := maskSecret("OPENROUTER_API_KEY", "sk-or-v1-abcdefghijkl1234"); !strings.HasPrefix(got, "sk-or-v1") {
		t.Error(got)
	}
}

func TestStartRunNeedsProviderKeys(t *testing.T) {
	clearProviderKeys(t)
	_, ts := newServer(t, Config{EnvFile: filepath.Join(t.TempDir(), ".env")})
	code, v := post(t, ts.URL+"/api/jobs", "application/json", `{"tasks":["hello-world-control"],"environment":"daytona"}`, "Origin", ts.URL)
	if code != 400 || !strings.Contains(fmt.Sprint(v["error"]), "DAYTONA_API_KEY") {
		t.Fatalf("%d %v", code, v)
	}
}
