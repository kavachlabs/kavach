package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// command is one slash command.
type command struct {
	name, args, desc string
	needsArg         bool
	run              func(m *Model, arg string) tea.Cmd
}

var commands []command

func init() {
	commands = []command{
		{"run", "", "Run the agent on the task with the settings below", false, (*Model).cmdRun},
		{"model", "<id>", "Choose the model, an OpenRouter id", true, (*Model).cmdModel},
		{"models", "[filter]", "List OpenRouter models with prices", false, (*Model).cmdModels},
		{"arm", "control|workflow|kavach|kavach-implement|both", "control (no Kavach), workflow, kavach, kavach-implement, or both", true, (*Model).cmdArm},
		{"runs", "<n>", "Attempts per model and arm", true, (*Model).cmdRuns},
		{"task", "<name>", "Choose the task", true, (*Model).cmdTask},
		{"agent", "opencode|oracle|nop", "Choose the agent; oracle applies the known fix, for checking a task", true, (*Model).cmdAgent},
		{"env", "docker|daytona|modal|vercel", "Where attempts run; the cloud providers need their keys in bench/agent/.env", true, (*Model).cmdEnv},
		{"timeout", "<minutes>", "Agent timeout per attempt; 0 keeps the task's", true, (*Model).cmdTimeout},
		{"retries", "<n>", "Times an attempt that ends with an error runs again, 0-5", true, (*Model).cmdRetries},
		{"matrix", "[ids…]", "Several models × both arms × N runs in one go", false, (*Model).cmdMatrix},
		{"jobs", "", "Open an earlier run", false, (*Model).cmdJobs},
		{"results", "", "Grade cards and the comparison for this run", false, (*Model).cmdResults},
		{"compare", "", "Pass rate per model and arm", false, (*Model).cmdCompare},
		{"show", "[trial]", "Open an attempt in OpenCode", false, (*Model).cmdShow},
		{"stop", "", "Stop this run", false, (*Model).cmdStop},
		{"pause", "", "Pause this run; /resume continues it", false, (*Model).cmdPause},
		{"resume", "", "Continue a paused or stopped run", false, (*Model).cmdResume},
		{"retry", "[trial]", "Run a failed attempt again once its run has ended", false, (*Model).cmdRetry},
		{"clear", "", "Back to the start screen", false, (*Model).cmdClear},
		{"help", "", "List the commands", false, (*Model).cmdHelp},
		{"quit", "", "Leave the console", false, (*Model).cmdQuit},
	}
}

// suggestion is one line of the pop-up under the prompt.
type suggestion struct {
	value, label, desc string
	needsArg           bool
}

// menuLimit caps the argument suggestions; the pop-up scrolls through them.
const menuLimit = 50

// armDescs describes each benchmark arm in /arm's completion menu.
var armDescs = map[string]string{
	"control":          "without Kavach",
	"workflow":         "contract-first workflow, without Kavach",
	"kavach":           "with Kavach's tools",
	"kavach-implement": "agent writes and checks its own contract",
}

