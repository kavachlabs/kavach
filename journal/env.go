package journal

// Env is the environment record of a journal (SPEC.md §3.2): the process
// environment and host facts at the moment the recorder started. It lets a
// replay reproduce failures that depend on TZ, GOMAXPROCS, config variables and
// the like, and lets tools report when the replaying machine differs.
type Env struct {
	// Vars holds environment variables, name to value. In 0.1 recorders store
	// every variable verbatim, secrets included, and set Scrubbed to false.
	// A scrubber removes or masks values and sets Scrubbed to true.
	Vars map[string]string `json:"vars,omitempty"`
	// Scrubbed reports whether a scrubber has processed Vars.
	Scrubbed bool    `json:"scrubbed"`
	System   *System `json:"system,omitempty"`
	Build    *Build  `json:"build,omitempty"`
}

// System describes the host the journal was recorded on.
type System struct {
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Kernel     string `json:"kernel,omitempty"`
	CPUs       int    `json:"cpus"`
	GOMAXPROCS int    `json:"gomaxprocs"`
	Timezone   string `json:"timezone,omitempty"`
	Hostname   string `json:"hostname,omitempty"`
}

// Build describes the binary that recorded the journal.
type Build struct {
	GoVersion string `json:"go_version"`
	Module    string `json:"module,omitempty"`
	Revision  string `json:"revision,omitempty"`
}
