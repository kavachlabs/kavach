// Package kconfig parses, validates, and writes Kavach git-native YAML
// configuration files (.kavach/config.yaml and .kavach/invariants.yaml)
// with zero external dependencies.
package kconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Default git-native .kavach/ paths.
const (
	DefaultDir            = ".kavach"
	DefaultConfigFile     = ".kavach/config.yaml"
	DefaultInvariantsFile = ".kavach/invariants.yaml"
	DefaultCapsulesDir    = ".kavach/capsules"
	DefaultMapJSONFile    = ".kavach/map.json"
	DefaultMapHTMLFile    = ".kavach/map.html"
)

// Supported declarative invariant kinds in .kavach/invariants.yaml.
const (
	KindAtMostOnceEmit = "at_most_once_emit"
	KindAntiLobotomy   = "anti_lobotomy"
	KindNonNegative    = "non_negative"
	KindConservation   = "conservation"
)

// Supported modes and PII scrubbing levels in .kavach/config.yaml.
const (
	ModeDriver = "driver"
	ModeScoped = "scoped"
	ModeRecord = "record"
	ModeReplay = "replay"
	ModeOff    = "off"

	PIIScrubOff      = "off"
	PIIScrubStandard = "standard"
	PIIScrubStrict   = "strict"
)

// Config represents .kavach/config.yaml.
type Config struct {
	Service   string   `yaml:"service" json:"service"`
	Mode      string   `yaml:"mode,omitempty" json:"mode,omitempty"`
	PII       string   `yaml:"pii,omitempty" json:"pii,omitempty"`
	RingPath  string   `yaml:"ring_path,omitempty" json:"ring_path,omitempty"`
	VaultPath string   `yaml:"vault_path,omitempty" json:"vault_path,omitempty"`
	Variants  int      `yaml:"variants,omitempty" json:"variants,omitempty"`
	Drivers   []string `yaml:"drivers,omitempty" json:"drivers,omitempty"`
}

// InvariantsFile represents .kavach/invariants.yaml.
type InvariantsFile struct {
	Service    string          `yaml:"service" json:"service"`
	Invariants []InvariantRule `yaml:"invariants" json:"invariants"`
}

// InvariantRule declares one declarative invariant checked during replay and
// fix verification.
type InvariantRule struct {
	Name               string   `yaml:"name" json:"name"`
	Kind               string   `yaml:"kind" json:"kind"`
	Sink               string   `yaml:"sink,omitempty" json:"sink,omitempty"`
	Sinks              []string `yaml:"sinks,omitempty" json:"sinks,omitempty"`
	Key                string   `yaml:"key,omitempty" json:"key,omitempty"`
	Field              string   `yaml:"field,omitempty" json:"field,omitempty"`
	OnErrorEmitCount   *int     `yaml:"on_error_emit_count,omitempty" json:"on_error_emit_count,omitempty"`
	MinRatioVsBaseline float64  `yaml:"min_ratio_vs_baseline,omitempty" json:"min_ratio_vs_baseline,omitempty"`
	Expr               string   `yaml:"expr,omitempty" json:"expr,omitempty"`
}

// IntPtr returns a pointer to v (useful for setting InvariantRule.OnErrorEmitCount).
func IntPtr(v int) *int { return &v }

// DefaultConfig returns a sensible default Config for service.
func DefaultConfig(service string) *Config {
	return &Config{
		Service:   service,
		Mode:      ModeDriver,
		PII:       PIIScrubStandard,
		VaultPath: DefaultCapsulesDir,
		Variants:  25,
	}
}

// FindInvariantsFile returns filepath.Join(dir, DefaultInvariantsFile) if that
// file exists and is not a directory, or "" otherwise.
func FindInvariantsFile(dir string) string {
	if dir == "" {
		dir = "."
	}
	p := filepath.Join(dir, DefaultInvariantsFile)
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return ""
	}
	return p
}

// Validate checks Config fields for validity.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("kconfig: nil config")
	}
	switch c.Mode {
	case "", ModeDriver, ModeScoped, ModeRecord, ModeReplay, ModeOff:
	default:
		return fmt.Errorf("kconfig: unsupported mode %q", c.Mode)
	}
	switch c.PII {
	case "", PIIScrubOff, PIIScrubStandard, PIIScrubStrict:
	default:
		return fmt.Errorf("kconfig: unsupported pii mode %q", c.PII)
	}
	if c.Variants < 0 {
		return fmt.Errorf("kconfig: variants must not be negative (%d)", c.Variants)
	}
	return nil
}

