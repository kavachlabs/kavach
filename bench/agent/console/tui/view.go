package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// wordmark is "kavach" in peach and "bench" in blue, in OpenCode's block
// letters.
var wordmark = [3][2]string{
	{"█  ▄ ▀▀▀█ █  █ ▀▀▀█ █▀▀▀ █   ", "█    ▄▀▀▀ █▀▀▄ █▀▀▀ █   "},
	{"█▄▀  █▀▀█ █  █ █▀▀█ █    █▀▀▄", "█▀▀█ █▀▀▀ █  █ █    █▀▀▄"},
	{"█ ▀▄ ▀▀▀▀  ▀▀  ▀▀▀▀ ▀▀▀▀ ▀  ▀", "▀▀▀▀ ▀▀▀▀ ▀  ▀ ▀▀▀▀ ▀  ▀"},
}

func version() string {
	// A build from a checkout gets a pseudo-version (v0.0.0-<time>-<commit>),
	// which says nothing useful here.
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" && !strings.HasPrefix(bi.Main.Version, "v0.0.0-") {
		return bi.Main.Version
	}
	return "dev"
}

// margin is the blank border around the screen, in columns.
const margin = 2

func (m *Model) View() string {
	var lines []string
	if m.page == pageHome {
		lines = m.homeView()
	} else {
		lines = m.sessionView()
	}
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	lines = lines[:m.height]
	for i, l := range lines {
		lines[i] = fill(l, m.width, colBg)
	}
	return strings.Join(lines, "\n")
}

// promptWidth is the width of the prompt box on the start screen.
func (m *Model) promptWidth() int { return min(76, m.width-2*margin) }

func (m *Model) homeView() []string {
	pw := m.promptWidth()
	indent := strings.Repeat(" ", max(0, (m.width-pw)/2))
	var block []string
	for _, w := range wordmark {
		block = append(block, center(sPeach.Render(w[0])+" "+sBlue.Render(w[1]), m.width))
	}
	block = append(block, "", "")
	for _, l := range m.bottomLines(pw) {
		block = append(block, indent+l)
	}

	foot := m.footer(m.width - 2*margin)
	top := max(0, (m.height-len(block)-1)/2-1)
	var out []string
	for range top {
		out = append(out, "")
	}
	out = append(out, block...)
	for len(out) < m.height-1 {
		out = append(out, "")
	}
	return append(out, strings.Repeat(" ", margin)+foot)
}

// bottomLines are the note, question, pop-up, prompt and key hints, in
// width.
func (m *Model) bottomLines(width int) []string {
	var out []string
	if m.note != "" {
		style := sRed
		if m.noteOK {
			style = sDim
		}
		out = append(out, style.Render(m.note), "")
	}
	if m.confirm != nil {
		var keys []string
		for _, c := range m.confirm.choices {
			keys = append(keys, c.key, c.label)
		}
		out = append(out, panel([]string{"", sBold.Render(m.confirm.text), "", hints(keys...), ""}, width, colPeach, colPanel)...)
		return out
	}
	focusPrompt := !m.focusList
	sugg := m.suggestions()
	if focusPrompt && len(sugg) > 0 {
		out = append(out, m.menuView(sugg, width)...)
	}
	if focusPrompt {
		out = append(out, panel([]string{"", m.input.View(), "", m.runLine(), ""}, width, colBlue, colPanel)...)
	}
	out = append(out, m.hintLine(width, focusPrompt && len(sugg) > 0))
	return out
}

// menuLines is how many suggestions the pop-up shows at once.
const menuLines = 8

