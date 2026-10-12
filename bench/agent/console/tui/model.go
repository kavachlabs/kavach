// Package tui is bench's terminal console: OpenCode's look, slash commands,
// and a live view of Harbor runs. It talks only to the console's HTTP API
// (API.md), so it and the web console always show the same runs.
package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// page is what fills the screen between the header and the prompt.
type page int

const (
	pageHome page = iota
	pageRuns
	pageResults
	pageCompare
	pageModels
	pageHelp
	pageJobs
	pageMatrix
)

// listPage reports whether p has a list the arrow keys move through while
// the prompt is closed.
func (p page) listPage() bool { return p == pageRuns || p == pageJobs || p == pageMatrix }

// Model is the console's Bubble Tea model.
type Model struct {
	c       *client
	ctx     context.Context
	owned   bool // this process serves the API
	set     settings
	setPath string
	version string
	now     func() time.Time

	width, height int
	status        *api.Status
	statusErr     string
	tasks         []harbor.Task
	tasksErr      string
	tasksLoaded   bool
	models        []api.Model
	modelsErr     string
	jobs          []harbor.Job
	events        chan event

	input     textinput.Model
	menu      int  // the highlighted suggestion
	focusList bool // keys go to the page's list, not the prompt

	page   page
	echo   string // the command that made the page
	note   string
	noteOK bool // the note is good news, not an error
	scroll int
	filter string // /models filter

	job     *harbor.Job // the job on the runs, results and compare pages
	selKey  string      // the selected attempt: task|model|attempt
	steps   map[string][]harbor.Step
	grades  map[string]*harbor.Grade // by trial id; nil value: none
	compare []harbor.CompareRow

	jobSel int
	pick   picker

	confirm *confirm
	frame   int
	keep    bool // leave the server running after quitting
}

// confirm is a question at the bottom of the screen; each key runs its
// choice.
type confirm struct {
	text    string
	choices []choice
}

type choice struct {
	key, label string
	run        func(m *Model) tea.Cmd
}

// picker chooses the models for /matrix.
type picker struct {
	filter string
	cursor int
	chosen map[string]bool
}

// New returns the console for the API at base. owned says this process
// serves it, so quitting would end runs' only way to be stopped.
func New(ctx context.Context, base string, owned bool, setPath string) *Model {
	in := textinput.New()
	in.Prompt = ""
	in.Placeholder = `Type / for commands…  "/run"`
	in.PlaceholderStyle = sDim
	in.TextStyle = sText
	in.Cursor.Style = sText
	in.Cursor.SetMode(cursor.CursorStatic)
	in.Focus()
	return &Model{
		c:       newClient(base),
		ctx:     ctx,
		owned:   owned,
		set:     loadSettings(setPath),
		setPath: setPath,
		version: version(),
		now:     time.Now,
		width:   100,
		height:  30,
		events:  make(chan event, 256),
		input:   in,
		steps:   map[string][]harbor.Step{},
		grades:  map[string]*harbor.Grade{},
	}
}

// Messages from commands.
type (
	statusMsg struct {
		s   api.Status
		err error
	}
	tasksMsg struct {
		t   []harbor.Task
		err error
	}
	modelsMsg struct {
		m   []api.Model
		err error
	}
	jobsMsg struct {
		j   []harbor.Job
		err error
	}
	jobMsg struct {
		j   harbor.Job
		err error
	}
	compareMsg struct {
		job  string
		rows []harbor.CompareRow
		err  error
	}
	stepsMsg struct {
		trial string
		s     []harbor.Step
		err   error
	}
	gradeMsg struct {
		trial string
		g     *harbor.Grade
	}
	startedMsg struct {
		j   harbor.Job
		err error
	}
	stoppedMsg struct {
		job string
		err error
	}
	pausedMsg struct {
		job string
		err error
	}
	resumedMsg struct {
		job string
		err error
	}
	eventMsg  event
	tickMsg   time.Time
	openedMsg struct{ err error }
)

func (m *Model) Init() tea.Cmd {
	go m.stream()
	return tea.Batch(m.loadStatus(), m.loadTasks(), m.loadJobs(), m.nextEvent(), tick())
}

