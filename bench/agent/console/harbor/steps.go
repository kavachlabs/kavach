package harbor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	// Running is set on a tool call that has not ended, which only the
	// session export shows; Detail is then the end of its output so far.
	Running bool `json:"running,omitempty"`

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
// fills while it runs in a local sandbox, or else from what a remote
// sandbox's run mirrors: the live copy of that log, or the session export.
// A trial with none has no steps.
func readSteps(dir string) ([]Step, error) { return stepsFrom(os.DirFS(dir)) }

// stepsFrom reads steps from fsys, which holds a trial directory's files.
func stepsFrom(fsys fs.FS) ([]Step, error) {
	b, err := fs.ReadFile(fsys, "agent/trajectory.json")
	if err == nil {
		return atifSteps(b)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := fsys.Open("agent/opencode.txt")
	if errors.Is(err, os.ErrNotExist) {
		// A remote sandbox's run: the agent's live copy of the log, or else
		// its session export.
		f, err = fsys.Open("agent/opencode-live.txt")
	}
	var lg openCodeLog
	switch {
	case errors.Is(err, os.ErrNotExist):
		lg, err = exportLog(fsys)
	case err != nil:
	default:
		defer f.Close()
		lg, err = readOpenCodeLog(f)
	}
	if err != nil {
		return nil, err
	}
	return withRunning(fsys, lg), nil
}

// maxRunningOutput bounds the output shown of a call still running.
const maxRunningOutput = 2000

// withRunning adds to a live attempt's steps the tool calls its session
// export shows running and the steps do not hold as ended: OpenCode logs a
// call only when it ends, so a long command would show nothing until then.
// They come last, in the export's order, at the call's start but never
// before the step before.
func withRunning(fsys fs.FS, lg openCodeLog) []Step {
	steps := lg.steps
	if !liveTrial(fsys) {
		return steps
	}
	b, err := fs.ReadFile(fsys, "agent/opencode-session.json")
	if err != nil {
		return steps
	}
	var export struct {
		Messages []struct {
			Info struct {
				Role string `json:"role"`
			} `json:"info"`
			Parts []struct {
				ID    string `json:"id"`
				Type  string `json:"type"`
				Tool  string `json:"tool"`
				State struct {
					Status   string         `json:"status"`
					Input    map[string]any `json:"input"`
					Metadata map[string]any `json:"metadata"`
					Time     struct {
						Start int64 `json:"start"`
					} `json:"time"`
				} `json:"state"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if json.Unmarshal(b, &export) != nil {
		return steps
	}
	t0 := lg.t0
	var last int64
	if len(steps) > 0 {
		last = steps[len(steps)-1].TMs
	}
	for _, m := range export.Messages {
		if m.Info.Role != "assistant" {
			continue
		}
		for _, p := range m.Parts {
			if p.Type != "tool" || p.State.Status != "running" || (p.ID != "" && lg.ended[p.ID]) {
				continue
			}
			s := toolStep(p.Tool, p.State.Input, "", nil)
			s.Running, s.Exit, s.Verdict, s.Passed, s.Contract = true, nil, "", nil, nil
			s.Detail = tailLines(str(p.State.Metadata["output"]), maxRunningOutput)
			if start := p.State.Time.Start; start > 0 {
				if t0 == 0 {
					t0 = start
				}
				last = max(last, start-t0)
			}
			s.TMs = last
			steps = append(steps, s)
		}
	}
	return steps
}

// liveTrial reports whether a trial's agent may still be running: it has no
// result, and the agent has not marked its live file finished.
func liveTrial(fsys fs.FS) bool {
	if b, err := fs.ReadFile(fsys, "result.json"); err == nil && len(b) > 0 && string(b) != "null" {
		return false
	}
	var live liveFile
	return readJSONFS(fsys, "agent/opencode-live.json", &live) != nil || live.Finished == ""
}

// tailLines is the end of s, at most n bytes, starting at a line, or the
// last n bytes when its last line is longer.
func tailLines(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i+1 < len(cut) {
		return cut[i+1:]
	}
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return cut
}

// exportLog reads steps from the session export the opencode-live agent
// mirrors every few seconds. A sandbox without a bind mount (Daytona, Modal)
// sends Harbor opencode.txt only when the agent ends, so while it runs the
// export is all there is. Its assistant messages hold the same parts the
// event log does; they are written out as the log's events and read as one.
func exportLog(fsys fs.FS) (openCodeLog, error) {
	b, err := fs.ReadFile(fsys, "agent/opencode-session.json")
	if errors.Is(err, fs.ErrNotExist) {
		return openCodeLog{steps: []Step{}}, nil
	}
	if err != nil {
		return openCodeLog{}, err
	}
	var export struct {
		Messages []struct {
			Info struct {
				Role string `json:"role"`
				Time struct {
					Created int64 `json:"created"`
				} `json:"time"`
			} `json:"info"`
			Parts []json.RawMessage `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &export); err != nil {
		// The export is replaced atomically, so this is not a partial write.
		return openCodeLog{}, fmt.Errorf("opencode-session.json: %w", err)
	}
	var log bytes.Buffer
	var last int64
	for _, m := range export.Messages {
		if m.Info.Role != "assistant" {
			continue // the user's message is the instruction, not a step
		}
		for _, raw := range m.Parts {
			var p struct {
				Type string `json:"type"`
				Time struct {
					Start int64 `json:"start"`
				} `json:"time"`
				State struct {
					Status string `json:"status"`
					Time   struct {
						Start int64 `json:"start"`
					} `json:"time"`
				} `json:"state"`
			}
			if json.Unmarshal(raw, &p) != nil {
				continue
			}
			// Steps never go back in time: a part without a time of its own
			// (step-finish) is at the part before it, and so is a tool call
			// the model began while the part before it still streamed.
			ts := max(p.Time.Start, p.State.Time.Start, m.Info.Time.Created, last)
			last = ts
			var typ string
			switch p.Type {
			case "text":
				typ = "text"
			case "tool":
				// The log has a tool call once it ends; one still running
				// shows when it does.
				if p.State.Status != "completed" && p.State.Status != "error" {
					continue
				}
				typ = "tool_use"
			case "step-finish":
				typ = "step_finish"
			default:
				continue
			}
			line, err := json.Marshal(struct {
				Type      string          `json:"type"`
				Timestamp int64           `json:"timestamp"`
				Part      json.RawMessage `json:"part"`
			}{typ, ts, raw})
			if err != nil {
				return openCodeLog{}, err
			}
			log.Write(append(line, '\n'))
		}
	}
	return readOpenCodeLog(&log)
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
		ID    string `json:"id"`
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
	lg, err := readOpenCodeLog(r)
	return lg.steps, err
}

// openCodeLog is what an event log gives: its steps, the timestamp they
// count from, and the ids of the tool calls that ended.
type openCodeLog struct {
	steps []Step
	t0    int64
	ended map[string]bool
}

func readOpenCodeLog(r io.Reader) (openCodeLog, error) {
	steps := []Step{}
	ended := map[string]bool{}
	br := bufio.NewReader(r)
	var t0 int64
	calls := 0
	for {
		line, err := br.ReadBytes('\n')
		if err == io.EOF {
			return openCodeLog{steps, t0, ended}, nil
		}
		if err != nil {
			return openCodeLog{}, err
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
			if p.ID != "" {
				ended[p.ID] = true
			}
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

// agentDiff is the verdict of the last kavach diff among steps that printed
// one.
func agentDiff(steps []Step) string {
	v := ""
	for _, s := range steps {
		if isDiff(s) && s.Verdict != "" {
			v = s.Verdict
		}
	}
	return v
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
