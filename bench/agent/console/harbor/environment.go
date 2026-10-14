package harbor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Environment is what a Harbor task runs in: the compose files that start
// the agent's and the verifier's containers, the Dockerfile they build, and
// the settings from task.toml that shape the agent's container.
type Environment struct {
	Task            string    `json:"task"`                        // the Harbor task: redis-10068-kavach
	Network         string    `json:"network,omitempty"`           // [agent] network_mode
	Workdir         string    `json:"workdir,omitempty"`           // [environment] workdir
	BuildTimeoutSec float64   `json:"build_timeout_sec,omitempty"` // [environment] build_timeout_sec
	VerifierMode    string    `json:"verifier_mode,omitempty"`     // [verifier] environment_mode
	MCPServers      []string  `json:"mcp_servers,omitempty"`       // [[environment.mcp_servers]] names
	Files           []EnvFile `json:"files"`
}

// EnvFile is one file of a task's environment.
type EnvFile struct {
	Role       string `json:"role"`                 // agent, verifier, dockerfile or task
	Path       string `json:"path"`                 // relative to the task folder, or absolute
	Content    string `json:"content"`              // truncated past maxEnvFile bytes
	Truncated  bool   `json:"truncated,omitempty"`  // Content is cut short
	Dockerfile string `json:"dockerfile,omitempty"` // compose files: the Dockerfile built, absolute
	Target     string `json:"target,omitempty"`     // compose files: the build target
	Missing    bool   `json:"missing,omitempty"`    // the file does not exist
}

const maxEnvFile = 256 << 10

// ErrNoTask is returned for a name that is not a task under the tasks dir.
var ErrNoTask = fmt.Errorf("no such task")

// TaskEnvironment reads the environment of the Harbor task name under dir.
// Variables in the compose files expand from this process's environment, as
// they do for the runs it starts.
func TaskEnvironment(dir, name string) (*Environment, error) {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return nil, ErrNoTask
	}
	taskDir := filepath.Join(dir, name)
	if !exists(filepath.Join(taskDir, "task.toml")) {
		return nil, ErrNoTask
	}
	env := &Environment{Task: name, Files: []EnvFile{}}
	seen := map[string]bool{}
	var dockerfiles []string
	for _, c := range []struct{ role, rel string }{
		{"agent", "environment/docker-compose.yaml"},
		{"verifier", "tests/docker-compose.yaml"},
	} {
		f := readEnvFile(c.role, filepath.Join(taskDir, c.rel))
		f.Path = c.rel
		if !f.Missing {
			context, dockerfile, target := composeBuild(f.Content)
			f.Target = target
			if dockerfile != "" {
				base := filepath.Join(taskDir, filepath.Dir(c.rel))
				ctx := expandEnv(context)
				if ctx == "" {
					ctx = "."
				}
				if !filepath.IsAbs(ctx) {
					ctx = filepath.Join(base, ctx)
				}
				f.Dockerfile = filepath.Clean(filepath.Join(ctx, dockerfile))
				if !seen[f.Dockerfile] {
					seen[f.Dockerfile] = true
					dockerfiles = append(dockerfiles, f.Dockerfile)
				}
			}
		}
		env.Files = append(env.Files, f)
	}
	for _, p := range dockerfiles {
		env.Files = append(env.Files, readEnvFile("dockerfile", p))
	}
	toml := readEnvFile("task", filepath.Join(taskDir, "task.toml"))
	toml.Path = "task.toml"
	env.Files = append(env.Files, toml)
	readEnvironmentTOML(toml.Content, env)
	return env, nil
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

// composeBuild returns the first service's build context, dockerfile and
// target. The task compose files are small and regular, so this reads lines
// rather than parsing YAML.
func composeBuild(content string) (context, dockerfile, target string) {
	inBuild, buildIndent := false, 0
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if inBuild && indent <= buildIndent {
			if dockerfile != "" || context != "" {
				return
			}
			inBuild = false
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		v = unquoteTOML(strings.TrimSpace(v))
		switch {
		case k == "build" && v == "":
			inBuild, buildIndent = true, indent
		case k == "build":
			return v, "Dockerfile", ""
		case inBuild && k == "context":
			context = v
		case inBuild && k == "dockerfile":
			dockerfile = v
		case inBuild && k == "target":
			target = v
		}
	}
	if context != "" && dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	return
}

// expandEnv expands ${VAR}, ${VAR:-default} and $VAR as Compose does.
func expandEnv(s string) string {
	lookup := os.LookupEnv
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		if s[i+1] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				b.WriteString(s[i:])
				break
			}
			expr := s[i+2 : i+end]
			name, def, hasDef := strings.Cut(expr, ":-")
			if v, ok := lookup(name); ok && (v != "" || !hasDef) {
				b.WriteString(v)
			} else {
				b.WriteString(def)
			}
			i += end
			continue
		}
		j := i + 1
		for j < len(s) && (s[j] == '_' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= '0' && s[j] <= '9') {
			j++
		}
		v, _ := lookup(s[i+1 : j])
		b.WriteString(v)
		i = j - 1
	}
	return b.String()
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
		case section == "environment" && k == "build_timeout_sec":
			env.BuildTimeoutSec = parseFloat(v)
		case section == "verifier" && k == "environment_mode":
			env.VerifierMode = unquoteTOML(v)
		case section == "environment.mcp_servers" && k == "name":
			env.MCPServers = append(env.MCPServers, unquoteTOML(v))
		}
	}
}