// stream keeps /api/events flowing into m.events, reconnecting when the
// stream breaks.
func (m *Model) stream() {
	for m.ctx.Err() == nil {
		m.c.events(m.ctx, m.events)
		select {
		case <-m.ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

func (m *Model) nextEvent() tea.Cmd {
	return func() tea.Msg {
		select {
		case e := <-m.events:
			return eventMsg(e)
		case <-m.ctx.Done():
			return nil
		}
	}
}

func tick() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) loadStatus() tea.Cmd {
	return func() tea.Msg { s, err := m.c.status(m.ctx); return statusMsg{s, err} }
}

func (m *Model) loadTasks() tea.Cmd {
	return func() tea.Msg { t, err := m.c.tasks(m.ctx); return tasksMsg{t, err} }
}

func (m *Model) loadModels() tea.Cmd {
	return func() tea.Msg { l, err := m.c.models(m.ctx); return modelsMsg{l, err} }
}

func (m *Model) loadJobs() tea.Cmd {
	return func() tea.Msg { j, err := m.c.jobs(m.ctx); return jobsMsg{j, err} }
}

func (m *Model) loadJob(id string) tea.Cmd {
	return func() tea.Msg { j, err := m.c.job(m.ctx, id); return jobMsg{j, err} }
}

func (m *Model) loadCompare(id string) tea.Cmd {
	return func() tea.Msg { r, err := m.c.compare(m.ctx, id); return compareMsg{id, r, err} }
}

func (m *Model) loadSteps(job, trial string) tea.Cmd {
	return func() tea.Msg { s, err := m.c.steps(m.ctx, job, trial); return stepsMsg{trial, s, err} }
}

func (m *Model) loadGrade(job, trial string) tea.Cmd {
	return func() tea.Msg {
		g, err := m.c.grade(m.ctx, job, trial)
		if err != nil {
			return gradeMsg{trial, nil}
		}
		return gradeMsg{trial, &g}
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.Width = max(10, min(m.width, 80)-8)
	case tea.KeyMsg:
		return m, m.key(msg)
	case tickMsg:
		m.frame++
		if m.frame%50 == 0 {
			return m, tea.Batch(tick(), m.loadStatus())
		}
		return m, tick()
	case statusMsg:
		if msg.err != nil {
			m.statusErr = msg.err.Error()
		} else {
			m.status, m.statusErr = &msg.s, ""
		}
	case tasksMsg:
		m.tasksLoaded = true
		if msg.err != nil {
			m.tasksErr = msg.err.Error()
			break
		}
		m.tasks, m.tasksErr = msg.t, ""
		if m.taskByName(m.set.Task) == nil && len(m.tasks) > 0 {
			m.set.Task = m.tasks[0].Name
			m.save()
		}
	case modelsMsg:
		if msg.err != nil {
			m.modelsErr = msg.err.Error()
			m.fail("Couldn't fetch OpenRouter's model list: %v", msg.err)
			break
		}
		m.models, m.modelsErr = msg.m, ""
		if mod := m.modelByID(m.set.Model); mod != nil && m.set.Name != displayName(*mod) {
			m.set.Name = displayName(*mod)
			m.save()
		}
	case jobsMsg:
		if msg.err == nil {
			m.jobs = msg.j
		}
	case jobMsg:
		if msg.err != nil {
			m.fail("Couldn't read the run: %v", msg.err)
			break
		}
		return m, m.gotJob(msg.j)
	case compareMsg:
		if msg.err != nil {
			m.fail("Couldn't compare: %v", msg.err)
		} else if m.job != nil && m.job.ID == msg.job {
			m.compare = msg.rows
		}
	case stepsMsg:
		if msg.err == nil {
			m.steps[msg.trial] = msg.s
		}
	case gradeMsg:
		m.grades[msg.trial] = msg.g
	case startedMsg:
		if msg.err != nil {
			m.fail("Harbor didn't start: %v", msg.err)
			break
		}
		m.ok("Started %s.", msg.j.ID)
		m.job, m.selKey, m.compare = nil, "", nil
		m.showPage(pageRuns, "")
		m.upsertJob(msg.j)
		return m, m.gotJob(msg.j)
	case stoppedMsg:
		if msg.err != nil {
			m.fail("Couldn't stop %s: %v", msg.job, msg.err)
		} else {
			m.ok("Stopping %s: Harbor cancels its attempts and removes their containers (about 15 seconds).", msg.job)
		}
	case pausedMsg:
		if msg.err != nil {
			m.fail("Couldn't pause %s: %v", msg.job, msg.err)
		} else {
			m.ok("Pausing %s: Harbor cancels the attempts in progress (about 15 seconds). /resume continues it.", msg.job)
		}
	case resumedMsg:
		if msg.err != nil {
			m.fail("Couldn't resume %s: %v", msg.job, msg.err)
		} else {
			m.ok("Resumed %s.", msg.job)
		}
		return m, m.loadJob(msg.job)
	case eventMsg:
		return m, tea.Batch(m.nextEvent(), m.gotEvent(event(msg)))
	case openedMsg:
		if msg.err != nil {
			m.fail("OpenCode: %v", msg.err)
		} else {
			m.note = ""
		}
	}
	return m, nil
}

// gotEvent applies a job or trial event and refetches what the screen
// shows of it.
func (m *Model) gotEvent(e event) tea.Cmd {
	if e.job != nil {
		m.upsertJob(*e.job)
		if m.job != nil && m.job.ID == e.job.ID {
			return m.loadJob(e.job.ID)
		}
		return nil
	}
	t := e.trial
	if m.job == nil || m.job.ID != t.Job {
		return nil
	}
	cmds := []tea.Cmd{m.loadJob(t.Job)}
	if sel := m.selected(); sel != nil && sel.ID == t.ID {
		cmds = append(cmds, m.loadSteps(t.Job, t.ID))
	}
	return tea.Batch(cmds...)
}

func (m *Model) upsertJob(j harbor.Job) {
	j.Trials = nil
	for i := range m.jobs {
		if m.jobs[i].ID == j.ID {
			m.jobs[i] = j
			return
		}
	}
	m.jobs = append([]harbor.Job{j}, m.jobs...)
}

// gotJob shows j, with its trials, and fetches what the page needs of it.
func (m *Model) gotJob(j harbor.Job) tea.Cmd {
	m.job = &j
	var cmds []tea.Cmd
	if m.selected() == nil {
		m.selKey = ""
		for _, t := range m.order() {
			if m.selKey == "" || t.Status == harbor.Running {
				m.selKey = trialKey(t)
				if t.Status == harbor.Running {
					break
				}
			}
		}
	}
	if t := m.selected(); t != nil && t.ID != "" {
		if _, ok := m.steps[t.ID]; !ok || t.Status == harbor.Running {
			cmds = append(cmds, m.loadSteps(j.ID, t.ID))
		}
	}
	if m.page == pageResults || m.page == pageCompare {
		cmds = append(cmds, m.loadCompare(j.ID))
		if m.page == pageResults {
			cmds = append(cmds, m.loadGrades()...)
		}
	}
	return tea.Batch(cmds...)
}

// loadGrades fetches the grade of each graded attempt not fetched yet.
func (m *Model) loadGrades() []tea.Cmd {
	var cmds []tea.Cmd
	for _, t := range m.job.Trials {
		if _, ok := m.grades[t.ID]; !ok && (t.Status == harbor.Pass || t.Status == harbor.Fail) {
			m.grades[t.ID] = nil
			cmds = append(cmds, m.loadGrade(t.Job, t.ID))
		}
	}
	return cmds
}

func trialKey(t harbor.Trial) string { return fmt.Sprintf("%s|%s|%d", t.Task, t.Model, t.Attempt) }

// order is the current job's attempts in grid order: model, arm, attempt.
func (m *Model) order() []harbor.Trial {
	if m.job == nil {
		return nil
	}
	ts := append([]harbor.Trial(nil), m.job.Trials...)
	models := modelOrder(*m.job)
	rank := map[string]int{}
	for i, mod := range models {
		rank[mod] = i
	}
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if rank[a.Model] != rank[b.Model] {
			return rank[a.Model] < rank[b.Model]
		}
		if a.Arm != b.Arm {
			return a.Arm < b.Arm
		}
		return a.Attempt < b.Attempt
	})
	return ts
}