// suggestions are the completions for what the prompt holds: commands
// while the first word is typed, then the command's argument values.
func (m *Model) suggestions() []suggestion {
	v := m.input.Value()
	if !strings.HasPrefix(v, "/") {
		return nil
	}
	name, arg, hasArg := strings.Cut(v[1:], " ")
	if !hasArg {
		var prefix, inside []suggestion
		for _, c := range commands {
			s := suggestion{"/" + c.name, "/" + c.name, c.desc, c.needsArg}
			if strings.HasPrefix(c.name, name) {
				prefix = append(prefix, s)
			} else if strings.Contains(c.name, name) {
				inside = append(inside, s)
			}
		}
		return append(prefix, inside...)
	}
	var vals []suggestion
	add := func(val, desc string) {
		if strings.Contains(strings.ToLower(val), strings.ToLower(arg)) && len(vals) < menuLimit {
			vals = append(vals, suggestion{"/" + name + " " + val, val, desc, false})
		}
	}
	switch name {
	case "arm":
		if t := m.taskByName(m.set.Task); t != nil && len(t.Arms) > 0 {
			for _, a := range t.Arms {
				if a.Arm != "" {
					add(a.Arm, armDescs[a.Arm])
				}
			}
			if len(t.Arms) > 2 {
				add("both", "all arms")
				add("all", "all arms")
			} else if len(t.Arms) == 2 {
				add("both", "both arms")
			}
		} else {
			add("control", armDescs["control"])
			add("kavach", armDescs["kavach"])
			add("both", "both arms")
		}
	case "agent":
		add("opencode", "OpenCode with the chosen model")
		add("oracle", "applies the task's known fix; no model, no cost")
		add("nop", "does nothing; the hidden test should fail")
	case "env":
		add("docker", "containers on this machine")
		for _, p := range harbor.Providers {
			add(p.Name, "needs "+p.Need())
		}
	case "task":
		for _, t := range m.tasks {
			add(t.Name, t.Title)
		}
	case "model":
		for _, mod := range m.models {
			if mod.Tools {
				add(mod.ID, fmt.Sprintf("%s · in %s · out %s", displayName(mod), price(mod.PriceIn), price(mod.PriceOut)))
			}
		}
	case "show":
		for _, t := range m.order() {
			if t.ID != "" {
				add(t.ID, m.attemptName(t)+" · "+t.Status)
			}
		}
	}
	if len(vals) == 1 && vals[0].value == strings.TrimSpace(v) {
		return nil
	}
	return vals
}

// exec runs one prompt line.
func (m *Model) exec(line string) tea.Cmd {
	if line == "" {
		return nil
	}
	m.note = ""
	if !strings.HasPrefix(line, "/") {
		m.fail("Commands start with /. Type / to see them.")
		return nil
	}
	name, arg, _ := strings.Cut(line[1:], " ")
	for _, c := range commands {
		if c.name == name {
			arg = strings.TrimSpace(arg)
			if c.needsArg && arg == "" {
				m.fail("/%s needs %s.", c.name, c.args)
				return nil
			}
			return c.run(m, arg)
		}
	}
	m.fail("There is no /%s. Type / to see the commands.", name)
	return nil
}

func (m *Model) needsModel() bool { return m.set.Agent == "" || m.set.Agent == "opencode" }

