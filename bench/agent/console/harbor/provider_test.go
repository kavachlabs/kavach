package harbor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartProvider(t *testing.T) {
	runner, _ := fakeHarbor(t, `exit 0`)
	clearKeys(t)
	tasks := []string{"hello-world-control"}

	if _, err := runner.Start(RunRequest{Tasks: tasks, Environment: "kubernetes"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown environment: %v", err)
	}
	_, err := runner.Start(RunRequest{Tasks: tasks, Environment: "modal"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "MODAL_TOKEN_ID and MODAL_TOKEN_SECRET") {
		t.Errorf("modal without keys: %v", err)
	}

	env := "MODAL_TOKEN_ID=ak-1\nexport MODAL_TOKEN_SECRET=\"as-2\"\nDAYTONA_API_KEY=\n"
	if err := os.WriteFile(runner.EnvFile, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Start(RunRequest{Tasks: tasks, Environment: "daytona"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("daytona with an empty key: %v", err)
	}
	t.Setenv("VERCEL_TOKEN", "from-process-env")
	for _, name := range []string{"modal", "vercel"} {
		id, err := runner.Start(RunRequest{Tasks: tasks, Environment: name})
		if err != nil {
			t.Fatal(name, err)
		}
		runner.Wait(id)
		var cfg struct {
			Environment map[string]any `json:"environment"`
		}
		if err := readJSON(runner.file(id, ".json"), &cfg); err != nil || cfg.Environment["type"] != name {
			t.Errorf("%s: %v %+v", name, err, cfg)
		}
	}
}

func clearKeys(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, p := range Providers {
		for _, k := range p.Keys() {
			t.Setenv(k, "")
		}
	}
	return home
}

func TestCredentialAlternatives(t *testing.T) {
	home := clearKeys(t)
	runner, _ := fakeHarbor(t, "")
	daytona, modal := Providers[0], Providers[1]

	os.WriteFile(runner.EnvFile, []byte("DAYTONA_JWT_TOKEN=j\n"), 0o600)
	if missing, _ := runner.Credentials(daytona); len(missing) != 1 || missing[0] != "DAYTONA_API_KEY" {
		t.Errorf("jwt without an organization: %v", missing)
	}
	os.WriteFile(runner.EnvFile, []byte("DAYTONA_JWT_TOKEN=j\nDAYTONA_ORGANIZATION_ID=o\n"), 0o600)
	if missing, _ := runner.Credentials(daytona); len(missing) != 0 {
		t.Errorf("jwt and organization: %v", missing)
	}

	if missing, via := runner.Credentials(modal); len(missing) != 2 || via != "" {
		t.Errorf("modal bare: %v %q", missing, via)
	}
	os.WriteFile(filepath.Join(home, ".modal.toml"), []byte("[default]\n"), 0o600)
	if missing, via := runner.Credentials(modal); len(missing) != 0 || via != "~/.modal.toml" {
		t.Errorf("modal.toml: %v %q", missing, via)
	}
	if !strings.Contains(modal.Need(), "MODAL_TOKEN_ID and MODAL_TOKEN_SECRET (or ~/.modal.toml)") {
		t.Error(modal.Need())
	}
}

func TestEmptyInFileOverridesEnvironment(t *testing.T) {
	clearKeys(t)
	runner, _ := fakeHarbor(t, "")
	t.Setenv("VERCEL_TOKEN", "from-process")
	if !runner.KeySet("VERCEL_TOKEN") {
		t.Fatal("process env not seen")
	}
	// Harbor loads the file with override, so the empty value wins.
	os.WriteFile(runner.EnvFile, []byte("VERCEL_TOKEN=\n"), 0o600)
	if runner.KeySet("VERCEL_TOKEN") {
		t.Error("empty value in the env file counted as set")
	}
}

func TestMissingExtra(t *testing.T) {
	clearKeys(t)
	runner, _ := fakeHarbor(t, `exit 0`)
	t.Setenv("DAYTONA_API_KEY", "k")
	runner.Extra = func(p Provider) error { return errors.New("no " + p.Name + " extra") }
	_, err := runner.Start(RunRequest{Tasks: []string{"hello-world-control"}, Environment: "daytona"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "no daytona extra") {
		t.Errorf("%v", err)
	}
}

func TestInstalledAsksHarborsPython(t *testing.T) {
	dir := t.TempDir()
	py := filepath.Join(dir, "python3")
	// Succeeds only for an import of "daytona", like an install with that extra.
	os.WriteFile(py, []byte("#!/bin/sh\n[ \"$3\" = \"import daytona\" ]\n"), 0o755)
	bin := filepath.Join(dir, "harbor")
	os.WriteFile(bin, []byte("#!"+py+"\nprint()\n"), 0o755)
	r := &Runner{Harbor: bin}
	if err := r.Installed(Providers[0]); err != nil {
		t.Errorf("daytona: %v", err)
	}
	err := r.Installed(Providers[1])
	if err == nil || !strings.Contains(err.Error(), "uv tool install --with 'harbor[modal]' harbor") {
		t.Errorf("modal: %v", err)
	}
}

// Harbor's factory passes environment.kwargs to ModalEnvironment, whose
// modal_vm_runtime switches off the host-network compose override.
func TestModalUsesVMRuntime(t *testing.T) {
	clearKeys(t)
	runner, _ := fakeHarbor(t, "")
	t.Setenv("MODAL_TOKEN_ID", "a")
	t.Setenv("MODAL_TOKEN_SECRET", "b")
	for env, want := range map[string]any{"modal": true, "vercel": nil, "daytona": nil} {
		t.Setenv("VERCEL_TOKEN", "v")
		t.Setenv("DAYTONA_API_KEY", "d")
		cfg, err := runner.jobConfig(&RunRequest{Tasks: []string{"hello-world-control"}, Name: "x", Environment: env})
		if err != nil {
			t.Fatal(env, err)
		}
		kwargs, _ := cfg["environment"].(map[string]any)["kwargs"].(map[string]any)
		if kwargs["modal_vm_runtime"] != want {
			t.Errorf("%s: kwargs %v", env, kwargs)
		}
	}
}

func TestDockerConfigUnchanged(t *testing.T) {
	runner, _ := fakeHarbor(t, "")
	for _, env := range []string{"", "docker"} {
		cfg, err := runner.jobConfig(&RunRequest{Tasks: []string{"hello-world-control"}, Name: "x", Environment: env})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := cfg["environment"]; ok {
			t.Errorf("environment %q wrote an environment section: %v", env, cfg)
		}
	}
}

func TestJobEnvironment(t *testing.T) {
	runner, _ := fakeHarbor(t, `exit 0`)
	t.Setenv("VERCEL_TOKEN", "x")
	id, err := runner.Start(RunRequest{Tasks: []string{"hello-world-control"}, Environment: "vercel"})
	if err != nil {
		t.Fatal(err)
	}
	runner.Wait(id)
	rd := &Reader{Dir: runner.JobsDir, Runs: runner}
	j, err := rd.Job(id)
	if err != nil || j.Environment != "vercel" {
		t.Fatalf("planned: %v %q", err, j.Environment)
	}
	// Harbor's own config.json wins once it exists.
	dir := filepath.Join(runner.JobsDir, id)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"environment":{"type":"modal"}}`), 0o644)
	if j, _ = rd.Job(id); j.Environment != "modal" {
		t.Errorf("config.json: %q", j.Environment)
	}
}