// modelOrder is the job's models as the job lists them, then any only its
// trials name.
func modelOrder(j harbor.Job) []string {
	seen := map[string]bool{}
	var out []string
	for _, mod := range j.Models {
		if !seen[mod] {
			seen[mod] = true
			out = append(out, mod)
		}
	}
	for _, t := range j.Trials {
		if !seen[t.Model] {
			seen[t.Model] = true
			out = append(out, t.Model)
		}
	}
	return out
}

func (m *Model) selected() *harbor.Trial {
	if m.job == nil {
		return nil
	}
	for i := range m.job.Trials {
		if trialKey(m.job.Trials[i]) == m.selKey {
			return &m.job.Trials[i]
		}
	}
	return nil
}

func (m *Model) moveSelection(d int) tea.Cmd {
	ts := m.order()
	if len(ts) == 0 {
		return nil
	}
	i := 0
	for k, t := range ts {
		if trialKey(t) == m.selKey {
			i = k
		}
	}
	i = (i + d + len(ts)) % len(ts)
	m.selKey = trialKey(ts[i])
	if t := ts[i]; t.ID != "" {
		if _, ok := m.steps[t.ID]; !ok || t.Status == harbor.Running {
			return m.loadSteps(t.Job, t.ID)
		}
	}
	return nil
}

