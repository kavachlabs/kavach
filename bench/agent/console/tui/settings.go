package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// settings are what /run will do. They persist between launches.
type settings struct {
	Agent   string   `json:"agent"`   // a Harbor agent: opencode, oracle, nop
	Task    string   `json:"task"`    // a task's base name: redis-10068
	Arm     string   `json:"arm"`     // control, workflow, kavach, kavach-implement or both
	Model   string   `json:"model"`   // an OpenRouter id: z-ai/glm-5.3
	Name    string   `json:"name"`    // the model's display name: GLM 5.3
	Runs    int      `json:"runs"`    // attempts per task and model
	Timeout int      `json:"timeout"` // agent timeout in minutes; 0 keeps the task's
	Retries int      `json:"retries"` // times Harbor runs an attempt again after an error
	Matrix  []string `json:"matrix"`  // OpenRouter ids last picked for /matrix
	Env     string   `json:"env"`     // where attempts run: docker (or empty), daytona, modal, vercel
}

func defaultSettings() settings {
	return settings{Agent: "opencode", Arm: "kavach", Model: "z-ai/glm-5.3", Name: "GLM 5.3", Runs: 1, Retries: 1}
}

// settingsPath is <user config dir>/kavach/bench.json.
func settingsPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "kavach", "bench.json")
}

// loadSettings reads path over the defaults; a missing or broken file
// gives the defaults.
func loadSettings(path string) settings {
	s := defaultSettings()
	if path == "" {
		return s
	}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &s)
	}
	if s.Runs < 1 {
		s.Runs = 1
	}
	return s
}

func (s settings) save(path string) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
