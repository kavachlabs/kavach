package harbor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Step kinds.
const (
	KindText   = "text"
	KindShell  = "shell"
	KindRead   = "read"
	KindSearch = "search"
	KindEdit   = "edit"
	KindMCP    = "mcp" // an MCP tool, or any tool not listed above
	KindError  = "error"
	KindUsage  = "usage" // the end of one model call, with its tokens
)

// Step is one line of an attempt as a console shows it.
type Step struct {
	TMs    int64    `json:"t_ms"` // since the attempt's first event
	Kind   string   `json:"kind"`
	Title  string   `json:"title"`
	Detail string   `json:"detail,omitempty"`
	Tool   string   `json:"tool,omitempty"`
	Exit   *int     `json:"exit,omitempty"` // a shell command's exit status, when known
	Tokens *Tokens  `json:"tokens,omitempty"`
	Cost   *float64 `json:"cost,omitempty"`

	// For Kavach's MCP tools (named kavach_*) and the kavach CLI run in a
	// shell. Verdict and Passed are set when the output carries a verdict:
	// JSON with a status or verdict, or the report of the CLI's replay or
	// diff.
	Kavach   bool   `json:"kavach,omitempty"`
	Verdict  string `json:"verdict,omitempty"`
	Passed   *bool  `json:"passed,omitempty"`
	Variants string `json:"variants,omitempty"`
	// Contract is the report of a contract check: kavach_contract_check, or
	// `kavach contract check --json` run in a shell.
	Contract *ContractCheck `json:"contract,omitempty"`
}

// ContractCheck is what a console shows of a contract check's report
// (contract.Report): the round, the verdict, which feature steps changed,
// which invariants the build declares, and how its variants fared by kind.
type ContractCheck struct {
	Round     int      `json:"round"`
	Streak    int      `json:"streak"`
	Verdict   string   `json:"verdict"`
	Passed    bool     `json:"passed"`
	Detail    string   `json:"detail,omitempty"`
	Features  []uint64 `json:"feature_steps"`
	Changed   []uint64 `json:"changed"` // the seqs of the feature steps whose outputs changed
	Unreached []uint64 `json:"unreached"`

	Invariants        []string `json:"invariants,omitempty"`
	Declared          []string `json:"declared,omitempty"`
	MissingInvariants []string `json:"missing_invariants,omitempty"`

	VariantCandidates   int                   `json:"variant_candidates"`
	VariantsPassed      int                   `json:"variants_passed"`
	VariantsPreexisting int                   `json:"variants_preexisting"`
	VariantKinds        map[string]KindCount  `json:"variant_kinds,omitempty"`
	VariantFailures     []ContractVariantFail `json:"variant_failures"`
}

// KindCount counts the variants of one class of mutation.
type KindCount struct {
	Candidates  int `json:"candidates"`
	Passed      int `json:"passed"`
	Preexisting int `json:"preexisting"`
}

// ContractVariantFail is a variant the new build failed.
type ContractVariantFail struct {
	ID       int     `json:"id"`
	Kind     string  `json:"kind,omitempty"`
	Mutation string  `json:"mutation"`
	Feature  uint64  `json:"feature_seq"`
	Verdict  string  `json:"verdict"`
	Seq      *uint64 `json:"seq,omitempty"`
	Detail   string  `json:"detail,omitempty"`
	// File is where the check saved the variant, inside the task's
	// container; the next check replaces it.
	File string `json:"file,omitempty"`
}

// maxFailText bounds a variant failure's mutation and detail.
const maxFailText = 600

// contractCheck reads a contract check's report from a tool's output: the
// JSON object kavach_contract_check and `kavach contract check --json`
// print, possibly after other output. It returns nil for anything else.
func contractCheck(out string) *ContractCheck {
	i := strings.Index(out, "{")
	if i < 0 {
		return nil
	}
	var v struct {
		Report *struct {
			ContractCheck
			Changed []struct {
				Seq uint64 `json:"seq"`
			} `json:"changed"`
		} `json:"report"`
	}
	if json.NewDecoder(strings.NewReader(out[i:])).Decode(&v) != nil || v.Report == nil {
		return nil
	}
	c := v.Report.ContractCheck
	c.Changed = make([]uint64, 0, len(v.Report.Changed))
	for _, ch := range v.Report.Changed {
		c.Changed = append(c.Changed, ch.Seq)
	}
	for i := range c.VariantFailures {
		f := &c.VariantFailures[i]
		f.Mutation, f.Detail = clipTo(f.Mutation, maxFailText), clipTo(f.Detail, maxFailText)
	}
	c.Detail = clipTo(c.Detail, maxFailText)
	return &c
}