func (m *Model) menuView(sugg []suggestion, width int) []string {
	first := 0
	if m.menu >= menuLines {
		first = m.menu - menuLines + 1
	}
	labelW := 0
	for _, s := range sugg {
		labelW = max(labelW, lipgloss.Width(s.label))
	}
	labelW = min(labelW+3, 44)
	out := []string{fill("", width, colPanel)}
	for i := first; i < len(sugg) && i < first+menuLines; i++ {
		s := sugg[i]
		label := s.label + strings.Repeat(" ", max(1, labelW-lipgloss.Width(s.label)))
		if i == m.menu {
			plain := "  " + label + s.desc
			out = append(out, lipgloss.NewStyle().Foreground(colOnPill).Background(colPeach).Render(fill(plain, width, "")))
			continue
		}
		out = append(out, fill("  "+sText.Render(label)+sDim.Render(s.desc), width, colPanel))
	}
	if n := len(sugg); n > menuLines {
		out = append(out, fill(sFaint.Render(fmt.Sprintf("  %d of %d", m.menu+1, n)), width, colPanel))
	}
	return append(out, fill("", width, colPanel))
}

// runLine says what /run will do: arm, task, model, provider, runs.
func (m *Model) runLine() string {
	var arm string
	switch m.set.Arm {
	case "control":
		arm = sText.Render("Control")
	case "both":
		arm = sPeach.Render("Kavach") + sDim.Render(" + ") + sText.Render("Control")
	default:
		arm = sPeach.Render("Kavach")
	}
	task := sText.Render(m.set.Task)
	if m.set.Task == "" {
		task = sDim.Render("no task")
	}
	model := sBold.Render(m.set.Agent) + " " + sDim.Render("agent")
	if m.needsModel() {
		model = sBold.Render(m.set.Name) + " " + sDim.Render("OpenRouter")
	}
	runs := sDim.Render("× " + plural(m.set.Runs, "run", "runs"))
	if m.set.Timeout > 0 {
		runs += sDim.Render(fmt.Sprintf(" · %dm timeout", m.set.Timeout))
	}
	return strings.Join([]string{arm, task, model, runs}, "  ")
}

func (m *Model) hintLine(width int, menu bool) string {
	var keys string
	switch {
	case m.confirm != nil:
		return ""
	case menu:
		keys = hints("↑↓", "select", "tab", "complete", "enter", "run", "esc", "close")
	case m.page == pageRuns && m.focusList:
		keys = hints("enter", "open in opencode", "↑↓", "select", "esc", "stop this attempt", "ctrl+p", "commands")
	case m.page == pageJobs && m.focusList:
		keys = hints("enter", "open", "↑↓", "select", "esc", "back", "ctrl+p", "commands")
	case m.page == pageMatrix && m.focusList:
		keys = hints("type", "to filter", "space", "pick", "enter", "start", "esc", "back")
	case m.page == pageHome:
		keys = hints("tab", "arm", "/", "commands", "ctrl+c", "quit")
	case m.page == pageResults || m.page == pageCompare:
		keys = hints("/show", "open an attempt", "/jobs", "other runs", "ctrl+p", "commands")
	default:
		keys = hints("pgup/pgdn", "scroll", "ctrl+p", "commands")
	}
	return spread(m.spinner(), keys, width)
}

// spinner is OpenCode's row of dots with a light running through it, shown
// while any run is going.
func (m *Model) spinner() string {
	running := false
	for _, j := range m.jobs {
		running = running || j.Status == harbor.JobRunning
	}
	if !running || m.page == pageHome {
		return ""
	}
	const n = 8
	pos := m.frame % (2*n - 2)
	if pos >= n {
		pos = 2*n - 2 - pos
	}
	var b strings.Builder
	for i := range n {
		if i == pos {
			b.WriteString(sPeach.Render("■"))
		} else {
			b.WriteString(sFaint.Render("·"))
		}
		b.WriteString(" ")
	}
	return b.String()
}

