package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kavachlabs/kavach/journal"
)

// maxIncidents bounds a listing.
const maxIncidents = 500

// incident summarizes one fixture.
type incident struct {
	Path       string           `json:"path"`
	Service    string           `json:"service,omitempty"`
	Start      string           `json:"start,omitempty"`
	Records    int              `json:"records"`
	Bytes      int64            `json:"bytes"`
	RecordedAt string           `json:"recorded_at,omitempty"`
	Handler    string           `json:"handler,omitempty"`
	Failure    *incidentFailure `json:"failure,omitempty"` // first panic, error or invariant marker
	Trigger    string           `json:"trigger,omitempty"` // reason of a manual flush
	Variant    *journal.Variant `json:"variant,omitempty"`
	Error      string           `json:"error,omitempty"` // the file could not be read
}

type incidentFailure struct {
	Kind    string `json:"kind"`
	Seq     uint64 `json:"seq"` // seq of the input whose step failed
	Message string `json:"message"`
}

// listIncidents finds fixtures (*.kavach) under root, skipping hidden
// directories and vendor/. It reports unreadable fixtures with Error set rather
// than failing, and stops after maxIncidents.
func listIncidents(root string) (list []incident, truncated bool, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".kavach" {
			return nil
		}
		if len(list) == maxIncidents {
			truncated = true
			return filepath.SkipAll
		}
		list = append(list, describeIncident(path))
		return nil
	})
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	return list, truncated, err
}

func describeIncident(path string) incident {
	inc := incident{Path: path}
	if st, err := os.Stat(path); err == nil {
		inc.Bytes = st.Size()
	}
	j, err := journal.ReadFile(path)
	if err != nil {
		inc.Error = err.Error()
		return inc
	}
	m := j.Header.Meta
	inc.Service, inc.Start, inc.Records = m.Service, m.Start, len(j.Records)
	inc.RecordedAt, inc.Handler, inc.Variant = m.RecordedAt, m.Handler, m.Variant
	var input uint64
	for _, r := range j.Records {
		switch {
		case r.Type == journal.TypeInput:
			input = r.Seq
		case r.Type == journal.TypeMarker && r.Kind == journal.MarkerTrigger && inc.Trigger == "":
			inc.Trigger = r.Message
		case r.Type == journal.TypeMarker && inc.Failure == nil &&
			(r.Kind == journal.MarkerPanic || r.Kind == journal.MarkerError || r.Kind == journal.MarkerInvariant):
			inc.Failure = &incidentFailure{Kind: r.Kind, Seq: input, Message: r.Message}
		}
	}
	return inc
}

func (inc incident) summary() string {
	switch {
	case inc.Error != "":
		return "unreadable: " + inc.Error
	case inc.Variant != nil:
		return fmt.Sprintf("variant %d: %s", inc.Variant.ID, inc.Variant.Mutation)
	case inc.Failure != nil:
		return fmt.Sprintf("%s at seq %d: %s", inc.Failure.Kind, inc.Failure.Seq, inc.Failure.Message)
	case inc.Trigger != "":
		return "trigger: " + inc.Trigger
	}
	return "no failure"
}

func cmdList(u ui, args []string, stdout, stderr io.Writer) int {
	fs := newFlags("list", stderr)
	asJSON := fs.Bool("json", false, "print the listing as JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return exitUsage
	}
	if len(pos) > 1 {
		fmt.Fprintf(stderr, "kavach list: expected at most one directory, got %d arguments\n", len(pos))
		return exitUsage
	}
	root := "."
	if len(pos) == 1 {
		root = pos[0]
	}
	list, truncated, err := listIncidents(root)
	if err != nil {
		fmt.Fprintf(stderr, "kavach list: %v\n", err)
		return exitError
	}
	if *asJSON {
		return writeJSON(stdout, stderr, map[string]any{"root": root, "incidents": orEmpty(list), "truncated": truncated})
	}
	u.printBanner(stdout)
	u.header(stdout, "KAVACH INCIDENTS")
	if len(list) == 0 {
		fmt.Fprintf(stdout, "no fixtures (*.kavach) under %s\n", root)
		return exitPass
	}
	for _, inc := range list {
		color := ansiDim
		switch {
		case inc.Error != "":
			color = ansiRed
		case inc.Variant != nil:
			color = ansiYellow
		case inc.Failure != nil:
			color = markerColor(inc.Failure.Kind)
		}
		fmt.Fprintf(stdout, "%s\n    %s · %d records · %d bytes · %s\n", u.paint(inc.Path, ansiBold),
			orDash(inc.Service), inc.Records, inc.Bytes, u.paint(inc.summary(), color))
	}
	if truncated {
		fmt.Fprintf(stdout, "%s\n", u.paint(fmt.Sprintf("stopped after %d fixtures", maxIncidents), ansiYellow))
	}
	return exitPass
}

func orEmpty(l []incident) []incident {
	if l == nil {
		return []incident{}
	}
	return l
}
