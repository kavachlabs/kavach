package harbor

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Task is a benchmark task with its arms.
type Task struct {
	Name        string    `json:"name"`                  // the base name: redis-10068
	Title       string    `json:"title"`                 // the first line of the instruction
	Source      string    `json:"source,omitempty"`      // upstream benchmark source from task.toml
	Track       string    `json:"track,omitempty"`       // "incident" or "feature"
	TimeoutSec  float64   `json:"timeout_sec,omitempty"` // default agent timeout in seconds
	Summary     string    `json:"summary,omitempty"`     // concise problem/goal excerpt
	Instruction string    `json:"instruction,omitempty"` // full markdown instruction body
	Arms        []TaskArm `json:"arms"`
}

// TaskArm is one Harbor task: one arm of a benchmark task.
type TaskArm struct {
	Arm         string `json:"arm"`                   // control, workflow, kavach, kavach-implement or ""
	Task        string `json:"task"`                  // the Harbor task's name: redis-10068-kavach
	Path        string `json:"path"`
	Description string `json:"description,omitempty"` // arm description from task.toml
}

// FindTasks lists the Harbor tasks (directories with a task.toml) directly
// under dir, grouped by base name. A missing dir has no tasks.
func FindTasks(dir string) ([]Task, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []Task{}, nil
	}
	if err != nil {
		return nil, err
	}
	tasks := []Task{}
	index := map[string]int{}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		tomlPath := filepath.Join(path, "task.toml")
		if !e.IsDir() || !exists(tomlPath) {
			continue
		}
		base, arm := SplitArm(e.Name())
		i, ok := index[base]
		if !ok {
			i = len(tasks)
			index[base] = i
			tasks = append(tasks, Task{Name: base})
		}
		t := &tasks[i]
		meta := readTaskTOML(tomlPath)
		if t.Source == "" && meta.source != "" {
			t.Source = meta.source
		}
		if t.TimeoutSec == 0 && meta.timeoutSec > 0 {
			t.TimeoutSec = meta.timeoutSec
		}
		mdPath := filepath.Join(path, "instruction.md")
		if t.Title == "" {
			t.Title = firstLine(mdPath)
		}
		// Prefer the control arm's instruction for the base task description.
		if t.Instruction == "" || arm == "control" {
			if body, summary := readInstruction(mdPath); body != "" {
				t.Instruction = body
				t.Summary = summary
			}
		}
		t.Arms = append(t.Arms, TaskArm{
			Arm:         arm,
			Task:        e.Name(),
			Path:        path,
			Description: meta.description,
		})
	}
	for i := range tasks {
		t := &tasks[i]
		sort.Slice(t.Arms, func(a, b int) bool { return CompareArms(t.Arms[a].Arm, t.Arms[b].Arm) })
		t.Track = "incident"
		for _, a := range t.Arms {
			if a.Arm == "workflow" || a.Arm == "kavach-implement" {
				t.Track = "feature"
				break
			}
		}
		if strings.Contains(strings.ToLower(t.Source), "deepswe") {
			t.Track = "feature"
		}
	}
	return tasks, nil
}

type taskTOMLMeta struct {
	source      string
	description string
	timeoutSec  float64
}

func readTaskTOML(path string) taskTOMLMeta {
	b, err := os.ReadFile(path)
	if err != nil {
		return taskTOMLMeta{}
	}
	var m taskTOMLMeta
	section := ""
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch {
		case section == "metadata" && k == "source":
			m.source = unquoteTOML(v)
		case section == "metadata" && k == "description":
			m.description = unquoteTOML(v)
		case section == "agent" && k == "timeout_sec":
			if f := parseFloat(v); f > 0 {
				m.timeoutSec = f
			}
		}
	}
	return m
}

func unquoteTOML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func parseFloat(s string) float64 {
	var f float64
	for i, c := range s {
		if (c < '0' || c > '9') && c != '.' {
			s = s[:i]
			break
		}
	}
	var whole, frac float64
	parts := strings.SplitN(s, ".", 2)
	for _, c := range parts[0] {
		if c >= '0' && c <= '9' {
			whole = whole*10 + float64(c-'0')
		}
	}
	if len(parts) == 2 {
		div := 1.0
		for _, c := range parts[1] {
			if c >= '0' && c <= '9' {
				div *= 10
				frac += float64(c-'0') / div
			}
		}
	}
	f = whole + frac
	return f
}

// readInstruction returns the markdown body after the first heading line and a
// concise summary paragraph for the task card.
func readInstruction(path string) (body, summary string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
	start := 0
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if t != "" {
			if strings.HasPrefix(t, "#") {
				start = i + 1
			}
			break
		}
	}
	body = strings.TrimSpace(strings.Join(lines[start:], "\n"))
	if body == "" {
		return "", ""
	}
	var paras []string
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		p := strings.TrimSpace(strings.Join(cur, " "))
		cur = nil
		if isBoilerplatePara(p) {
			return
		}
		paras = append(paras, p)
	}
	inFence := false
	for _, l := range lines[start:] {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			flush()
			continue
		}
		if inFence {
			continue
		}
		if isStopSection(t) {
			flush()
			if len(paras) > 0 {
				break
			}
			continue
		}
		if t == "" {
			flush()
			if len(paras) >= 2 {
				break
			}
			continue
		}
		if strings.HasPrefix(t, "#") {
			flush()
			continue
		}
		t = strings.TrimSpace(strings.TrimPrefix(t, ">"))
		if t == "" {
			flush()
			continue
		}
		if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") {
			flush()
			if len(paras) > 0 {
				break
			}
			continue
		}
		cur = append(cur, t)
	}
	flush()
	summary = strings.Join(paras, "\n\n")
	if len(summary) > 360 {
		cut := summary[:360]
		if idx := strings.LastIndexAny(cut, ". "); idx > 200 {
			cut = cut[:idx]
		}
		summary = strings.TrimRight(cut, " ,;:-") + "…"
	}
	return body, summary
}

func isBoilerplatePara(p string) bool {
	if p == "" || p == "Goal" || p == "Instruction" || p == "Report" || p == "Feature request" {
		return true
	}
	if strings.HasPrefix(p, "(") && strings.HasSuffix(p, ")") {
		return true
	}
	lower := strings.ToLower(p)
	if strings.HasPrefix(lower, "you are working in ") ||
		strings.HasPrefix(lower, "to check your work,") ||
		strings.HasPrefix(lower, "the tree is already built") ||
		strings.HasPrefix(lower, "redis is already built") {
		return true
	}
	return false
}

func isStopSection(line string) bool {
	lower := strings.ToLower(strings.TrimSpace(strings.TrimLeft(line, "#> ")))
	return strings.HasPrefix(lower, "testing") ||
		strings.HasPrefix(lower, "building") ||
		strings.HasPrefix(lower, "expected behavior") ||
		strings.HasPrefix(lower, "to reproduce") ||
		strings.HasPrefix(lower, "defaults:") ||
		strings.HasPrefix(lower, "behavior:")
}

// firstLine returns the first non-empty line of a Markdown file, without its
// heading marks or a "Task:" label.
func firstLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(strings.TrimLeft(sc.Text(), "#"))
		if l = strings.TrimSpace(strings.TrimPrefix(l, "Task:")); l != "" {
			return l
		}
	}
	return ""
}