func (m *Model) footer(width int) string {
	var parts []string
	st := m.status
	switch {
	case m.statusErr != "":
		parts = append(parts, sRed.Render("The bench API isn't answering: "+m.statusErr))
	case st == nil:
		parts = append(parts, sDim.Render("connecting…"))
	default:
		if st.Docker.OK {
			parts = append(parts, sGreen.Render("⊙")+sDim.Render(" docker ready"))
		} else {
			parts = append(parts, sRed.Render("⊙ Docker isn't running: start Colima with `colima start`"))
		}
		if st.Harbor.OK {
			parts = append(parts, sDim.Render("harbor "+strings.TrimPrefix(st.Harbor.Version, "harbor ")))
		} else {
			parts = append(parts, sRed.Render("harbor isn't on PATH: install it with `uv tool install harbor`"))
		}
		if st.Tasks == 0 {
			parts = append(parts, sRed.Render("No tasks found under "+shortPath(st.TasksDir)))
		} else {
			parts = append(parts, sDim.Render(plural(st.Tasks, "task", "tasks")))
		}
		parts = append(parts, sDim.Render(plural(len(m.jobs), "run saved", "runs saved")+" in "+shortPath(st.JobsDir)))
	}
	right := sDim.Render(m.version)
	left := strings.Join(parts, "  ")
	if room := width - lipgloss.Width(right) - 2; lipgloss.Width(left) > room {
		left = fill(left, room, "")
	}
	return spread(left, right, width)
}

// shortPath writes p relative to the current directory when it is inside
// it, else with the home directory as ~.
func shortPath(p string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return p
}

func (m *Model) sessionView() []string {
	cw := m.width - 2*margin
	pad := strings.Repeat(" ", margin)
	title, right := m.header()
	var out []string
	out = append(out, "")
	for _, l := range panel([]string{"", spread(sBold.Render(title), sDim.Render(right), cw-4), ""}, cw, colFaint, colHead) {
		out = append(out, pad+l)
	}
	out = append(out, "")
	bottom := m.bottomLines(cw)
	avail := max(1, m.height-len(out)-len(bottom)-1)
	body := m.body(cw, avail)
	if len(body) > avail {
		if f := m.focusLine(); f >= 0 {
			m.scroll = min(max(m.scroll, f-avail+1), f)
		}
		m.scroll = min(m.scroll, len(body)-avail)
		body = body[m.scroll : m.scroll+avail]
	} else {
		m.scroll = 0
	}
	for _, l := range body {
		out = append(out, pad+l)
	}
	for len(out) < m.height-len(bottom)-1 {
		out = append(out, "")
	}
	for _, l := range bottom {
		out = append(out, pad+l)
	}
	return out
}

// focusLine is the body line the selection is on, kept in view; -1 for
// none.
func (m *Model) focusLine() int {
	switch m.page {
	case pageJobs:
		return m.jobSel + 2
	case pageMatrix:
		return m.pick.cursor + 4
	}
	return -1
}

// header is the title bar's text: the run on job pages, else the task.
func (m *Model) header() (title, right string) {
	if j := m.job; j != nil && (m.page == pageRuns || m.page == pageResults || m.page == pageCompare) {
		bases, arms, runs := map[string]bool{}, map[string]bool{}, 0
		var base string
		for _, t := range j.Tasks {
			b, a := harbor.SplitArm(t)
			if !bases[b] {
				base = b
			}
			bases[b], arms[a] = true, true
		}
		for _, t := range j.Trials {
			runs = max(runs, t.Attempt)
		}
		if len(bases) > 1 {
			base = plural(len(bases), "task", "tasks")
		}
		models := plural(len(j.Models), "model", "models")
		if len(j.Models) == 0 {
			models = j.Agent
		}
		title = fmt.Sprintf("# %s · %s × %s × %s", base, models, plural(len(arms), "arm", "arms"), plural(max(runs, 1), "run", "runs"))
		right = fmt.Sprintf("%d of %d done   %s", j.Done, j.Total, dollars(j.Cost))
		if j.Status == harbor.JobStopped {
			right = "stopped · " + right
		}
		return title, right
	}
	title = "# " + m.set.Task
	if t := m.taskByName(m.set.Task); t != nil {
		right = t.Title
	}
	return title, right
}