func (m *Model) taskByName(name string) *harbor.Task {
	for i := range m.tasks {
		if m.tasks[i].Name == name {
			return &m.tasks[i]
		}
	}
	return nil
}

func (m *Model) modelByID(id string) *api.Model {
	for i := range m.models {
		if m.models[i].ID == id {
			return &m.models[i]
		}
	}
	return nil
}

// displayName is OpenRouter's name without its vendor: "Z.AI: GLM 5.3" is
// GLM 5.3.
func displayName(mod api.Model) string {
	if _, after, ok := strings.Cut(mod.Name, ": "); ok {
		return after
	}
	if mod.Name != "" {
		return mod.Name
	}
	return mod.ID
}

func (m *Model) save() {
	if err := m.set.save(m.setPath); err != nil {
		m.fail("Couldn't save settings: %v", err)
	}
}

func (m *Model) fail(format string, args ...any) {
	m.note, m.noteOK = fmt.Sprintf(format, args...), false
}

func (m *Model) ok(format string, args ...any) {
	m.note, m.noteOK = fmt.Sprintf(format, args...), true
}

func (m *Model) showPage(p page, echo string) {
	m.page, m.echo, m.scroll = p, echo, 0
	m.focusList = p.listPage()
	if m.focusList {
		m.input.Blur()
	} else {
		m.input.Focus()
	}
}

// runningHere are the jobs this server started that are still running.
func (m *Model) runningHere() []harbor.Job {
	var out []harbor.Job
	for _, j := range m.jobs {
		if j.Stoppable {
			out = append(out, j)
		}
	}
	return out
}

// quit leaves, first asking what to do with runs only this process can
// stop.
func (m *Model) quit() tea.Cmd {
	running := m.runningHere()
	if !m.owned || len(running) == 0 {
		return tea.Quit
	}
	m.confirm = &confirm{
		text: fmt.Sprintf("%s started here still going. This console serves the API they report to.",
			plural(len(running), "run is", "runs are")),
		choices: []choice{
			{"s", "stop them and quit", func(m *Model) tea.Cmd {
				var cmds []tea.Cmd
				for _, j := range running {
					cmds = append(cmds, m.stopJob(j.ID))
				}
				return tea.Sequence(append(cmds, tea.Quit)...)
			}},
			{"k", "keep the server running", func(m *Model) tea.Cmd {
				m.keep = true
				return tea.Quit
			}},
			{"esc", "stay", func(*Model) tea.Cmd { return nil }},
		},
	}
	return nil
}

func (m *Model) stopJob(id string) tea.Cmd {
	return func() tea.Msg { return stoppedMsg{id, m.c.stop(m.ctx, id)} }
}

func (m *Model) key(k tea.KeyMsg) tea.Cmd {
	if m.confirm != nil {
		for _, c := range m.confirm.choices {
			if k.String() == c.key || (c.key == "esc" && k.Type == tea.KeyCtrlC) {
				m.confirm = nil
				return c.run(m)
			}
		}
		return nil
	}
	switch k.String() {
	case "ctrl+c":
		return m.quit()
	case "pgup":
		m.scroll = max(0, m.scroll-(m.height/2))
		return nil
	case "pgdown":
		m.scroll += m.height / 2
		return nil
	case "ctrl+p":
		m.openPrompt("/")
		return nil
	}
	if m.focusList {
		return m.listKey(k)
	}
	return m.promptKey(k)
}

func (m *Model) openPrompt(text string) {
	m.focusList = false
	m.input.Focus()
	m.input.SetValue(text)
	m.input.CursorEnd()
	m.menu = 0
}

