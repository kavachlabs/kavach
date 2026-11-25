package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var defaultSkillNames = []string{
	"kavach-fix",
	"kavach-implement",
	"kavach-invariants",
	"kavach-setup",
	"kavach-verify",
}

type skillInstallAction struct {
	Agent  string   `json:"agent"`
	Target string   `json:"target"`
	Skills []string `json:"skills"`
	Source string   `json:"source"`
	DryRun bool     `json:"dry_run"`
}

func cmdSkills(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("skills", stderr)
	agent := fs.String("agent", "", "target agent: claude, cursor, codex, antigravity, or all (default: auto-detect)")
	dirOverride := fs.String("dir", "", "explicit destination directory for installed skills")
	dryRun := fs.Bool("dry-run", false, "print planned skill installation without writing files")
	asJSON := fs.Bool("json", false, "print the result as JSON")

	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "kavach skills: unexpected positional arguments %v\n", pos)
		return exitUsage
	}

	targets, err := resolveSkillTargets(*agent, *dirOverride)
	if err != nil {
		fmt.Fprintf(stderr, "kavach skills: %v\n", err)
		return exitUsage
	}

	srcDir := findLocalSkillsSource()
	srcLabel := srcDir
	if srcLabel == "" {
		srcLabel = "builtin-template"
	}

	actions := make([]skillInstallAction, 0, len(targets))
	for _, t := range targets {
		skills := discoverSkillNames(srcDir)
		act := skillInstallAction{
			Agent:  t.agent,
			Target: t.dir,
			Skills: skills,
			Source: srcLabel,
			DryRun: *dryRun,
		}
		if !*dryRun {
			if err := installSkillsToDir(srcDir, t.dir, skills); err != nil {
				fmt.Fprintf(stderr, "kavach skills: %v\n", err)
				return exitError
			}
		}
		actions = append(actions, act)
	}

	if *asJSON {
		return writeJSON(stdout, stderr, map[string]any{
			"dry_run": *dryRun,
			"npx_cmd": "npx skills@latest add kavachlabs/kavach",
			"actions": actions,
		})
	}

	u.printBanner(stdout)
	u.header(stdout, "KAVACH SKILLS")
	for _, a := range actions {
		verb := "installed"
		if a.DryRun {
			verb = "would install"
		}
		fmt.Fprintf(stdout, "%s  %s (%d skills) -> %s\n",
			u.label(a.Agent, 12), verb, len(a.Skills), a.Target)
		fmt.Fprintf(stdout, "              %s\n", strings.Join(a.Skills, ", "))
	}
	fmt.Fprintf(stdout, "\n%s npx skills@latest add kavachlabs/kavach\n", u.paint("Or via npm:", ansiDim))
	return exitPass
}

type skillTarget struct {
	agent string
	dir   string
}

func resolveSkillTargets(agent, dirOverride string) ([]skillTarget, error) {
	if dirOverride != "" {
		abs, err := filepath.Abs(dirOverride)
		if err != nil {
			return nil, err
		}
		label := agent
		if label == "" {
			label = "custom"
		}
		return []skillTarget{{agent: label, dir: abs}}, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	claudeHome := os.Getenv("CLAUDE_CONFIG_DIR")
	if claudeHome == "" {
		claudeHome = filepath.Join(home, ".claude")
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	cursorHome := filepath.Join(home, ".cursor")
	agHome := filepath.Join(home, ".gemini", "config")

	byAgent := map[string]skillTarget{
		"claude":      {agent: "claude", dir: filepath.Join(claudeHome, "skills")},
		"cursor":      {agent: "cursor", dir: filepath.Join(cursorHome, "skills")},
		"codex":       {agent: "codex", dir: filepath.Join(codexHome, "skills")},
		"antigravity": {agent: "antigravity", dir: filepath.Join(agHome, "skills")},
	}

	a := strings.ToLower(strings.TrimSpace(agent))
	switch a {
	case "":
		var out []skillTarget
		for _, pair := range []struct {
			name string
			home string
		}{
			{"claude", claudeHome},
			{"cursor", cursorHome},
			{"codex", codexHome},
			{"antigravity", agHome},
		} {
			if st, err := os.Stat(pair.home); err == nil && st.IsDir() {
				out = append(out, byAgent[pair.name])
			}
		}
		if len(out) == 0 {
			out = append(out, byAgent["claude"])
		}
		return out, nil
	case "all":
		return []skillTarget{byAgent["claude"], byAgent["cursor"], byAgent["codex"], byAgent["antigravity"]}, nil
	default:
		t, ok := byAgent[a]
		if !ok {
			return nil, fmt.Errorf("unknown --agent %q (expected claude, cursor, codex, antigravity, or all)", agent)
		}
		return []skillTarget{t}, nil
	}
}

func findLocalSkillsSource() string {
	candidates := []string{}
	if env := os.Getenv("KAVACH_SKILLS_SRC"); env != "" {
		candidates = append(candidates, env)
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "skills"))
		candidates = append(candidates, filepath.Join(wd, "..", "..", "skills"))
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, "skills"), filepath.Join(dir, "..", "skills"))
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "kavach-setup", "SKILL.md")); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

