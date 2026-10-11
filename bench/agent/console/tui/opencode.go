package tui

import (
	"os/exec"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// show opens an attempt in OpenCode's own terminal UI: attached to its
// read-only view while it runs, or imported from its export once it has
// finished. The console is suspended until OpenCode exits.
func (m *Model) show(t *harbor.Trial) tea.Cmd {
	if t == nil {
		return nil
	}
	root := ""
	if m.status != nil {
		root = m.status.Root
	}
	cmd, why := openCommand(*t, root)
	if cmd == nil {
		m.fail("%s", why)
		return nil
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		m.fail("OpenCode isn't installed: see https://opencode.ai/docs to install it.")
		return nil
	}
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return openedMsg{err} })
}

// openCommand is the command that opens t in OpenCode, or why it can't be
// opened. root is the kavach repository, whose show.sh opens a finished
// attempt.
func openCommand(t harbor.Trial, root string) (*exec.Cmd, string) {
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
	case oc != nil && oc.Export != "" && root == "":
		return nil, "The API server didn't say where the kavach repository is, so its show.sh can't open this attempt."
	case oc != nil && oc.Export != "":
		// show.sh imports into its own OpenCode data folder, keeping
		// benchmark sessions out of yours.
		trial := filepath.Dir(filepath.Dir(oc.Export))
		return exec.Command(filepath.Join(root, "bench", "agent", "harbor", "show.sh"), trial), ""
	case t.Status == harbor.Running && oc == nil:
		return nil, "This attempt's OpenCode server hasn't announced itself yet (no agent/opencode-live.json). Try again in a moment; agents other than opencode never do."
	case t.Status == harbor.Running:
		return nil, "This attempt's OpenCode server has stopped and its session export isn't written yet. Try again when the attempt finishes."
	case oc == nil:
		return nil, "This attempt has no OpenCode session: it ran without the live hook (no agent/opencode-live.json), or with an agent other than opencode."
	}
	return nil, "This attempt has no session export (agent/opencode-session.json), so OpenCode can't open it."
}
