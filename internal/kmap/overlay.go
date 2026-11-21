package kmap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// Overlay adds a mode tab of stage-chain lanes (for example, Shadow Healer runs)
// to a map without encoding enterprise-specific schemas into the OSS CLI.
type Overlay struct {
	Version int    `json:"kavach_map_overlay"`
	Mode    string `json:"mode"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	Lanes   []Lane `json:"lanes"`
	Source  string `json:"source"`
}

// Lane is one horizontal chain of stages inside an overlay mode.
type Lane struct {
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Stages []Stage `json:"stages"`
}

// Stage is one card in a lane.
type Stage struct {
	ID      string         `json:"id"`
	Title   string         `json:"title"`
	Status  string         `json:"status"`
	Verdict string         `json:"verdict,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	Nodes   []string       `json:"nodes,omitempty"`
	Fixture string         `json:"fixture,omitempty"`
	Metrics map[string]any `json:"metrics,omitempty"`
	Items   []Item         `json:"items,omitempty"`
	Runs    []string       `json:"runs,omitempty"`
}

// Item is a detailed line inside a stage (e.g. compensation outputs to undo/apply).
type Item struct {
	Label  string   `json:"label,omitempty"`
	Text   string   `json:"text,omitempty"`
	Status string   `json:"status,omitempty"`
	Sink   string   `json:"sink,omitempty"`
	Nodes  []string `json:"nodes,omitempty"`
}

var validStageStatuses = map[string]bool{
	"pass": true,
	"fail": true,
	"warn": true,
	"info": true,
	"skip": true,
}

// LoadOverlay reads, decodes, normalizes, and validates an overlay JSON file.
func LoadOverlay(path string) (*Overlay, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ov Overlay
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ov); err != nil {
		return nil, fmt.Errorf("decode overlay %s: %w", path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("decode overlay %s: unexpected trailing JSON data", path)
	}
	if err := ov.normalizeAndValidate(path); err != nil {
		return nil, err
	}
	return &ov, nil
}

func (ov *Overlay) normalizeAndValidate(path string) error {
	if ov.Version != 1 {
		return fmt.Errorf("overlay %s: kavach_map_overlay must be 1, got %d", path, ov.Version)
	}
	if ov.Mode == "" {
		return fmt.Errorf("overlay %s: mode is required", path)
	}
	if ov.Title == "" {
		ov.Title = ov.Mode
	}
	for li := range ov.Lanes {
		l := &ov.Lanes[li]
		if l.Title == "" {
			l.Title = l.ID
		}
		for si := range l.Stages {
			st := &l.Stages[si]
			if st.Status == "" {
				st.Status = "info"
			}
			if !validStageStatuses[st.Status] {
				return fmt.Errorf("overlay %s lane %q stage %q: unknown status %q", path, l.ID, st.ID, st.Status)
			}
			if len(st.Metrics) > 0 {
				norm := make(map[string]any, len(st.Metrics))
				for k, v := range st.Metrics {
					norm[k] = normalizeMetricValue(v)
				}
				st.Metrics = norm
			}
		}
	}
	return nil
}

func normalizeMetricValue(v any) any {
	switch v.(type) {
	case string, bool, float64, float32, int, int64, uint64, json.Number:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}