func discoverSkillNames(srcDir string) []string {
	if srcDir == "" {
		return append([]string(nil), defaultSkillNames...)
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return append([]string(nil), defaultSkillNames...)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "kavach-") {
			continue
		}
		if st, err := os.Stat(filepath.Join(srcDir, e.Name(), "SKILL.md")); err == nil && !st.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return append([]string(nil), defaultSkillNames...)
	}
	sort.Strings(names)
	return names
}

func installSkillsToDir(srcDir, dstDir string, skills []string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	for _, name := range skills {
		targetSkill := filepath.Join(dstDir, name)
		if err := os.RemoveAll(targetSkill); err != nil {
			return err
		}
		if srcDir != "" {
			if err := copyTree(filepath.Join(srcDir, name), targetSkill); err != nil {
				return err
			}
			continue
		}
		// Fallback standalone skill stub when binary is run outside the kavach checkout.
		if err := os.MkdirAll(targetSkill, 0o755); err != nil {
			return err
		}
		stub := fmt.Sprintf("---\nname: %s\ndescription: Kavach deterministic replay and verification skill (%s).\n---\n\nRun `kavach init`, `kavach doctor`, `kavach map --auto`, and `kavach diff`.\nInstall full skill docs with: `npx skills@latest add kavachlabs/kavach --skill %s`\n", name, name, name)
		if err := os.WriteFile(filepath.Join(targetSkill, "SKILL.md"), []byte(stub), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	})
}

func cmdInstallMCP(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("install-mcp", stderr)
	target := fs.String("target", ".mcp.json", "MCP config target: .mcp.json, claude, or cursor")
	command := fs.String("command", "kavach", "kavach executable to register in MCP config")
	dryRun := fs.Bool("dry-run", false, "print planned MCP config update without writing files")
	asJSON := fs.Bool("json", false, "print the result as JSON")

	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) > 0 {
		fmt.Fprintf(stderr, "kavach install-mcp: unexpected positional arguments %v\n", pos)
		return exitUsage
	}

	destPath, err := resolveMCPTarget(*target)
	if err != nil {
		fmt.Fprintf(stderr, "kavach install-mcp: %v\n", err)
		return exitUsage
	}

	cfg := map[string]any{}
	if existing, err := os.ReadFile(destPath); err == nil && len(strings.TrimSpace(string(existing))) > 0 {
		if err := json.Unmarshal(existing, &cfg); err != nil {
			fmt.Fprintf(stderr, "kavach install-mcp: parse existing %s: %v\n", destPath, err)
			return exitError
		}
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok || servers == nil {
		servers = map[string]any{}
	}
	entry := map[string]any{
		"command": *command,
		"args":    []string{"mcp"},
	}
	servers["kavach"] = entry
	cfg["mcpServers"] = servers

	formatted, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "kavach install-mcp: %v\n", err)
		return exitError
	}
	formatted = append(formatted, '\n')

	if !*dryRun {
		if dir := filepath.Dir(destPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintf(stderr, "kavach install-mcp: %v\n", err)
				return exitError
			}
		}
		if err := os.WriteFile(destPath, formatted, 0o644); err != nil {
			fmt.Fprintf(stderr, "kavach install-mcp: %v\n", err)
			return exitError
		}
	}

	if *asJSON {
		return writeJSON(stdout, stderr, map[string]any{
			"target":  *target,
			"path":    destPath,
			"dry_run": *dryRun,
			"entry":   entry,
			"config":  cfg,
		})
	}

	u.printBanner(stdout)
	u.header(stdout, "KAVACH INSTALL-MCP")
	verb := "configured"
	if *dryRun {
		verb = "would configure"
	}
	fmt.Fprintf(stdout, "%s%s (%s)\n", u.label("target", 10), destPath, verb)
	fmt.Fprintf(stdout, "%s%s mcp\n", u.label("command", 10), *command)
	if *dryRun {
		fmt.Fprintf(stdout, "\n%s", string(formatted))
	}
	return exitPass
}

func resolveMCPTarget(target string) (string, error) {
	t := strings.TrimSpace(target)
	switch strings.ToLower(t) {
	case "", ".mcp.json":
		return ".mcp.json", nil
	case "claude":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".claude.json"), nil
	case "cursor":
		return filepath.Join(".cursor", "mcp.json"), nil
	default:
		if strings.HasSuffix(strings.ToLower(t), ".json") {
			return t, nil
		}
		return "", fmt.Errorf("unknown --target %q (expected .mcp.json, claude, cursor, or a *.json path)", target)
	}
}