// maxText bounds a step's title and detail: tool output can be whole files.
const maxText = 4000

// readSteps reads a trial's steps from Harbor's ATIF trajectory, which the
// agent writes when it finishes, or else from OpenCode's event log, which
// fills while it runs. A trial with neither has no steps.
func readSteps(dir string) ([]Step, error) {
	b, err := os.ReadFile(filepath.Join(dir, "agent", "trajectory.json"))
	if err == nil {
		return atifSteps(b)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, "agent", "opencode.txt"))
	if errors.Is(err, os.ErrNotExist) {
		return []Step{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return openCodeSteps(f)
}

// atifSteps converts an ATIF trajectory (Harbor's agent-neutral format).
// ATIF has one timestamp per model call, so a call's tools share it.
func atifSteps(b []byte) ([]Step, error) {
	var traj struct {
		Steps []struct {
			Timestamp string `json:"timestamp"`
			Source    string `json:"source"`
			Message   any    `json:"message"`
			ToolCalls []struct {
				ID        string         `json:"tool_call_id"`
				Name      string         `json:"function_name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"tool_calls"`
			Observation *struct {
				Results []struct {
					CallID  string `json:"source_call_id"`
					Content any    `json:"content"`
				} `json:"results"`
			} `json:"observation"`
			Metrics *struct {
				Prompt     int64          `json:"prompt_tokens"` // includes the cached tokens
				Completion int64          `json:"completion_tokens"`
				Cached     int64          `json:"cached_tokens"`
				Cost       *float64       `json:"cost_usd"`
				Extra      map[string]any `json:"extra"`
			} `json:"metrics"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(b, &traj); err != nil {
		return nil, fmt.Errorf("trajectory.json: %w", err)
	}
	steps := []Step{}
	var t0 time.Time
	calls := 0
	for _, s := range traj.Steps {
		if s.Source != "agent" {
			continue
		}
		var ms int64
		if t := parseTime(s.Timestamp); t != nil {
			if t0.IsZero() {
				t0 = *t
			}
			ms = t.Sub(t0).Milliseconds()
		}
		if text := strings.TrimSpace(contentText(s.Message)); text != "" {
			steps = append(steps, Step{TMs: ms, Kind: KindText, Title: clip(text)})
		}
		outputs := map[string]string{}
		if s.Observation != nil {
			for _, r := range s.Observation.Results {
				outputs[r.CallID] = contentText(r.Content)
			}
		}
		for _, c := range s.ToolCalls {
			st := toolStep(c.Name, c.Arguments, outputs[c.ID], nil)
			st.TMs = ms
			steps = append(steps, st)
		}
		if m := s.Metrics; m != nil {
			calls++
			reasoning, _ := m.Extra["reasoning_tokens"].(float64)
			tok := Tokens{Input: m.Prompt - m.Cached, Cached: m.Cached, Output: m.Completion, Reasoning: int64(reasoning)}
			steps = append(steps, Step{TMs: ms, Kind: KindUsage, Title: fmt.Sprintf("step %d", calls), Tokens: &tok, Cost: m.Cost})
		}
	}
	return steps, nil
}

// contentText is the text of an ATIF message or observation, which is a
// string or a list of content parts.
func contentText(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// openCodeEvent is one line of `opencode run --format json`.
type openCodeEvent struct {
	Type      string `json:"type"`
	Timestamp int64  `json:"timestamp"`
	Part      struct {
		Text  string `json:"text"`
		Tool  string `json:"tool"`
		State struct {
			Status   string         `json:"status"`
			Input    map[string]any `json:"input"`
			Output   string         `json:"output"`
			Error    any            `json:"error"`
			Metadata map[string]any `json:"metadata"`
		} `json:"state"`
		Cost   *float64 `json:"cost"`
		Tokens struct {
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			Reasoning int64 `json:"reasoning"`
			Cache     struct {
				Read int64 `json:"read"`
			} `json:"cache"`
		} `json:"tokens"`
	} `json:"part"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// openCodeSteps converts OpenCode's event log. Harbor's agent tees OpenCode's
// stdout and stderr into one file, so lines that are not JSON events are
// skipped, as is a last line still being written.
func openCodeSteps(r io.Reader) ([]Step, error) {
	steps := []Step{}
	br := bufio.NewReader(r)
	var t0 int64
	calls := 0
	for {
		line, err := br.ReadBytes('\n')
		if err == io.EOF {
			return steps, nil
		}
		if err != nil {
			return nil, err
		}
		var e openCodeEvent
		if !bytes.HasPrefix(line, []byte("{")) || json.Unmarshal(line, &e) != nil {
			continue
		}
		if t0 == 0 {
			t0 = e.Timestamp
		}
		ms := e.Timestamp - t0
		p := e.Part
		switch e.Type {
		case "text":
			if text := strings.TrimSpace(p.Text); text != "" {
				steps = append(steps, Step{TMs: ms, Kind: KindText, Title: clip(text)})
			}
		case "tool_use":
			var s Step
			if p.State.Status == "error" {
				s = Step{Kind: KindError, Tool: p.Tool, Title: clip(p.Tool + ": " + fmt.Sprint(p.State.Error))}
			} else {
				s = toolStep(p.Tool, p.State.Input, p.State.Output, p.State.Metadata)
			}
			s.TMs = ms
			steps = append(steps, s)
		case "step_finish":
			calls++
			tok := Tokens{Input: p.Tokens.Input, Cached: p.Tokens.Cache.Read, Output: p.Tokens.Output, Reasoning: p.Tokens.Reasoning}
			steps = append(steps, Step{TMs: ms, Kind: KindUsage, Title: fmt.Sprintf("step %d", calls), Tokens: &tok, Cost: p.Cost})
		case "error":
			title := e.Error.Name
			if e.Error.Data.Message != "" {
				title += ": " + e.Error.Data.Message
			}
			steps = append(steps, Step{TMs: ms, Kind: KindError, Title: clip(title)})
		}
	}
}

// toolStep describes one tool call. meta is OpenCode's tool metadata (exit
// status, diff), which ATIF does not carry.
func toolStep(name string, in map[string]any, out string, meta map[string]any) Step {
	s := Step{Tool: name}
	switch name {
	case "bash":
		s.Kind, s.Title = KindShell, str(in["command"])
		s.Detail = out
		if o := str(meta["output"]); o != "" {
			s.Detail = o
		}
		if code, ok := meta["exit"].(float64); ok {
			c := int(code)
			s.Exit = &c
		}
		if m := kavachCommand(s.Title); m != nil {
			s.Kavach = true
			switch {
			case strings.Contains(s.Title, "kavach contract check"):
				if c := contractCheck(s.Detail); c != nil {
					s.Contract, s.Verdict = c, c.Verdict
					s.Passed = &c.Passed
				}
			case m[1] == "replay" || m[1] == "diff":
				cliVerdict(&s)
			}
		}
	case "read":
		s.Kind, s.Title = KindRead, str(in["filePath"])
		offset, _ := in["offset"].(float64)
		limit, _ := in["limit"].(float64)
		switch first := max(int(offset), 1); {
		case limit > 0:
			s.Title += fmt.Sprintf(" lines %d–%d", first, first+int(limit)-1)
		case offset > 0:
			s.Title += fmt.Sprintf(" from line %d", int(offset))
		}
	case "grep", "glob", "list":
		s.Kind, s.Title = KindSearch, str(in["pattern"])
		if s.Title == "" {
			s.Title = str(in["path"])
		}
		s.Detail = out
	case "edit", "write", "patch", "multiedit", "apply_patch":
		s.Kind, s.Title = KindEdit, str(in["filePath"])
		s.Detail = str(meta["diff"])
		if s.Detail == "" {
			s.Detail = editDiff(in)
		}
	default:
		s.Kind, s.Title, s.Detail = KindMCP, name, out
		if strings.HasPrefix(name, "kavach_") {
			kavachStep(&s, in, out)
		}
	}
	s.Title, s.Detail = clip(s.Title), clip(s.Detail)
	return s
}

// editDiff renders an edit's arguments as removed and added lines, for logs
// that do not carry the diff.
func editDiff(in map[string]any) string {
	var b strings.Builder
	for _, l := range lines(str(in["oldString"])) {
		b.WriteString("-" + l + "\n")
	}
	for _, l := range lines(str(in["newString"]) + str(in["content"])) {
		b.WriteString("+" + l + "\n")
	}
	return b.String()
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// kavachStep reads the verdict of a Kavach tool (kavach_replay, kavach_diff;
// OpenCode prefixes the MCP server's name: kavach_kavach_diff).
func kavachStep(s *Step, in map[string]any, out string) {
	s.Kavach = true
	parts := strings.SplitN(s.Tool, "_", 3)
	s.Title = "kavach " + parts[len(parts)-1]
	if arg := str(in["new"]) + str(in["bin"]); arg != "" {
		s.Title += " " + arg
	}
	if !kavachJSON(s, out) {
		return
	}
	if strings.HasSuffix(s.Tool, "contract_check") {
		s.Contract = contractCheck(out)
	}
}

// kavachJSON reads the verdict from a JSON report of a replay or diff, and
// says whether out was one.
func kavachJSON(s *Step, out string) bool {
	var r struct {
		Status   string `json:"status"`
		Verdict  string `json:"verdict"`
		Passed   *bool  `json:"passed"`
		Variants *struct {
			Candidates  int `json:"candidates"`
			Reproducing int `json:"reproducing"`
			Passing     int `json:"passing"`
		} `json:"variants"`
	}
	if json.Unmarshal([]byte(out), &r) != nil {
		return false
	}
	s.Verdict = r.Status
	if s.Verdict == "" {
		s.Verdict = r.Verdict
	}
	s.Passed = r.Passed
	if v := r.Variants; v != nil && v.Candidates > 0 {
		s.Variants = fmt.Sprintf("%d of %d variants reproduce on the old build · %d of those pass", v.Reproducing, v.Candidates, v.Passing)
	}
	return true
}

var (
	// kavachCLI matches a shell command that runs the kavach CLI, with or
	// without a path and global flags; the group is its subcommand.
	kavachCLI = regexp.MustCompile(`(?:^|[\s;&|(])(?:[\w./-]*/)?kavach(?:\s+--[\w-]+)*\s+(inspect|replay|diff|list|contract)\b`)
	// cliLine is a line of the CLI's report that names the verdict (replay
	// prints it as result) or the variants.
	cliLine = regexp.MustCompile(`^(verdict|result|variants)\s+(.+?)\s*$`)
	ansiEsc = regexp.MustCompile(`\x1b\[[0-9;]*m`)
)

// cliVerdict reads the verdict of the CLI's replay or diff from its output:
// the --json report, or the report it prints. Output piped elsewhere leaves
// none.
func cliVerdict(s *Step) {
	out := strings.TrimSpace(ansiEsc.ReplaceAllString(s.Detail, ""))
	if kavachJSON(s, out) {
		return
	}
	for _, l := range strings.Split(out, "\n") {
		m := cliLine.FindStringSubmatch(l)
		switch {
		case m == nil:
		case m[1] == "variants":
			s.Variants = m[2]
		default:
			s.Verdict = m[2]
		}
	}
	if s.Verdict != "" {
		p := s.Verdict == "fixed" || s.Verdict == "ok"
		s.Passed = &p
	}
}

// kavachCommand matches kavachCLI against the lines of a shell command that
// are not comments: agents mention the CLI in comments too.
func kavachCommand(cmd string) []string {
	for _, l := range strings.Split(cmd, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		if m := kavachCLI.FindStringSubmatch(l); m != nil {
			return m
		}
	}
	return nil
}

// isDiff says whether s is a `kavach diff`, as a tool or on the command line.
func isDiff(s Step) bool {
	if s.Kind == KindShell {
		m := kavachCommand(s.Title)
		return m != nil && m[1] == "diff"
	}
	return s.Kavach && strings.HasSuffix(s.Tool, "kavach_diff")
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// clip cuts s to maxText bytes on a rune boundary.
func clip(s string) string { return clipTo(s, maxText) }

// clipTo cuts s to n bytes on a rune boundary.
func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