// ParseConfig parses YAML bytes into a validated Config.
func ParseConfig(data []byte) (*Config, error) {
	root, err := parseYAML(data)
	if err != nil {
		return nil, fmt.Errorf("kconfig: parse config.yaml: %w", err)
	}
	if root.kind != yamlMap {
		return nil, errors.New("kconfig: parse config.yaml: root must be a YAML mapping")
	}
	cfg := &Config{}
	for _, k := range root.keys {
		v := root.fields[k]
		switch k {
		case "service":
			if cfg.Service, err = v.asString(); err != nil {
				return nil, fmt.Errorf("kconfig: service: %w", err)
			}
		case "mode":
			if cfg.Mode, err = v.asString(); err != nil {
				return nil, fmt.Errorf("kconfig: mode: %w", err)
			}
		case "pii":
			if cfg.PII, err = v.asString(); err != nil {
				return nil, fmt.Errorf("kconfig: pii: %w", err)
			}
		case "ring_path":
			if cfg.RingPath, err = v.asString(); err != nil {
				return nil, fmt.Errorf("kconfig: ring_path: %w", err)
			}
		case "vault_path":
			if cfg.VaultPath, err = v.asString(); err != nil {
				return nil, fmt.Errorf("kconfig: vault_path: %w", err)
			}
		case "variants":
			if cfg.Variants, err = v.asInt(); err != nil {
				return nil, fmt.Errorf("kconfig: variants: %w", err)
			}
		case "drivers":
			if cfg.Drivers, err = v.asStringSlice(); err != nil {
				return nil, fmt.Errorf("kconfig: drivers: %w", err)
			}
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadConfig reads and parses a .kavach/config.yaml file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(b)
}

// MarshalConfig renders cfg as clean, deterministic YAML.
func MarshalConfig(cfg *Config) ([]byte, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var b strings.Builder
	if cfg.Service != "" {
		b.WriteString("service: " + formatYAMLScalar(cfg.Service) + "\n")
	}
	if cfg.Mode != "" {
		b.WriteString("mode: " + formatYAMLScalar(cfg.Mode) + "\n")
	}
	if cfg.PII != "" {
		b.WriteString("pii: " + formatYAMLScalar(cfg.PII) + "\n")
	}
	if cfg.RingPath != "" {
		b.WriteString("ring_path: " + formatYAMLScalar(cfg.RingPath) + "\n")
	}
	if cfg.VaultPath != "" {
		b.WriteString("vault_path: " + formatYAMLScalar(cfg.VaultPath) + "\n")
	}
	if cfg.Variants > 0 {
		b.WriteString("variants: " + strconv.Itoa(cfg.Variants) + "\n")
	}
	if len(cfg.Drivers) > 0 {
		b.WriteString("drivers: " + formatInlineList(cfg.Drivers) + "\n")
	}
	return []byte(b.String()), nil
}

// WriteConfig writes cfg as YAML to path, creating parent directories as needed.
func WriteConfig(path string, cfg *Config) error {
	b, err := MarshalConfig(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Validate checks InvariantsFile and all contained rules for validity.
func (f *InvariantsFile) Validate() error {
	if f == nil {
		return errors.New("kconfig: nil invariants file")
	}
	seen := make(map[string]bool, len(f.Invariants))
	for i, r := range f.Invariants {
		if err := r.Validate(); err != nil {
			return fmt.Errorf("kconfig: invariant[%d]: %w", i, err)
		}
		if seen[r.Name] {
			return fmt.Errorf("kconfig: duplicate invariant name %q", r.Name)
		}
		seen[r.Name] = true
	}
	return nil
}

// Validate checks a single InvariantRule for validity.
func (r InvariantRule) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("invariant is missing name")
	}
	switch r.Kind {
	case KindAtMostOnceEmit:
		if r.OnErrorEmitCount != nil && *r.OnErrorEmitCount < 0 {
			return fmt.Errorf("invariant %q: on_error_emit_count must not be negative (%d)", r.Name, *r.OnErrorEmitCount)
		}
	case KindAntiLobotomy:
		if r.MinRatioVsBaseline < 0 {
			return fmt.Errorf("invariant %q: min_ratio_vs_baseline must not be negative (%g)", r.Name, r.MinRatioVsBaseline)
		}
	case KindNonNegative:
		if strings.TrimSpace(r.Field) == "" && strings.TrimSpace(r.Expr) == "" {
			return fmt.Errorf("invariant %q: non_negative requires field", r.Name)
		}
	case KindConservation:
		if strings.TrimSpace(r.Field) == "" && strings.TrimSpace(r.Expr) == "" {
			return fmt.Errorf("invariant %q: conservation requires field", r.Name)
		}
	case "":
		return fmt.Errorf("invariant %q is missing kind", r.Name)
	default:
		return fmt.Errorf("invariant %q has unsupported kind %q", r.Name, r.Kind)
	}
	return nil
}

// TargetSinks returns the deduplicated list of sinks referenced by Sink and Sinks.
func (r InvariantRule) TargetSinks() []string {
	if r.Sink == "" && len(r.Sinks) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	if r.Sink != "" {
		seen[r.Sink] = true
		out = append(out, r.Sink)
	}
	for _, s := range r.Sinks {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ParseInvariants parses YAML bytes into a validated InvariantsFile.
func ParseInvariants(data []byte) (*InvariantsFile, error) {
	root, err := parseYAML(data)
	if err != nil {
		return nil, fmt.Errorf("kconfig: parse invariants.yaml: %w", err)
	}
	if root.kind != yamlMap {
		return nil, errors.New("kconfig: parse invariants.yaml: root must be a YAML mapping")
	}
	out := &InvariantsFile{}
	for _, k := range root.keys {
		v := root.fields[k]
		switch k {
		case "service":
			if out.Service, err = v.asString(); err != nil {
				return nil, fmt.Errorf("kconfig: service: %w", err)
			}
		case "invariants":
			if v.isNull() {
				continue
			}
			if v.kind != yamlSeq {
				return nil, fmt.Errorf("kconfig: invariants: line %d: expected list", v.line)
			}
			for idx, item := range v.items {
				rule, err := decodeInvariantRule(item)
				if err != nil {
					return nil, fmt.Errorf("kconfig: invariants[%d]: %w", idx, err)
				}
				out.Invariants = append(out.Invariants, rule)
			}
		}
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeInvariantRule(n *yamlNode) (InvariantRule, error) {
	if n == nil || n.kind != yamlMap {
		line := 0
		if n != nil {
			line = n.line
		}
		return InvariantRule{}, fmt.Errorf("line %d: expected invariant mapping", line)
	}
	var r InvariantRule
	var err error
	for _, k := range n.keys {
		v := n.fields[k]
		switch k {
		case "name":
			if r.Name, err = v.asString(); err != nil {
				return r, fmt.Errorf("name: %w", err)
			}
		case "kind":
			if r.Kind, err = v.asString(); err != nil {
				return r, fmt.Errorf("kind: %w", err)
			}
		case "sink":
			if r.Sink, err = v.asString(); err != nil {
				return r, fmt.Errorf("sink: %w", err)
			}
		case "sinks":
			if r.Sinks, err = v.asStringSlice(); err != nil {
				return r, fmt.Errorf("sinks: %w", err)
			}
		case "key":
			if r.Key, err = v.asString(); err != nil {
				return r, fmt.Errorf("key: %w", err)
			}
		case "field":
			if r.Field, err = v.asString(); err != nil {
				return r, fmt.Errorf("field: %w", err)
			}
		case "on_error_emit_count":
			if !v.isNull() {
				val, err := v.asInt()
				if err != nil {
					return r, fmt.Errorf("on_error_emit_count: %w", err)
				}
				r.OnErrorEmitCount = IntPtr(val)
			}
		case "min_ratio_vs_baseline":
			if r.MinRatioVsBaseline, err = v.asFloat64(); err != nil {
				return r, fmt.Errorf("min_ratio_vs_baseline: %w", err)
			}
		case "expr":
			if r.Expr, err = v.asString(); err != nil {
				return r, fmt.Errorf("expr: %w", err)
			}
		}
	}
	return r, nil
}

// LoadInvariants reads and parses a .kavach/invariants.yaml file.
func LoadInvariants(path string) (*InvariantsFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseInvariants(b)
}

// MarshalInvariants renders inv as clean, deterministic YAML matching §4 of the spec.
func MarshalInvariants(inv *InvariantsFile) ([]byte, error) {
	if err := inv.Validate(); err != nil {
		return nil, err
	}
	var b strings.Builder
	if inv.Service != "" {
		b.WriteString("service: " + formatYAMLScalar(inv.Service) + "\n")
	}
	if len(inv.Invariants) == 0 {
		b.WriteString("invariants: []\n")
		return []byte(b.String()), nil
	}
	b.WriteString("invariants:\n")
	for i, r := range inv.Invariants {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("  - name: " + formatYAMLScalar(r.Name) + "\n")
		b.WriteString("    kind: " + formatYAMLScalar(r.Kind) + "\n")
		if r.Sink != "" {
			b.WriteString("    sink: " + formatYAMLScalar(r.Sink) + "\n")
		}
		if len(r.Sinks) > 0 {
			b.WriteString("    sinks: " + formatInlineList(r.Sinks) + "\n")
		}
		if r.Key != "" {
			b.WriteString("    key: " + formatYAMLScalar(r.Key) + "\n")
		}
		if r.Field != "" {
			b.WriteString("    field: " + formatYAMLScalar(r.Field) + "\n")
		}
		if r.OnErrorEmitCount != nil {
			b.WriteString("    on_error_emit_count: " + strconv.Itoa(*r.OnErrorEmitCount) + "\n")
		}
		if r.MinRatioVsBaseline > 0 {
			b.WriteString("    min_ratio_vs_baseline: " + formatFloat(r.MinRatioVsBaseline) + "\n")
		}
		if r.Expr != "" {
			b.WriteString("    expr: " + formatYAMLScalar(r.Expr) + "\n")
		}
	}
	return []byte(b.String()), nil
}

func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// WriteInvariants writes inv as YAML to path, creating parent directories as needed.
func WriteInvariants(path string, inv *InvariantsFile) error {
	b, err := MarshalInvariants(inv)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
