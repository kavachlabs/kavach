package tui

import (
	"os/exec"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// show opens an attempt in OpenCode's own terminal UI: attached to its
// server while it runs, or imported from its export once it has finished.
// The console is suspended until OpenCode exits.
func (m *Model) show(t *harbor.Trial) tea.Cmd {
	if t == nil {
		return nil
	}
	cmd, why := openCommand(*t)
	if cmd == nil {
		m.fail("%s", why)
		return nil
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		m.fail("OpenCode isn't installed: see https://opencode.ai/docs to install it.")
		return nil
	}
	run := func(*Model) tea.Cmd {
		return tea.ExecProcess(cmd, func(err error) tea.Msg { return openedMsg{err} })
	}
	if t.Status != harbor.Running {
		return run(m)
	}
	m.confirm = &confirm{
		text: "Attach to the live attempt? Anything you type in OpenCode goes to the agent and spoils the attempt.",
		choices: []choice{
			{"enter", "attach", run},
			{"esc", "cancel", func(*Model) tea.Cmd { return nil }},
		},
	}
	return nil
}

// openCommand is the command that opens t in OpenCode, or why it can't be
// opened.
func openCommand(t harbor.Trial) (*exec.Cmd, string) {
	oc := t.OpenCode
	switch {
	case t.Status == harbor.Queued:
		return nil, "This attempt hasn't started yet."
	case t.Status == harbor.Running && oc != nil && oc.Finished == nil && oc.URL != "":
		args := []string{"attach", oc.URL}
		if oc.Session != "" {
			args = append(args, "--session", oc.Session)
		}
		return exec.Command("opencode", args...), ""
	case oc != nil && oc.Export != "":
		// Import and open from the same directory: OpenCode files a session
		// under the project of the directory it runs in.
		script := `opencode import "$1" >/dev/null && exec opencode --session "$2"`
		return exec.Command("sh", "-c", script, "sh", oc.Export, oc.Session), ""
	case t.Status == harbor.Running && oc == nil:
		return nil, "This attempt's OpenCode server hasn't announced itself yet (no agent/opencode-live.json). Try again in a moment; agents other than opencode never do."
	case t.Status == harbor.Running:
		return nil, "This attempt's OpenCode server has stopped and its session export isn't written yet. Try again when the attempt finishes."
	case oc == nil:
		return nil, "This attempt has no OpenCode session: it ran without the live hook (no agent/opencode-live.json), or with an agent other than opencode."
	}
	return nil, "This attempt has no session export (agent/opencode-session.json), so OpenCode can't open it."
}
