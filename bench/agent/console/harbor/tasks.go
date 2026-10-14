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
	Name  string    `json:"name"`  // the base name: redis-10068
	Title string    `json:"title"` // the first line of the instruction
	Arms  []TaskArm `json:"arms"`
}

// TaskArm is one Harbor task: one arm of a benchmark task.
type TaskArm struct {
	Arm  string `json:"arm"`  // control, workflow, kavach, kavach-implement or ""
	Task string `json:"task"` // the Harbor task's name: redis-10068-kavach
	Path string `json:"path"`
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
		if !e.IsDir() || !exists(filepath.Join(path, "task.toml")) {
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
		if t.Title == "" {
			t.Title = firstLine(filepath.Join(path, "instruction.md"))
		}
		t.Arms = append(t.Arms, TaskArm{Arm: arm, Task: e.Name(), Path: path})
	}
	for _, t := range tasks {
		sort.Slice(t.Arms, func(i, j int) bool { return CompareArms(t.Arms[i].Arm, t.Arms[j].Arm) })
	}
	return tasks, nil
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
