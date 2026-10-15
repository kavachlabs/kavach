package harbor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Grade is the verifier's grade.json with the tests its output lists.
type Grade struct {
	Grade  json.RawMessage `json:"grade"`
	Passed int             `json:"passed"`
	Failed int             `json:"failed"`
	Tests  []TestResult    `json:"tests"`
	// AgentDiff is the verdict of the last kavach diff the agent ran itself,
	// in its container. When it differs from the verifier's, one of the two
	// builds is not the code the other checked.
	AgentDiff string `json:"agent_diff,omitempty"`
}

// TestResult is one test the verifier ran.
type TestResult struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
}

// Patch returns the agent's change, as the task's collect hook saved it.
func (r *Reader) Patch(job, id string) ([]byte, error) {
	dir, err := r.trialDir(job, id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "artifacts", "logs", "artifacts", "patch.diff"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

// Grade returns a trial's grade.json and the tests listed in the verifier's
// output (grade.log, test-stdout.txt). Tests are found best-effort: Redis's
// test runner ("[ok]: name (1 ms)", "[err]: name in file") and pytest's -rA
// summary ("PASSED file::test", "FAILED file::test - why").
func (r *Reader) Grade(job, id string) (Grade, error) {
	dir, err := r.trialDir(job, id)
	if err != nil {
		return Grade{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "verifier", "grade.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Grade{}, ErrNotFound
	}
	if err != nil {
		return Grade{}, err
	}
	if !json.Valid(b) {
		return Grade{}, fmt.Errorf("grade.json is not JSON")
	}
	g := Grade{Grade: b, Tests: []TestResult{}}
	index := map[string]int{}
	for _, name := range []string{"grade.log", "test-stdout.txt"} {
		for _, t := range listedTests(filepath.Join(dir, "verifier", name)) {
			if i, ok := index[t.Name]; ok {
				g.Tests[i] = t
				continue
			}
			index[t.Name] = len(g.Tests)
			g.Tests = append(g.Tests, t)
		}
	}
	for _, t := range g.Tests {
		if t.OK {
			g.Passed++
		} else {
			g.Failed++
		}
	}
	if steps, err := readSteps(dir); err == nil {
		g.AgentDiff = agentDiff(steps)
	}
	return g, nil
}

var (
	tclTest  = regexp.MustCompile(`^\[(ok|err|exception)\]: (.+?)(?: \(\d+ ms\)| in \S+)?$`)
	pytestRe = regexp.MustCompile(`^(PASSED|FAILED|ERROR) (\S+)`)
)

func listedTests(path string) []TestResult {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var tests []TestResult
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if m := tclTest.FindStringSubmatch(line); m != nil {
			tests = append(tests, TestResult{Name: m[2], OK: m[1] == "ok"})
		} else if m := pytestRe.FindStringSubmatch(line); m != nil {
			tests = append(tests, TestResult{Name: m[2], OK: m[1] == "PASSED"})
		}
	}
	return tests
}

func (r *Reader) trialDir(job, id string) (string, error) {
	if !validName(job) || !validName(id) {
		return "", ErrNotFound
	}
	dir := filepath.Join(r.Dir, job, id)
	if !isTrialDir(dir) {
		return "", ErrNotFound
	}
	return dir, nil
}