func (m *Model) body(width, height int) []string {
	switch m.page {
	case pageRuns:
		return m.runsBody(width, height)
	case pageResults:
		return append(m.cardsBody(width), append([]string{""}, m.compareBody()...)...)
	case pageCompare:
		return m.compareBody()
	case pageModels:
		return m.modelsBody()
	case pageHelp:
		return m.helpBody()
	case pageJobs:
		return m.jobsBody(width)
	case pageMatrix:
		return m.matrixBody(width)
	}
	return nil
}

func modelID(model string) string { return strings.TrimPrefix(model, "openrouter/") }

// modelShort drops the vendor: z-ai/glm-5.3 is glm-5.3.
func modelShort(model string) string {
	return model[strings.LastIndex(model, "/")+1:]
}

func (m *Model) modelLabel(model string, short bool) string {
	switch {
	case model != "" && short:
		return modelShort(model)
	case model != "":
		return modelID(model)
	case m.job != nil:
		return m.job.Agent
	}
	return "—"
}

func (m *Model) attemptName(t harbor.Trial) string {
	model := modelShort(t.Model)
	if model == "" && m.job != nil {
		model = m.job.Agent
	}
	arm := t.Arm
	if arm == "" {
		arm = t.Task
	}
	return fmt.Sprintf("%s · %s · #%d", model, arm, t.Attempt)
}

func (m *Model) runsBody(width, height int) []string {
	if m.job == nil {
		return []string{sDim.Render("Loading the run…")}
	}
	if m.job.Error != "" {
		return []string{sRed.Render("Harbor stopped before the run began:"), "", sDim.Render(m.job.Error)}
	}
	ts := m.order()
	if len(ts) == 0 {
		return []string{sDim.Render("Harbor is preparing the run: building the task's image can take a few minutes the first time.")}
	}
	var arms []string
	seen := map[string]bool{}
	for _, t := range ts {
		if !seen[t.Arm] {
			seen[t.Arm] = true
			arms = append(arms, t.Arm)
		}
	}
	header := []string{sDim.Render("model")}
	for _, a := range arms {
		if a == "" {
			a = "attempts"
		}
		header = append(header, sDim.Render(a))
	}
	rows := [][]string{header}
	for _, model := range modelOrder(*m.job) {
		row := []string{sText.Render(m.modelLabel(model, false))}
		for _, a := range arms {
			var dots []string
			for _, t := range ts {
				if t.Model == model && t.Arm == a {
					dots = append(dots, m.gridDot(t))
				}
			}
			row = append(row, strings.Join(dots, " "))
		}
		rows = append(rows, row)
	}
	var out []string
	for _, l := range table(rows, 4) {
		out = append(out, "  "+l)
	}
	out = append(out, "  "+sGreen.Render("●")+sDim.Render(" pass  ")+sRed.Render("●")+sDim.Render(" fail  ")+
		sRed.Render("✕")+sDim.Render(" error  ")+sPeach.Render("◐")+sDim.Render(" running  ")+sFaint.Render("○")+sDim.Render(" queued"), "")
	if t := m.selected(); t != nil {
		out = append(out, panel(m.attemptLines(*t, width-4, height-len(out)-2), width, colPeach, colPanel)...)
	}
	return out
}

func (m *Model) gridDot(t harbor.Trial) string {
	if trialKey(t) != m.selKey {
		return dot(t.Status)
	}
	return lipgloss.NewStyle().Background(colFaint).Bold(true).Render(dot(t.Status))
}