// listKey handles keys on a page's list while the prompt is closed.
func (m *Model) listKey(k tea.KeyMsg) tea.Cmd {
	if k.String() == "/" && m.page != pageMatrix {
		m.openPrompt("/")
		return nil
	}
	switch m.page {
	case pageRuns:
		switch k.String() {
		case "up", "left", "k", "shift+tab":
			return m.moveSelection(-1)
		case "down", "right", "j", "tab":
			return m.moveSelection(1)
		case "enter":
			return m.show(m.selected())
		case "esc":
			return m.askStopAttempt()
		case "r":
			m.showPage(pageResults, "/results")
			return m.gotJob(*m.job)
		case "p":
			if m.job.Resumable {
				return m.resume(*m.job)
			}
			return m.askPause(*m.job)
		case "q":
			return m.quit()
		}
	case pageJobs:
		switch k.String() {
		case "up", "k":
			m.jobSel = max(0, m.jobSel-1)
		case "down", "j":
			m.jobSel = min(len(m.jobs)-1, m.jobSel+1)
		case "enter":
			if m.jobSel < len(m.jobs) {
				return m.openJob(m.jobs[m.jobSel].ID)
			}
		case "esc":
			m.showPage(pageHome, "")
		}
	case pageMatrix:
		return m.pickKey(k)
	}
	return nil
}

func (m *Model) openJob(id string) tea.Cmd {
	if m.job == nil || m.job.ID != id {
		m.job, m.selKey, m.compare = nil, "", nil
	}
	m.showPage(pageRuns, "")
	return m.loadJob(id)
}

// askStopAttempt confirms stopping the selected attempt. The API stops
// whole jobs only, so the question says what else goes with it.
func (m *Model) askStopAttempt() tea.Cmd {
	t := m.selected()
	if t == nil || m.job == nil {
		return nil
	}
	if t.Status != harbor.Running && t.Status != harbor.Queued {
		m.fail("This attempt has finished; there is nothing to stop.")
		return nil
	}
	return m.askStop(*m.job)
}

// askPause confirms pausing a run whose attempts in progress would start
// over on resuming.
func (m *Model) askPause(j harbor.Job) tea.Cmd {
	if j.Status != harbor.JobRunning {
		m.fail("%s isn't running.", j.ID)
		return nil
	}
	if !j.Stoppable {
		m.fail("%s was started by another bench process; only that process can pause it.", j.ID)
		return nil
	}
	running := 0
	for _, t := range j.Trials {
		if t.Status == harbor.Running {
			running++
		}
	}
	pause := func(m *Model) tea.Cmd {
		return func() tea.Msg { return pausedMsg{j.ID, m.c.pause(m.ctx, j.ID)} }
	}
	if running == 0 {
		return pause(m)
	}
	them := "them"
	if running == 1 {
		them = "it"
	}
	m.confirm = &confirm{
		text: fmt.Sprintf("Pause %s? Finished attempts are kept; %s start over when you resume, and what was spent on %s is lost.", j.ID,
			plural(running, "running attempt", "running attempts"), them),
		choices: []choice{
			{"y", "pause", pause},
			{"esc", "keep going", func(*Model) tea.Cmd { return nil }},
		},
	}
	return nil
}

func (m *Model) resume(j harbor.Job) tea.Cmd {
	if !j.Resumable {
		if j.Status == harbor.JobDone {
			m.fail("%s has finished.", j.ID)
		} else if j.Status == harbor.JobPausing {
			m.fail("%s is still pausing; try again in a few seconds.", j.ID)
		} else if j.Status == harbor.JobRunning {
			m.fail("%s is running.", j.ID)
		} else {
			m.fail("%s wasn't started by bench, so it can't be resumed here.", j.ID)
		}
		return nil
	}
	if m.status != nil && m.status.JobsWarning != "" {
		m.fail("%s", m.status.JobsWarning)
		return nil
	}
	return func() tea.Msg { return resumedMsg{j.ID, m.c.resume(m.ctx, j.ID)} }
}

