package tui

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

var update = flag.Bool("update", false, "rewrite the golden screens in testdata")

const fixtures = "../harbor/testdata"

// newTestModel serves the API over the core's fixtures, with redis-10068
// from the real tasks, and returns a console on it. POSTs to /api/jobs are
// recorded instead of starting Harbor.
func newTestModel(t *testing.T) (*Model, *[]harbor.RunRequest) {
	t.Helper()
	lipgloss.SetColorProfile(termenv.Ascii)
	// A copy keeps the files fresh: a running job whose folder has not
	// changed for 45 minutes counts as stopped.
	jobs := filepath.Join(t.TempDir(), "jobs")
	if err := os.CopyFS(jobs, os.DirFS(fixtures+"/jobs")); err != nil {
		t.Fatal(err)
	}
	tasks, _ := filepath.Abs("../../harbor/tasks")
	srv := api.New(api.Config{JobsDir: jobs, TasksDir: tasks, Harbor: "true", ModelsURL: modelsServer(t)})
	var posted []harbor.RunRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/jobs" {
			var req harbor.RunRequest
			json.NewDecoder(r.Body).Decode(&req)
			posted = append(posted, req)
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(harbor.Job{ID: "new-job", Status: harbor.JobRunning, Stoppable: true})
			return
		}
		srv.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := New(ctx, ts.URL, false, filepath.Join(t.TempDir(), "bench.json"))
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 34})
	drive(m, m.loadStatus(), m.loadTasks(), m.loadJobs())
	m.status.Docker = api.Check{OK: true, Version: "29.5.2"}
	m.status.Harbor = api.Check{OK: true, Version: "0.24.0"}
	m.status.JobsDir = "/jobs"
	return m, &posted
}