// attemptLines describe one attempt and its latest steps, in at most
// height lines.
func (m *Model) attemptLines(t harbor.Trial, width, height int) []string {
	fresh := t.Tokens.Fresh()
	var head string
	switch t.Status {
	case harbor.Queued:
		return []string{"", sFaint.Render("○ ") + sBold.Render(m.attemptName(t)) + "   " + sDim.Render("queued: it starts when a slot frees"), ""}
	case harbor.Running:
		var parts []string
		if t.Started != nil {
			parts = append(parts, "running "+clock(m.now().Sub(*t.Started).Seconds()))
		} else {
			parts = append(parts, "running")
		}
		if t.Steps > 0 {
			parts = append(parts, fmt.Sprintf("step %d", t.Steps))
		}
		if fresh > 0 || t.Tokens.Cached > 0 {
			parts = append(parts, fmt.Sprintf("%s tokens (+%s cached)", tokens(fresh), tokens(t.Tokens.Cached)))
		}
		if t.Cost != nil {
			parts = append(parts, dollars(*t.Cost))
		}
		head = sPeach.Render("◐") + " " + sBold.Render(m.attemptName(t)) + "   " + sDim.Render(strings.Join(parts, " · "))
	default:
		head = badge(t.Status) + "  " + sBold.Render(m.attemptName(t)) + "   " + sDim.Render(trialStats(t))
	}
	lines := []string{"", head}
	if t.Error != "" {
		lines = append(lines, sRed.Render(t.Error))
	}
	if t.Kavach != nil {
		lines = append(lines, kavachLine(*t.Kavach))
	}
	steps := m.steps[t.ID]
	var shown []string
	for _, s := range steps {
		if l := stepLine(s, width-2); l != "" {
			shown = append(shown, "  "+l)
		}
	}
	room := max(3, height-len(lines)-2)
	if len(shown) > room {
		shown = shown[len(shown)-room:]
	}
	if len(shown) > 0 {
		lines = append(lines, "")
		lines = append(lines, shown...)
	} else if t.Status == harbor.Running {
		lines = append(lines, "", sDim.Render("  Waiting for the agent's first step… agents other than opencode show none."))
	}
	return append(lines, "")
}

func trialStats(t harbor.Trial) string {
	var parts []string
	if t.Seconds != nil {
		parts = append(parts, duration(*t.Seconds))
	}
	if t.Steps > 0 {
		parts = append(parts, plural(t.Steps, "step", "steps"))
	}
	if t.Tokens.Fresh() > 0 {
		parts = append(parts, tokens(t.Tokens.Fresh())+" tokens")
	}
	if t.Cost != nil {
		parts = append(parts, dollars(*t.Cost))
	}
	return strings.Join(parts, " · ")
}

func kavachLine(k harbor.Kavach) string {
	v := sBold.Render(k.Verdict)
	if k.Verdict != "fixed" {
		v = lipgloss.NewStyle().Foreground(colRed).Bold(true).Render(k.Verdict)
	}
	line := sPeach.Render("kavach diff ") + v
	if k.Variants != "" {
		line += sDim.Render(" · " + k.Variants)
	}
	return line
}

var matchCount = regexp.MustCompile(`^Found (\d+) match`)

// stepLine renders a step as OpenCode shows tool calls: quiet grey lines
// with a glyph per tool, the model's words in bold. Usage steps are left
// out.
func stepLine(s harbor.Step, width int) string {
	title := strings.Join(strings.Fields(s.Title), " ")
	var line string
	switch s.Kind {
	case "usage":
		return ""
	case "text":
		line = sBold.Render(title)
	case "shell":
		line = sDim.Render("$ " + title)
		if s.Exit != nil && *s.Exit != 0 {
			line += sRed.Render(fmt.Sprintf("  exit %d", *s.Exit))
		}
	case "read":
		path, lines, ok := strings.Cut(title, " lines ")
		line = sDim.Render("→ Read " + path)
		if ok {
			line += sFaint.Render(" lines " + lines)
		}
	case "search":
		line = sDim.Render(`* Grep "` + title + `"`)
		if mm := matchCount.FindStringSubmatch(s.Detail); mm != nil {
			line += sFaint.Render(" (" + mm[1] + " matches)")
		}
	case "edit":
		line = sDim.Render("← Edit " + title)
	case "error":
		line = sRed.Render("✕ " + title)
	default:
		if s.Kavach {
			line = sPeach.Render("⚙ " + title)
			if s.Verdict != "" {
				line += "  " + sBold.Render(s.Verdict)
			}
		} else {
			line = sDim.Render("⚙ " + title)
		}
	}
	return fill(line, width, "")
}