// runTasks are the Harbor tasks /run starts: the chosen task in the chosen
// arms.
func (m *Model) runTasks(arm string) ([]string, error) {
	if !m.tasksLoaded {
		return nil, fmt.Errorf("the task list hasn't loaded yet; try again in a moment")
	}
	if m.tasksErr != "" {
		return nil, fmt.Errorf("couldn't list the tasks: %s", m.tasksErr)
	}
	if len(m.tasks) == 0 {
		return nil, fmt.Errorf("no tasks found under %s", m.tasksDir())
	}
	t := m.taskByName(m.set.Task)
	if t == nil {
		return nil, fmt.Errorf("there is no task %q; pick one with /task", m.set.Task)
	}
	var out []string
	for _, a := range t.Arms {
		if arm == "both" || arm == "all" || a.Arm == arm || a.Arm == "" {
			out = append(out, a.Task)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no %s arm", t.Name, arm)
	}
	return out, nil
}

func (m *Model) tasksDir() string {
	if m.status != nil {
		return m.status.TasksDir
	}
	return "bench/agent/harbor/tasks"
}

func (m *Model) start(tasks, models []string) tea.Cmd {
	if m.status != nil && !m.status.Docker.OK && m.local(m.set.Env) {
		m.fail("Docker isn't running: start Colima with `colima start`.")
		return nil
	}
	if m.status != nil && m.status.JobsWarning != "" && m.local(m.set.Env) {
		m.fail("%s", m.status.JobsWarning)
		return nil
	}
	if p := m.envProblem(m.set.Env); p != "" {
		m.fail("%s.", p)
		return nil
	}
	req := harbor.RunRequest{Tasks: tasks, Attempts: m.set.Runs, Agent: m.set.Agent, TimeoutSec: float64(m.set.Timeout * 60), Retries: m.set.Retries, Environment: m.set.Env}
	if m.needsModel() {
		for _, id := range models {
			req.Models = append(req.Models, "openrouter/"+id)
		}
	}
	m.ok("Starting Harbor…")
	return func() tea.Msg { j, err := m.c.start(m.ctx, req); return startedMsg{j, err} }
}

func (m *Model) cmdRun(string) tea.Cmd {
	tasks, err := m.runTasks(m.set.Arm)
	if err != nil {
		m.fail("Can't run: %v.", err)
		return nil
	}
	if m.needsModel() && m.set.Model == "" {
		m.fail("Choose a model first: /model <OpenRouter id>, or /models to browse.")
		return nil
	}
	return m.start(tasks, []string{m.set.Model})
}

func (m *Model) cmdMatrix(arg string) tea.Cmd {
	if !m.needsModel() {
		m.fail("/matrix compares models; the %s agent has none. Use /run, or /agent opencode.", m.set.Agent)
		return nil
	}
	if ids := strings.Fields(arg); len(ids) > 0 {
		return m.startMatrix(ids)
	}
	m.pick = picker{chosen: map[string]bool{}}
	for _, id := range append([]string{m.set.Model}, m.set.Matrix...) {
		if id != "" {
			m.pick.chosen[id] = true
		}
	}
	m.showPage(pageMatrix, "/matrix")
	return m.wantModels()
}

func (m *Model) startMatrix(ids []string) tea.Cmd {
	tasks, err := m.runTasks("both")
	if err != nil {
		m.fail("Can't run: %v.", err)
		return nil
	}
	m.set.Matrix = ids
	m.save()
	return m.start(tasks, ids)
}

// wantModels fetches the model list unless it is here.
func (m *Model) wantModels() tea.Cmd {
	if m.models != nil {
		return nil
	}
	return m.loadModels()
}

func (m *Model) cmdModel(arg string) tea.Cmd {
	id := strings.TrimPrefix(arg, "openrouter/")
	m.set.Model, m.set.Name = id, id
	if mod := m.modelByID(id); mod != nil {
		m.set.Name = displayName(*mod)
		if !mod.Tools {
			m.fail("%s doesn't support tool calls on OpenRouter; the agent needs them.", id)
		}
	} else if m.models != nil {
		m.fail("OpenRouter doesn't list %s; /models <filter> searches its list.", id)
	}
	if m.set.Agent != "opencode" {
		m.set.Agent = "opencode"
	}
	m.save()
	return m.wantModels()
}

func (m *Model) cmdModels(arg string) tea.Cmd {
	m.filter = arg
	m.showPage(pageModels, strings.TrimSpace("/models "+arg))
	return m.wantModels()
}

func (m *Model) cmdArm(arg string) tea.Cmd {
	switch arg {
	case "control", "workflow", "kavach", "kavach-implement", "both", "all":
		m.set.Arm = arg
		m.save()
	default:
		m.fail("/arm takes control, workflow, kavach, kavach-implement, both or all.")
	}
	return nil
}

func (m *Model) cmdAgent(arg string) tea.Cmd {
	m.set.Agent = arg
	m.save()
	return nil
}

func (m *Model) cmdEnv(arg string) tea.Cmd {
	if arg != "docker" && !slices.ContainsFunc(harbor.Providers, func(p harbor.Provider) bool { return p.Name == arg }) {
		m.fail("/env takes docker, daytona, modal or vercel.")
		return nil
	}
	m.set.Env = arg
	m.save()
	if p := m.envProblem(arg); p != "" {
		m.fail("Saved, but %s.", p)
	}
	return nil
}

// local is whether env runs in Docker on this machine.
func (m *Model) local(env string) bool { return env == "" || env == "docker" }

// envProblem is why a run on env cannot start, from the server's status.
func (m *Model) envProblem(env string) string {
	if m.status == nil {
		return ""
	}
	for _, e := range m.status.Environments {
		if e.Name == env && !e.Ready {
			return e.Problem
		}
	}
	return ""
}

func (m *Model) cmdRuns(arg string) tea.Cmd {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 1 || n > 100 {
		m.fail("/runs takes a number from 1 to 100.")
		return nil
	}
	m.set.Runs = n
	m.save()
	return nil
}

func (m *Model) cmdTask(arg string) tea.Cmd {
	if m.taskByName(arg) == nil {
		var names []string
		for _, t := range m.tasks {
			names = append(names, t.Name)
		}
		if len(names) == 0 {
			m.fail("No tasks found under %s.", m.tasksDir())
		} else {
			m.fail("There is no task %q. Tasks: %s.", arg, strings.Join(names, ", "))
		}
		return nil
	}
	m.set.Task = arg
	m.save()
	return nil
}

func (m *Model) cmdTimeout(arg string) tea.Cmd {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 0 {
		m.fail("/timeout takes minutes; 0 keeps the task's own timeout.")
		return nil
	}
	m.set.Timeout = n
	m.save()
	return nil
}

func (m *Model) cmdRetries(arg string) tea.Cmd {
	n, err := strconv.Atoi(arg)
	if err != nil || n < 0 || n > 5 {
		m.fail("/retries takes 0 to 5: how many times an attempt that ends with an error runs again.")
		return nil
	}
	m.set.Retries = n
	m.save()
	return nil
}

func (m *Model) cmdJobs(string) tea.Cmd {
	m.jobSel = 0
	m.showPage(pageJobs, "/jobs")
	if len(m.jobs) == 0 {
		m.fail("No runs yet. Start one with /run.")
	}
	return m.loadJobs()
}

// currentJob is the job on screen, or the newest one.
func (m *Model) currentJob() string {
	if m.job != nil {
		return m.job.ID
	}
	if len(m.jobs) > 0 {
		return m.jobs[0].ID
	}
	return ""
}

func (m *Model) cmdResults(string) tea.Cmd { return m.resultsPage(pageResults, "/results") }
func (m *Model) cmdCompare(string) tea.Cmd { return m.resultsPage(pageCompare, "/compare") }

func (m *Model) resultsPage(p page, echo string) tea.Cmd {
	id := m.currentJob()
	if id == "" {
		m.fail("No runs yet. Start one with /run.")
		return nil
	}
	m.showPage(p, echo)
	if m.job != nil && m.job.ID == id {
		return m.gotJob(*m.job)
	}
	return m.loadJob(id)
}

func (m *Model) cmdShow(arg string) tea.Cmd {
	if arg == "" {
		if t := m.selected(); t != nil {
			return m.show(t)
		}
		m.fail("Open a run first (/jobs), then pick an attempt, or give /show a trial id.")
		return nil
	}
	if m.job != nil {
		for i := range m.job.Trials {
			if m.job.Trials[i].ID == arg {
				return m.show(&m.job.Trials[i])
			}
		}
	}
	m.fail("There is no attempt %s in this run.", arg)
	return nil
}

func (m *Model) cmdStop(string) tea.Cmd {
	if m.job == nil {
		m.fail("Open a run first: /jobs.")
		return nil
	}
	return m.askStop(*m.job)
}

func (m *Model) cmdPause(string) tea.Cmd {
	if m.job == nil {
		m.fail("Open a run first: /jobs.")
		return nil
	}
	return m.askPause(*m.job)
}

func (m *Model) cmdResume(string) tea.Cmd {
	if m.job == nil {
		m.fail("Open a run first: /jobs.")
		return nil
	}
	return m.resume(*m.job)
}

func (m *Model) cmdRetry(arg string) tea.Cmd {
	if m.job == nil {
		m.fail("Open a run first: /jobs.")
		return nil
	}
	t := m.selected()
	if arg != "" {
		t = nil
		for i := range m.job.Trials {
			if m.job.Trials[i].ID == arg {
				t = &m.job.Trials[i]
			}
		}
		if t == nil {
			m.fail("There is no attempt %s in this run.", arg)
			return nil
		}
	}
	if t == nil {
		m.fail("Pick an attempt, or give /retry a trial id.")
		return nil
	}
	return m.askRetry(*m.job, *t)
}

func (m *Model) cmdClear(string) tea.Cmd {
	m.job, m.selKey, m.compare = nil, "", nil
	m.showPage(pageHome, "")
	return nil
}

func (m *Model) cmdHelp(string) tea.Cmd {
	m.showPage(pageHelp, "/help")
	return nil
}

func (m *Model) cmdQuit(string) tea.Cmd { return m.quit() }
