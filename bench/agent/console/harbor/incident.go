package harbor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kavachlabs/kavach/journal"
	"github.com/kavachlabs/kavach/replay"
)

// Incident is what a trial was checked against, decoded into steps: the
// task's fixture with the verifier's kavach diff of it and the old and new
// builds' replays, or, for a feature task, the contract's recorded traffic.
//
// The contract task graders save the traffic they checked as
// verifier/traffic.kavach, with each build's replay of it in
// replay-{old,new}.json (kavach contract check --save-replays). Before they
// did, a trial shows the traffic of the contract the agent collected, with
// no replays.
//
// The diff task graders save verifier/incident.kavach, kavach-diff.json and
// replay-{old,new}.json. Runs graded before they did have only the report
// the grader logged (grade.log), so Diff comes from that, the fixture from
// the task's source, and Old and New are null.
type Incident struct {
	Kind    string        `json:"kind"`    // "incident", or "traffic" for a contract's recorded traffic
	Fixture string        `json:"fixture"` // where it was read: verifier/incident.kavach, the task's source, the contract
	Service string        `json:"service"`
	Start   string        `json:"start"`
	Records int           `json:"records"`
	Steps   []VariantStep `json:"steps"`
	// Truncated and JournalTruncated are as in VariantReplay.
	Truncated        bool `json:"truncated,omitempty"`
	JournalTruncated bool `json:"journal_truncated,omitempty"`
	// Failure is the step the recording failed at: its marker.
	Failure *IncidentFailure `json:"failure"`
	Old     *VariantResult   `json:"old"`
	New     *VariantResult   `json:"new"`
	Diff    *IncidentDiff    `json:"diff"`
}