// cardsBody is a grade card per finished attempt, two to a row.
func (m *Model) cardsBody(width int) []string {
	if m.job == nil {
		return []string{sDim.Render("Loading the run…")}
	}
	var cards [][]string
	for _, t := range m.order() {
		if t.Status == harbor.Pass || t.Status == harbor.Fail || t.Status == harbor.Error {
			cards = append(cards, m.cardLines(t))
		}
	}
	if len(cards) == 0 {
		return []string{sDim.Render("No attempt has finished yet. /jobs shows the run's progress.")}
	}
	const gap = 3
	cardW := (width - gap) / 2
	var out []string
	for i := 0; i < len(cards); i += 2 {
		left, right := cards[i], []string(nil)
		if i+1 < len(cards) {
			right = cards[i+1]
		}
		n := max(len(left), len(right))
		for k := range n + 2 {
			l := fill("  "+at(left, k-1), cardW, colPanel)
			r := ""
			if right != nil {
				r = fill("  "+at(right, k-1), cardW, colPanel)
			}
			out = append(out, l+strings.Repeat(" ", gap)+r)
		}
		out = append(out, "")
	}
	return out[:len(out)-1]
}

func at(lines []string, i int) string {
	if i < 0 || i >= len(lines) {
		return ""
	}
	return lines[i]
}

func (m *Model) cardLines(t harbor.Trial) []string {
	lines := []string{badge(t.Status) + "  " + sBold.Render(m.attemptName(t))}
	switch {
	case t.Status == harbor.Error:
		lines = append(lines, sRed.Render(t.Error))
	case m.grades[t.ID] != nil && m.grades[t.ID].Passed+m.grades[t.ID].Failed > 0:
		g := m.grades[t.ID]
		if g.Failed > 0 {
			lines = append(lines, sDim.Render(fmt.Sprintf("hidden test: %d of %d tests failed", g.Failed, g.Passed+g.Failed)))
		} else {
			lines = append(lines, sDim.Render(fmt.Sprintf("hidden test: all %d tests passed", g.Passed)))
		}
	case t.Status == harbor.Pass:
		lines = append(lines, sDim.Render("hidden upstream test passed"))
	default:
		lines = append(lines, sDim.Render("hidden upstream test failed"))
	}
	if t.Kavach != nil {
		lines = append(lines, kavachLine(*t.Kavach))
	}
	if s := trialStats(t); s != "" {
		lines = append(lines, sDim.Render(s))
	}
	return lines
}

// compareBody is the table the benchmark is for: per model, the pass rate
// in each arm and what an attempt took, control → kavach.
func (m *Model) compareBody() []string {
	out := []string{sDim.Render("› /compare")}
	if m.compare == nil {
		return append(out, sDim.Render("  Comparing…"))
	}
	type pair struct{ control, kavach *harbor.CompareRow }
	var keys []string
	pairs := map[string]*pair{}
	tasks := map[string]bool{}
	for i := range m.compare {
		r := &m.compare[i]
		tasks[r.Task] = true
		k := r.Task + "\x00" + r.Model
		if pairs[k] == nil {
			pairs[k] = &pair{}
			keys = append(keys, k)
		}
		if r.Arm == "kavach" {
			pairs[k].kavach = r
		} else {
			pairs[k].control = r
		}
	}
	if len(keys) == 0 {
		return append(out, sDim.Render("  Nothing to compare yet."))
	}
	rows := [][]string{{sDim.Render("model"), sDim.Render("control"), sDim.Render("kavach"),
		sDim.Render("median time"), sDim.Render("median tokens"), sDim.Render("cost / attempt")}}
	for _, k := range keys {
		p := pairs[k]
		first := p.control
		if first == nil {
			first = p.kavach
		}
		name := m.modelLabel(first.Model, false)
		if len(tasks) > 1 {
			name = first.Task + " · " + name
		}
		c, kv := passCell(p.control), passCell(p.kavach)
		if rate(p.kavach) > rate(p.control) {
			kv = sBold.Render(kv)
		} else if rate(p.control) > rate(p.kavach) {
			c = sBold.Render(c)
		}
		rows = append(rows, []string{name,
			c + pendingNote(p.control), kv + pendingNote(p.kavach),
			arrow(p.control, p.kavach, func(r *harbor.CompareRow) string {
				if r.MedianSeconds == nil {
					return ""
				}
				return duration(*r.MedianSeconds)
			}),
			arrow(p.control, p.kavach, func(r *harbor.CompareRow) string {
				if r.MedianFresh == nil {
					return ""
				}
				return tokens(*r.MedianFresh)
			}),
			arrow(p.control, p.kavach, func(r *harbor.CompareRow) string {
				if r.MeanCost == nil {
					return ""
				}
				return dollars(*r.MeanCost)
			}),
		})
	}
	for _, l := range table(rows, 4) {
		out = append(out, "  "+l)
	}
	return append(out, "",
		sDim.Render("  control → kavach. A pass is the hidden upstream test passing."),
		sDim.Render("  Tokens are fresh: input not served from cache, plus output."))
}

