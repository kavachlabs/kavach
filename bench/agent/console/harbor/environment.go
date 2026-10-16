package harbor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Environment is what a Harbor task runs in: the compose files that start
// the agent's and the verifier's containers, the Dockerfile their images are
// built from, and
// the settings from task.toml that shape the agent's container.
type Environment struct {
	Task            string    `json:"task"`                        // the Harbor task: redis-10068-kavach
	Network         string    `json:"network,omitempty"`           // [agent] network_mode
	Workdir         string    `json:"workdir,omitempty"`           // [environment] workdir
	BuildTimeoutSec float64   `json:"build_timeout_sec,omitempty"` // [environment] build_timeout_sec
	VerifierMode    string    `json:"verifier_mode,omitempty"`     // [verifier] environment_mode
	MCPServers      []string  `json:"mcp_servers,omitempty"`       // [[environment.mcp_servers]] names
	Files           []EnvFile `json:"files"`

	images map[string]string // role -> docker_image from task.toml
}

// EnvFile is one file of a task's environment.
type EnvFile struct {
	Role       string `json:"role"`                 // agent, verifier, dockerfile or task
	Path       string `json:"path"`                 // relative to the task folder, or absolute
	Content    string `json:"content"`              // truncated past maxEnvFile bytes
	Truncated  bool   `json:"truncated,omitempty"`  // Content is cut short
	Image      string `json:"image,omitempty"`      // compose files: the prebuilt image task.toml names
	Dockerfile string `json:"dockerfile,omitempty"` // compose files: the Dockerfile the image is built from, absolute
	Target     string `json:"target,omitempty"`     // compose files: the image's build target
	Missing    bool   `json:"missing,omitempty"`    // the file does not exist
}

const maxEnvFile = 256 << 10

// ErrNoTask is returned for a name that is not a task under the tasks dir.
var ErrNoTask = fmt.Errorf("no such task")

// TaskEnvironment reads the environment of the Harbor task name under dir.
// The compose files build nothing: the agent's and the verifier's images are
// the docker_image refs in task.toml (ghcr.io/…/bench-<task>:<target>-<key>),
// and their Dockerfile is bench/agent/tasks/<task>/Dockerfile, found from dir.
func TaskEnvironment(dir, name string) (*Environment, error) {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return nil, ErrNoTask
	}
	taskDir := filepath.Join(dir, name)
	if !exists(filepath.Join(taskDir, "task.toml")) {
		return nil, ErrNoTask
	}
	env := &Environment{Task: name, Files: []EnvFile{}}
	toml := readEnvFile("task", filepath.Join(taskDir, "task.toml"))
	toml.Path = "task.toml"
	readEnvironmentTOML(toml.Content, env)
	seen := map[string]bool{}
	var dockerfiles []string
	for _, c := range []struct{ role, rel string }{
		{"agent", "environment/docker-compose.yaml"},
		{"verifier", "tests/docker-compose.yaml"},
	} {
		f := readEnvFile(c.role, filepath.Join(taskDir, c.rel))
		f.Path = c.rel
		if base, target, ok := splitImage(env.images[c.role]); ok {
			f.Image, f.Target = env.images[c.role], target
			f.Dockerfile = filepath.Join(dir, "..", "..", "tasks", base, "Dockerfile")
			if !seen[f.Dockerfile] {
				seen[f.Dockerfile] = true
				dockerfiles = append(dockerfiles, f.Dockerfile)
			}
		}
		env.Files = append(env.Files, f)
	}
	for _, p := range dockerfiles {
		env.Files = append(env.Files, readEnvFile("dockerfile", p))
	}
	env.Files = append(env.Files, toml)
	return env, nil
}

// splitImage takes ghcr.io/kavachlabs/bench-<task>:<target>-<key> apart into
// the task and the Dockerfile target (image-tag.py makes these refs).
func splitImage(ref string) (task, target string, ok bool) {
	repo, tag, found := strings.Cut(ref, ":")
	if !found {
		return
	}
	task, found = strings.CutPrefix(repo[strings.LastIndex(repo, "/")+1:], "bench-")
	n := strings.LastIndex(tag, "-")
	if !found || task == "" || n <= 0 {
		return "", "", false
	}
	return task, tag[:n], true
}

func readEnvFile(role, path string) EnvFile {
	f := EnvFile{Role: role, Path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		f.Missing = true
		return f
	}
	if len(b) > maxEnvFile {
		b, f.Truncated = b[:maxEnvFile], true
	}
	f.Content = string(b)
	return f
}

func readEnvironmentTOML(content string, env *Environment) {
	section := ""
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[] ")
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case section == "agent" && k == "network_mode":
			env.Network = unquoteTOML(v)
		case section == "environment" && k == "workdir":
			env.Workdir = unquoteTOML(v)
		case section == "environment" && k == "docker_image":
			env.setImage("agent", unquoteTOML(v))
		case section == "verifier.environment" && k == "docker_image":
			env.setImage("verifier", unquoteTOML(v))
		case section == "environment" && k == "build_timeout_sec":
			env.BuildTimeoutSec = parseFloat(v)
		case section == "verifier" && k == "environment_mode":
			env.VerifierMode = unquoteTOML(v)
		case section == "environment.mcp_servers" && k == "name":
			env.MCPServers = append(env.MCPServers, unquoteTOML(v))
		}
	}
}

func (e *Environment) setImage(role, ref string) {
	if e.images == nil {
		e.images = map[string]string{}
	}
	e.images[role] = ref
}