func (m *Model) askStop(j harbor.Job) tea.Cmd {
	if j.Status != harbor.JobRunning {
		m.fail("%s isn't running.", j.ID)
		return nil
	}
	if !j.Stoppable {
		m.fail("%s was started by another bench process; only that process can stop it.", j.ID)
		return nil
	}
	running, queued := 0, 0
	for _, t := range j.Trials {
		switch t.Status {
		case harbor.Running:
			running++
		case harbor.Queued:
			queued++
		}
	}
	m.confirm = &confirm{
		text: fmt.Sprintf("Stop %s? Harbor stops whole runs: this cancels %s and %s.", j.ID,
			plural(running, "running attempt", "running attempts"), plural(queued, "queued one", "queued ones")),
		choices: []choice{
			{"y", "stop", func(m *Model) tea.Cmd { return m.stopJob(j.ID) }},
			{"esc", "keep going", func(*Model) tea.Cmd { return nil }},
		},
	}
	return nil
}

// promptKey handles keys while the prompt is open.
func (m *Model) promptKey(k tea.KeyMsg) tea.Cmd {
	sugg := m.suggestions()
	switch k.String() {
	case "up":
		if len(sugg) > 0 {
			m.menu = (m.menu - 1 + len(sugg)) % len(sugg)
		}
		return nil
	case "down":
		if len(sugg) > 0 {
			m.menu = (m.menu + 1) % len(sugg)
		}
		return nil
	case "tab":
		if len(sugg) > 0 {
			m.complete(sugg[min(m.menu, len(sugg)-1)])
			return nil
		}
		if m.input.Value() == "" {
			m.cycleArm()
		}
		return nil
	case "esc":
		if m.input.Value() != "" {
			m.input.SetValue("")
			m.menu = 0
			return nil
		}
		if m.page.listPage() {
			m.focusList = true
			m.input.Blur()
		}
		m.note = ""
		return nil
	case "enter":
		line := strings.TrimSpace(m.input.Value())
		if len(sugg) > 0 {
			s := sugg[min(m.menu, len(sugg)-1)]
			if s.value != line {
				if s.needsArg {
					m.complete(s)
					return nil
				}
				line = s.value
			}
		}
		m.input.SetValue("")
		m.menu = 0
		return m.exec(line)
	}
	before := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	if m.input.Value() != before {
		m.menu = 0
	}
	return cmd
}

func (m *Model) complete(s suggestion) {
	v := s.value
	if s.needsArg {
		v += " "
	}
	m.input.SetValue(v)
	m.input.CursorEnd()
	m.menu = 0
}

func (m *Model) cycleArm() {
	next := map[string]string{"kavach": "control", "control": "both", "both": "kavach"}
	m.set.Arm = next[m.set.Arm]
	if m.set.Arm == "" {
		m.set.Arm = "kavach"
	}
	m.save()
}

// pickKey handles the /matrix model picker.
func (m *Model) pickKey(k tea.KeyMsg) tea.Cmd {
	items := m.pickItems()
	switch k.Type {
	case tea.KeyUp:
		m.pick.cursor = max(0, m.pick.cursor-1)
	case tea.KeyDown:
		m.pick.cursor = min(len(items)-1, m.pick.cursor+1)
	case tea.KeySpace:
		if m.pick.cursor < len(items) {
			id := items[m.pick.cursor].ID
			m.pick.chosen[id] = !m.pick.chosen[id]
		}
	case tea.KeyEnter:
		var ids []string
		for id, on := range m.pick.chosen {
			if on {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		if len(ids) == 0 {
			m.fail("Pick at least one model: space toggles the highlighted one.")
			return nil
		}
		return m.startMatrix(ids)
	case tea.KeyEsc:
		if m.pick.filter != "" {
			m.pick.filter, m.pick.cursor = "", 0
			return nil
		}
		m.showPage(pageHome, "")
	case tea.KeyBackspace:
		if m.pick.filter != "" {
			r := []rune(m.pick.filter)
			m.pick.filter, m.pick.cursor = string(r[:len(r)-1]), 0
		}
	case tea.KeyRunes:
		m.pick.filter += string(k.Runes)
		m.pick.cursor = 0
	}
	return nil
}

// pickItems are the models the picker lists: those that support tools and
// match the filter, chosen ones first.
func (m *Model) pickItems() []api.Model {
	var chosen, rest []api.Model
	q := strings.ToLower(m.pick.filter)
	for _, mod := range m.models {
		if !mod.Tools {
			continue
		}
		if m.pick.chosen[mod.ID] {
			chosen = append(chosen, mod)
		} else if strings.Contains(strings.ToLower(mod.ID), q) || strings.Contains(strings.ToLower(mod.Name), q) {
			rest = append(rest, mod)
		}
	}
	return append(chosen, rest...)
}