func passCell(r *harbor.CompareRow) string {
	if r == nil || r.Attempts == 0 {
		return "–"
	}
	return fmt.Sprintf("%d / %d", r.Passes, r.Attempts)
}

func rate(r *harbor.CompareRow) float64 {
	if r == nil || r.Attempts == 0 {
		return -1
	}
	return float64(r.Passes) / float64(r.Attempts)
}

func pendingNote(r *harbor.CompareRow) string {
	if r == nil {
		return ""
	}
	var parts []string
	if r.Errors > 0 {
		parts = append(parts, fmt.Sprintf("%d err", r.Errors))
	}
	if r.Pending > 0 {
		parts = append(parts, fmt.Sprintf("%d left", r.Pending))
	}
	if len(parts) == 0 {
		return ""
	}
	return sFaint.Render(" (" + strings.Join(parts, ", ") + ")")
}

func arrow(control, kavach *harbor.CompareRow, f func(*harbor.CompareRow) string) string {
	get := func(r *harbor.CompareRow) string {
		if r == nil {
			return "–"
		}
		if v := f(r); v != "" {
			return v
		}
		return "–"
	}
	return get(control) + sDim.Render(" → ") + get(kavach)
}

func (m *Model) modelsBody() []string {
	out := []string{sDim.Render("› " + m.echo)}
	if m.models == nil {
		if m.modelsErr != "" {
			return append(out, sRed.Render("  Couldn't fetch OpenRouter's model list: "+m.modelsErr))
		}
		return append(out, sDim.Render("  Fetching OpenRouter's model list…"))
	}
	q := strings.ToLower(m.filter)
	rows := [][]string{{sDim.Render("model"), sDim.Render("context"), sDim.Render("in / M"), sDim.Render("out / M"), sDim.Render("tools")}}
	for _, mod := range m.models {
		if !strings.Contains(strings.ToLower(mod.ID), q) && !strings.Contains(strings.ToLower(mod.Name), q) {
			continue
		}
		tools := sGreen.Render("yes")
		if !mod.Tools {
			tools = sDim.Render("no")
		}
		id := sText.Render(mod.ID)
		if mod.ID == m.set.Model {
			id = sPeach.Render(mod.ID)
		}
		rows = append(rows, []string{id, fmt.Sprintf("%dk", mod.Context/1000), price(mod.PriceIn), price(mod.PriceOut), tools})
	}
	if len(rows) == 1 {
		return append(out, sDim.Render(fmt.Sprintf("  No OpenRouter model matches %q.", m.filter)))
	}
	for _, l := range table(rows, 4) {
		out = append(out, "  "+l)
	}
	return append(out, "", sDim.Render(fmt.Sprintf("  %s. /model <id> picks one; the agent needs tools.", plural(len(rows)-1, "model", "models"))))
}