// modelsServer stands in for OpenRouter's model list.
func modelsServer(t *testing.T) string {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[
 {"id":"z-ai/glm-5.3","name":"Z.AI: GLM 5.3","context_length":202752,"pricing":{"prompt":"0.0000005","completion":"0.0000022"},"supported_parameters":["tools"]},
 {"id":"nvidia/nemotron-3.5-lightning","name":"NVIDIA: Nemotron 3.5 Lightning","context_length":262144,"pricing":{"prompt":"0.00000007","completion":"0.0000002"},"supported_parameters":["tools"]},
 {"id":"nvidia/nemotron-3.5-content-safety","name":"NVIDIA: Nemotron 3.5 Content Safety","context_length":131072,"pricing":{"prompt":"0.0000002","completion":"0.0000002"},"supported_parameters":[]}
]}`)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// drive runs cmds and every command their messages lead to, as Bubble Tea
// would, except the ticker and the event stream, which never end.
func drive(m *Model, cmds ...tea.Cmd) {
	for len(cmds) > 0 {
		cmd := cmds[0]
		cmds = cmds[1:]
		if cmd == nil {
			continue
		}
		switch msg := cmd().(type) {
		case nil, tickMsg:
		case tea.BatchMsg:
			cmds = append(cmds, msg...)
		default:
			_, next := m.Update(msg)
			cmds = append(cmds, next)
		}
	}
}

func typeLine(m *Model, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func press(m *Model, k tea.KeyType) {
	_, cmd := m.Update(tea.KeyMsg{Type: k})
	drive(m, cmd)
}

// screen renders m without trailing spaces, for golden files.
func screen(m *Model) string {
	var b strings.Builder
	for _, l := range strings.Split(m.View(), "\n") {
		b.WriteString(strings.TrimRight(l, " ") + "\n")
	}
	return b.String()
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".txt")
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from %s (go test -update rewrites it):\n%s", name, path, got)
	}
}

func TestStartScreen(t *testing.T) {
	m, _ := newTestModel(t)
	if m.set.Task != "redis-10068" {
		t.Fatalf("task %q, want the first task", m.set.Task)
	}
	golden(t, "start", screen(m))
}

func TestSlashMenu(t *testing.T) {
	m, _ := newTestModel(t)
	typeLine(m, "/mo")
	var got []string
	for _, s := range m.suggestions() {
		got = append(got, s.value)
	}
	if strings.Join(got, " ") != "/model /models" {
		t.Errorf("suggestions for /mo: %v", got)
	}
	golden(t, "slash-menu", screen(m))

	press(m, tea.KeyDown)
	press(m, tea.KeyTab)
	if v := m.input.Value(); v != "/models" {
		t.Errorf("tab completed to %q", v)
	}
	press(m, tea.KeyEsc)
	if m.input.Value() != "" || m.suggestions() != nil {
		t.Errorf("esc left %q", m.input.Value())
	}
}

func TestArgumentSuggestions(t *testing.T) {
	m, _ := newTestModel(t)
	typeLine(m, "/arm ")
	if s := m.suggestions(); len(s) != 3 || s[2].value != "/arm both" {
		t.Errorf("/arm suggestions: %+v", s)
	}
	press(m, tea.KeyDown)
	press(m, tea.KeyDown)
	press(m, tea.KeyEnter)
	if m.set.Arm != "both" {
		t.Errorf("arm %q after picking both", m.set.Arm)
	}
}

func TestSettingsPersist(t *testing.T) {
	m, _ := newTestModel(t)
	for _, line := range []string{"/arm control", "/runs 3", "/timeout 45", "/agent oracle"} {
		drive(m, m.exec(line))
		if m.note != "" {
			t.Fatalf("%s: %s", line, m.note)
		}
	}
	drive(m, m.exec("/runs 0"))
	if !strings.Contains(m.note, "1 to 100") {
		t.Errorf("/runs 0: note %q", m.note)
	}
	drive(m, m.exec("/task nope"))
	if !strings.Contains(m.note, "redis-10068") {
		t.Errorf("/task nope should list the tasks: %q", m.note)
	}
	got := loadSettings(m.setPath)
	want := settings{Agent: "oracle", Task: "redis-10068", Arm: "control", Model: "z-ai/glm-5.3", Name: "GLM 5.3", Runs: 3, Timeout: 45}
	if got.Agent != want.Agent || got.Task != want.Task || got.Arm != want.Arm || got.Runs != want.Runs || got.Timeout != want.Timeout {
		t.Errorf("saved %+v, want %+v", got, want)
	}
}

func TestModelCommand(t *testing.T) {
	m, _ := newTestModel(t)
	drive(m, m.exec("/model openrouter/nvidia/nemotron-3.5-lightning"))
	if m.set.Model != "nvidia/nemotron-3.5-lightning" || m.set.Name != "Nemotron 3.5 Lightning" {
		t.Errorf("model %q named %q", m.set.Model, m.set.Name)
	}
	drive(m, m.exec("/model nvidia/nemotron-3.5-content-safety"))
	if !strings.Contains(m.note, "tool calls") {
		t.Errorf("a model without tools should warn: %q", m.note)
	}
	drive(m, m.exec("/models nemotron"))
	golden(t, "models", screen(m))
}

func TestRunRequests(t *testing.T) {
	m, posted := newTestModel(t)
	drive(m, m.exec("/runs 3"), m.exec("/arm both"), m.exec("/timeout 30"), m.exec("/run"))
	drive(m, m.exec("/agent oracle"), m.exec("/arm kavach"), m.exec("/run"))
	drive(m, m.exec("/agent opencode"), m.exec("/matrix z-ai/glm-5.3 nvidia/nemotron-3.5-lightning"))
	if len(*posted) != 3 {
		t.Fatalf("%d runs started, want 3", len(*posted))
	}
	want := []harbor.RunRequest{
		{Tasks: []string{"redis-10068-control", "redis-10068-kavach"}, Models: []string{"openrouter/z-ai/glm-5.3"}, Attempts: 3, Agent: "opencode", TimeoutSec: 1800},
		{Tasks: []string{"redis-10068-kavach"}, Attempts: 3, Agent: "oracle", TimeoutSec: 1800},
		{Tasks: []string{"redis-10068-control", "redis-10068-kavach"}, Models: []string{"openrouter/z-ai/glm-5.3", "openrouter/nvidia/nemotron-3.5-lightning"}, Attempts: 3, Agent: "opencode", TimeoutSec: 1800},
	}
	for i, w := range want {
		g, _ := json.Marshal((*posted)[i])
		e, _ := json.Marshal(w)
		if string(g) != string(e) {
			t.Errorf("run %d:\n got %s\nwant %s", i, g, e)
		}
	}
	if m.page != pageRuns {
		t.Errorf("after /matrix the page is %d, want the runs view", m.page)
	}
}

func TestRunWithoutDocker(t *testing.T) {
	m, posted := newTestModel(t)
	m.status.Docker = api.Check{Error: "Cannot connect to the Docker daemon"}
	drive(m, m.exec("/run"))
	if len(*posted) != 0 || !strings.Contains(m.note, "colima start") {
		t.Errorf("posted %d runs, note %q", len(*posted), m.note)
	}
}

// openRuns opens the fixture job with a running, a passed, an errored and
// a queued attempt.
func openRuns(t *testing.T) *Model {
	m, _ := newTestModel(t)
	drive(m, m.openJob("opencode-glm"))
	sel := m.selected()
	if sel == nil || sel.Status != harbor.Running {
		t.Fatalf("selected %+v, want the running attempt", sel)
	}
	started := *sel.Started
	m.now = func() time.Time { return started.Add(134 * time.Second) }
	return m
}

func TestRunsView(t *testing.T) {
	m := openRuns(t)
	golden(t, "runs", screen(m))

	press(m, tea.KeyDown)
	if sel := m.selected(); sel.Status != harbor.Pass {
		t.Fatalf("down selected %s", sel.Status)
	}
	golden(t, "runs-pass", screen(m))

	press(m, tea.KeyEsc)
	if !strings.Contains(m.note, "finished") {
		t.Errorf("esc on a finished attempt: %q", m.note)
	}
	press(m, tea.KeyUp)
	press(m, tea.KeyEsc)
	if !strings.Contains(m.note, "another bench process") {
		t.Errorf("esc on a job this server didn't start: %q", m.note)
	}
}

func TestStopConfirm(t *testing.T) {
	m := openRuns(t)
	m.job.Stoppable = true
	press(m, tea.KeyEsc)
	if m.confirm == nil || !strings.Contains(m.confirm.text, "1 running attempt and 1 queued one") {
		t.Fatalf("confirm %+v", m.confirm)
	}
	press(m, tea.KeyEsc)
	if m.confirm != nil {
		t.Error("esc should dismiss the question")
	}
}

func TestQuitAsksAboutRuns(t *testing.T) {
	m, _ := newTestModel(t)
	m.owned = true
	m.jobs = append(m.jobs, harbor.Job{ID: "mine", Status: harbor.JobRunning, Stoppable: true})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil || m.confirm == nil {
		t.Fatal("ctrl+c should ask before leaving runs without a server")
	}
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	if !m.keep || cmd == nil {
		t.Error("k should quit and keep the server")
	}
}

func TestResultsAndCompare(t *testing.T) {
	m := openRuns(t)
	drive(m, m.exec("/results"))
	golden(t, "results", screen(m))
	drive(m, m.exec("/compare"))
	golden(t, "compare", screen(m))
}

func TestGradeCard(t *testing.T) {
	m, _ := newTestModel(t)
	drive(m, m.openJob("redis-oracle"))
	drive(m, m.exec("/results"))
	out := screen(m)
	if !strings.Contains(out, "PASS") || !strings.Contains(out, "tests passed") {
		t.Errorf("the oracle's grade card:\n%s", out)
	}
}

func TestOpenCommand(t *testing.T) {
	m := openRuns(t)
	byStatus := map[string]harbor.Trial{}
	for _, tr := range m.job.Trials {
		byStatus[tr.Status] = tr
	}
	cmd, _ := openCommand(byStatus[harbor.Running], "/repo")
	if cmd == nil || strings.Join(cmd.Args, " ") != "opencode attach http://127.0.0.1:41240 --session ses_running0001" {
		t.Errorf("running: %v", cmd)
	}
	cmd, _ = openCommand(byStatus[harbor.Pass], "/repo")
	if cmd == nil || len(cmd.Args) != 2 || cmd.Args[0] != "/repo/bench/agent/harbor/show.sh" || !strings.HasSuffix(cmd.Args[1], "__Pass001") {
		t.Errorf("finished: %v", cmd)
	}
	if cmd, why := openCommand(byStatus[harbor.Pass], ""); cmd != nil || why == "" {
		t.Errorf("finished, no root: %v %q", cmd, why)
	}
	for _, s := range []string{harbor.Error, harbor.Queued} {
		if cmd, why := openCommand(byStatus[s], "/repo"); cmd != nil || why == "" {
			t.Errorf("%s: %v %q", s, cmd, why)
		}
	}
}

func TestStepLines(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	exit := 2
	for _, c := range []struct {
		step harbor.Step
		want string
	}{
		{harbor.Step{Kind: "read", Title: "src/t_stream.c lines 780–840"}, "→ Read src/t_stream.c lines 780–840"},
		{harbor.Step{Kind: "search", Title: "ms_delta", Detail: "Found 4 matches\n…"}, `* Grep "ms_delta" (4 matches)`},
		{harbor.Step{Kind: "shell", Title: "make", Exit: &exit}, "$ make  exit 2"},
		{harbor.Step{Kind: "mcp", Title: "kavach diff ./src/redis-server", Kavach: true, Verdict: "fixed"}, "⚙ kavach diff ./src/redis-server  fixed"},
		{harbor.Step{Kind: "text", Title: "I suspect\nthe deltas"}, "I suspect the deltas"},
		{harbor.Step{Kind: "usage", Title: "step 3"}, ""},
	} {
		if got := strings.TrimRight(stepLine(c.step, 80), " "); got != c.want {
			t.Errorf("%s: %q, want %q", c.step.Kind, got, c.want)
		}
	}
}

func TestFormats(t *testing.T) {
	for _, c := range [][2]string{
		{tokens(9100), "9.1k"}, {tokens(170956), "171k"}, {tokens(1831936), "1.83M"}, {tokens(950), "950"},
		{duration(249), "4m 09s"}, {duration(41), "41s"}, {duration(3720), "1h 02m"},
		{clock(134), "2:14"}, {dollars(0.082), "$0.08"}, {dollars(0.002), "<$0.01"},
	} {
		if c[0] != c[1] {
			t.Errorf("%q, want %q", c[0], c[1])
		}
	}
}