// IncidentFailure is the recorded failure: the seq of its input and its
// marker.
type IncidentFailure struct {
	Seq     uint64 `json:"seq"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// IncidentDiff is the verifier's kavach diff of the incident.
type IncidentDiff struct {
	Source     string      `json:"source"` // "json": kavach-diff.json; "log": the report in grade.log
	Verdict    string      `json:"verdict"`
	Old        string      `json:"old"`
	New        string      `json:"new"`
	Detail     string      `json:"detail,omitempty"`
	Divergence *Divergence `json:"divergence,omitempty"`
	// Variants and Regressions list every check from the JSON report. The
	// log has only their summary lines.
	Variants        *ProbeSet `json:"variants,omitempty"`
	Regressions     *ProbeSet `json:"regressions,omitempty"`
	VariantsLine    string    `json:"variants_line,omitempty"`
	RegressionsLine string    `json:"regressions_line,omitempty"`
}

// Divergence is the first step before the failure whose outputs on the new
// build differ from production's.
type Divergence struct {
	Seq    uint64 `json:"seq"`
	Output int    `json:"output"`
	Detail string `json:"detail,omitempty"`
}

// ProbeSet is one kind of check diff ran: the variants of the failing input,
// or the regression variants of earlier inputs like it.
type ProbeSet struct {
	Candidates  int     `json:"candidates"`
	Reproducing int     `json:"reproducing,omitempty"`
	Passing     int     `json:"passing"`
	Required    int     `json:"required,omitempty"`
	Counted     int     `json:"counted,omitempty"`
	Neighbours  int     `json:"neighbours,omitempty"`
	Checks      []Probe `json:"checks"`
}

// Probe is one variant: the mutation, each build's verdict on it, and
// whether it passed. Reproduces (variants) and Counted (regressions) say
// whether the old build's result makes it count.
type Probe struct {
	ID          int     `json:"id"`
	Mutation    string  `json:"mutation"`
	Old         string  `json:"old"`
	New         string  `json:"new,omitempty"`
	Passed      bool    `json:"passed"`
	Reproduces  *bool   `json:"reproduces,omitempty"`
	Counted     *bool   `json:"counted,omitempty"`
	IncidentSeq *uint64 `json:"incident_seq,omitempty"`
	InputSeq    *uint64 `json:"input_seq,omitempty"`
}

const maxProbes = 512

// TrialIncident decodes what a trial was checked against. A trial of a task
// with neither a fixture nor a collected contract has none.
func (r *Reader) TrialIncident(job, id string) (*Incident, error) {
	dir, err := r.trialDir(job, id)
	if err != nil {
		return nil, err
	}
	v := filepath.Join(dir, "verifier")
	inc := &Incident{Kind: "incident"}
	path := filepath.Join(v, "incident.kavach")
	inc.Fixture = "verifier/incident.kavach"
	if !exists(path) {
		path = taskFixture(dir)
		inc.Fixture = path
	}
	if path == "" {
		path = filepath.Join(v, "traffic.kavach")
		inc.Kind, inc.Fixture = "traffic", "verifier/traffic.kavach"
	}
	if inc.Kind == "traffic" && !exists(path) {
		path = filepath.Join(dir, filepath.FromSlash(contractSubdir), "traffic.kavach")
		inc.Fixture = contractSubdir + "/traffic.kavach"
		if !exists(path) {
			return nil, ErrNotFound
		}
	}
	j, err := journal.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", inc.Fixture, err)
	}
	inc.Service, inc.Start, inc.Records, inc.JournalTruncated = j.Header.Meta.Service, j.Header.Meta.Start, len(j.Records), j.Truncated
	inc.Steps, inc.Truncated = variantSteps(j.Records)
	for _, s := range inc.Steps {
		if s.Marker != nil {
			inc.Failure = &IncidentFailure{Seq: s.Seq, Kind: s.Marker.Kind, Message: s.Marker.Message}
			break
		}
	}
	if inc.Kind == "traffic" && inc.Fixture != "verifier/traffic.kavach" {
		return inc, nil
	}
	for _, b := range []struct {
		name string
		dst  **VariantResult
	}{{"replay-old.json", &inc.Old}, {"replay-new.json", &inc.New}} {
		if res, err := readReplayReport(filepath.Join(v, b.name)); err == nil {
			var cut bool
			*b.dst, cut = variantResult(res)
			inc.Truncated = inc.Truncated || cut
		}
	}
	if inc.Kind == "traffic" {
		return inc, nil
	}
	inc.Diff = readDiffReport(filepath.Join(v, "kavach-diff.json"))
	if inc.Diff == nil {
		inc.Diff = diffFromLog(filepath.Join(v, "grade.log"))
	}
	return inc, nil
}

// taskFixture finds the incident.kavach beside the Dockerfile the task's
// verifier builds (bench/agent/tasks/<task>/), from the Harbor task the trial
// ran. It returns "" when the task has none.
func taskFixture(trialDir string) string {
	b, err := os.ReadFile(filepath.Join(trialDir, "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Task struct {
			Path string `json:"path"`
		} `json:"task"`
	}
	if json.Unmarshal(b, &cfg) != nil || cfg.Task.Path == "" {
		return ""
	}
	env, err := TaskEnvironment(filepath.Dir(cfg.Task.Path), filepath.Base(cfg.Task.Path))
	if err != nil {
		return ""
	}
	for _, f := range env.Files {
		if f.Role == "verifier" && f.Dockerfile != "" {
			p := filepath.Join(filepath.Dir(f.Dockerfile), "incident.kavach")
			if exists(p) {
				return p
			}
		}
	}
	return ""
}

// readReplayReport reads `kavach replay --json`: the replay.Result under
// "result".
func readReplayReport(path string) (*replay.Result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rep struct {
		Result *replay.Result `json:"result"`
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if rep.Result == nil {
		return nil, fmt.Errorf("%s: no result", filepath.Base(path))
	}
	return rep.Result, nil
}

// readDiffReport reads `kavach diff --json`, or returns nil when it is
// missing or not a report.
func readDiffReport(path string) *IncidentDiff {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rep struct {
		IncidentDiff
		Divergence *struct {
			Seq    uint64 `json:"seq"`
			Output int    `json:"output"`
			Detail string `json:"detail"`
		} `json:"divergence"`
	}
	if json.Unmarshal(b, &rep) != nil || rep.Verdict == "" {
		return nil
	}
	d := rep.IncidentDiff
	d.Source, d.Detail = "json", clipTo(d.Detail, maxFailText)
	if rep.Divergence != nil {
		d.Divergence = &Divergence{Seq: rep.Divergence.Seq, Output: rep.Divergence.Output, Detail: clipTo(rep.Divergence.Detail, maxFailText)}
	}
	for _, p := range []*ProbeSet{d.Variants, d.Regressions} {
		if p == nil {
			continue
		}
		if p.Checks == nil {
			p.Checks = []Probe{}
		}
		if len(p.Checks) > maxProbes {
			p.Checks = p.Checks[:maxProbes]
		}
		for i := range p.Checks {
			c := &p.Checks[i]
			c.Mutation, c.Old, c.New = clipTo(c.Mutation, maxFailText), clipTo(c.Old, maxFailText), clipTo(c.New, maxFailText)
		}
	}
	return &d
}

var (
	// diffReportLine is a line of the CLI's diff report.
	diffReportLine = regexp.MustCompile(`^(fixture|old|new|verdict|variants|regressions)\s+(.+?)\s*$`)
	divLine        = regexp.MustCompile(`^first divergence at step seq (\d+): (.+?)\s*$`)
	divOutput      = regexp.MustCompile(`^output (\d+) `)
)

// diffFromLog reads the last kavach diff report a grader logged. It returns
// nil when there is none.
func diffFromLog(path string) *IncidentDiff {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var d *IncidentDiff
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := ansiEsc.ReplaceAllString(strings.TrimRight(sc.Text(), "\r"), "")
		if m := diffReportLine.FindStringSubmatch(line); m != nil {
			if m[1] == "fixture" {
				d = &IncidentDiff{Source: "log"} // a report starts here
				continue
			}
			if d == nil {
				continue
			}
			v := clipTo(m[2], maxFailText)
			switch m[1] {
			case "old":
				d.Old = v
			case "new":
				d.New = v
			case "verdict":
				d.Verdict = v
			case "variants":
				d.VariantsLine = v
			case "regressions":
				d.RegressionsLine = v
			}
			continue
		}
		if m := divLine.FindStringSubmatch(line); m != nil && d != nil {
			seq, _ := strconv.ParseUint(m[1], 10, 64)
			dv := &Divergence{Seq: seq, Detail: clipTo(m[2], maxFailText)}
			if o := divOutput.FindStringSubmatch(m[2]); o != nil {
				dv.Output, _ = strconv.Atoi(o[1])
			}
			d.Divergence = dv
		}
	}
	if d == nil || d.Verdict == "" {
		return nil
	}
	return d
}