func (m *Model) helpBody() []string {
	rows := [][]string{}
	for _, c := range commands {
		rows = append(rows, []string{sText.Render(strings.TrimSpace("/" + c.name + " " + c.args)), sDim.Render(c.desc)})
	}
	out := []string{sDim.Render("› /help")}
	for _, l := range table(rows, 4) {
		out = append(out, "  "+l)
	}
	return append(out, "",
		sDim.Render("  Settings are saved in "+shortPath(m.setPath)+"."),
		sDim.Render("  Model ids are OpenRouter's; the console runs them as openrouter/<id> through OpenCode."))
}

func (m *Model) jobsBody(width int) []string {
	out := []string{sDim.Render("› /jobs")}
	if len(m.jobs) == 0 {
		return append(out, sDim.Render("  No runs under "+m.jobsDir()+" yet. Start one with /run."))
	}
	rows := [][]string{{"", sDim.Render("run"), sDim.Render("status"), sDim.Render("agent"), sDim.Render("done"), sDim.Render("cost"), sDim.Render("started")}}
	for _, j := range m.jobs {
		agent := j.Agent
		if len(j.Models) > 0 {
			var ms []string
			for _, mod := range j.Models {
				ms = append(ms, modelShort(mod))
			}
			agent = strings.Join(ms, ", ")
		}
		status := sDim.Render(j.Status)
		switch j.Status {
		case harbor.JobRunning:
			status = sPeach.Render(j.Status)
		case harbor.JobDone:
			status = sGreen.Render(j.Status)
		}
		started := ""
		if j.Started != nil {
			started = j.Started.Local().Format("Jan 2 15:04")
		}
		rows = append(rows, []string{"", sText.Render(j.ID), status, agent, fmt.Sprintf("%d / %d", j.Done, j.Total), dollars(j.Cost), sDim.Render(started)})
	}
	for i, l := range table(rows, 3) {
		if i > 0 && i-1 == m.jobSel {
			l = fill(sPeach.Render("›")+l[1:], width, colSel)
		}
		out = append(out, l)
	}
	return out
}

func (m *Model) jobsDir() string {
	if m.status != nil {
		return shortPath(m.status.JobsDir)
	}
	return "the jobs folder"
}

func (m *Model) matrixBody(width int) []string {
	chosen := 0
	for _, on := range m.pick.chosen {
		if on {
			chosen++
		}
	}
	tasks := 2
	attempts := chosen * tasks * m.set.Runs
	out := []string{sDim.Render("› /matrix"),
		"  " + sText.Render(fmt.Sprintf("%s × 2 arms × %s = %s",
			plural(chosen, "model", "models"), plural(m.set.Runs, "run", "runs"), plural(attempts, "attempt", "attempts"))) +
			sDim.Render("   on "+m.set.Task),
		"  " + sDim.Render("filter ") + sText.Render(m.pick.filter) + lipgloss.NewStyle().Background(colText).Render(" "),
		""}
	if m.models == nil {
		if m.modelsErr != "" {
			return append(out, sRed.Render("  Couldn't fetch OpenRouter's model list: "+m.modelsErr))
		}
		return append(out, sDim.Render("  Fetching OpenRouter's model list…"))
	}
	items := m.pickItems()
	if len(items) == 0 {
		return append(out, sDim.Render(fmt.Sprintf("  No model with tool calls matches %q.", m.pick.filter)))
	}
	rows := [][]string{}
	for _, mod := range items {
		box := sFaint.Render("[ ]")
		if m.pick.chosen[mod.ID] {
			box = sPeach.Render("[x]")
		}
		rows = append(rows, []string{box, sText.Render(mod.ID), sDim.Render(fmt.Sprintf("%dk", mod.Context/1000)),
			sDim.Render(price(mod.PriceIn) + " / " + price(mod.PriceOut))})
	}
	for i, l := range table(rows, 2) {
		if i == m.pick.cursor {
			l = fill("  "+l, width, colSel)
		} else {
			l = "  " + l
		}
		out = append(out, l)
	}
	return out
}
