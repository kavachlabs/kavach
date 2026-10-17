package harbor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Provider is a remote Harbor environment: the credentials its preflight
// accepts (harbor/environments/) and the Python module its extra installs.
type Provider struct {
	Name string
	// Sets are alternative credentials; all the variables of any one set
	// authenticate. The first is the one to suggest.
	Sets [][]string
	// File, under the home folder, also authenticates when it exists.
	File string
	// Module is imported by Harbor's environment; it is missing unless the
	// harbor[Name] extra is installed.
	Module string
}

// Providers are the remote environments a run can use, in display order.
//
// shortcut: Vercel also accepts the credentials `vercel link` writes to
// .env.local, found by the vercel SDK; bench asks for VERCEL_TOKEN only.
var Providers = []Provider{
	{"daytona", [][]string{{"DAYTONA_API_KEY"}, {"DAYTONA_JWT_TOKEN", "DAYTONA_ORGANIZATION_ID"}}, "", "daytona"},
	{"modal", [][]string{{"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET"}}, ".modal.toml", "modal"},
	{"vercel", [][]string{{"VERCEL_TOKEN"}}, "", "vercel.sandbox"},
}

// Keys lists every variable p reads, for display.
func (p Provider) Keys() []string {
	var keys []string
	for _, set := range p.Sets {
		keys = append(keys, set...)
	}
	return keys
}

// Need describes the credentials p accepts.
func (p Provider) Need() string {
	var alts []string
	for _, set := range p.Sets {
		alts = append(alts, strings.Join(set, " and "))
	}
	if p.File != "" {
		alts = append(alts, "~/"+p.File)
	}
	s := alts[0]
	if len(alts) > 1 {
		s += " (or " + strings.Join(alts[1:], ", or ") + ")"
	}
	return s
}

// KeySet reports whether Harbor will see k set. Harbor loads the env file
// over the process environment, so a key the file leaves empty is empty even
// when bench's own environment has it. Values are never returned.
func (r *Runner) KeySet(k string) bool {
	return r.Getenv(k) != ""
}

// Getenv is the value Harbor sees for k: the env file's, else the process
// environment's.
func (r *Runner) Getenv(k string) string {
	vals, _ := ReadEnv(r.EnvFile)
	if v, ok := vals[k]; ok {
		return v
	}
	return os.Getenv(k)
}

// Credentials says whether p can authenticate: missing is empty if it can,
// and via names the file that does it when no variables do. Otherwise
// missing lists what the first set lacks.
func (r *Runner) Credentials(p Provider) (missing []string, via string) {
	return credentials(p, r.KeySet)
}

func credentials(p Provider, isSet func(string) bool) (missing []string, via string) {
	for _, set := range p.Sets {
		ok := true
		for _, k := range set {
			ok = ok && isSet(k)
		}
		if ok {
			return nil, ""
		}
	}
	if p.File != "" {
		if home, err := os.UserHomeDir(); err == nil && exists(filepath.Join(home, p.File)) {
			return nil, "~/" + p.File
		}
	}
	for _, k := range p.Sets[0] {
		if !isSet(k) {
			missing = append(missing, k)
		}
	}
	return missing, ""
}

// Problem says why a run on p cannot start, or "" if it can.
func (r *Runner) Problem(p Provider) string {
	return r.problem(p, r.KeySet)
}

func (r *Runner) problem(p Provider, isSet func(string) bool) string {
	if missing, _ := credentials(p, isSet); len(missing) > 0 {
		return fmt.Sprintf("%s needs %s in the env file or bench's environment", p.Name, p.Need())
	}
	if err := r.extraErr(p); err != nil {
		return err.Error()
	}
	return ""
}

type extraResult struct {
	err error
	at  time.Time
}

// extraRetry is how long a missing extra stays cached, so installing it is
// noticed without restarting bench.
const extraRetry = 30 * time.Second

func (r *Runner) extraErr(p Provider) error {
	if r.Extra != nil {
		return r.Extra(p)
	}
	r.extraMu.Lock()
	defer r.extraMu.Unlock()
	if c, ok := r.extras[p.Name]; ok && (c.err == nil || time.Since(c.at) < extraRetry) {
		return c.err
	}
	err := r.Installed(p)
	if r.extras == nil {
		r.extras = map[string]extraResult{}
	}
	r.extras[p.Name] = extraResult{err, time.Now()}
	return err
}

// Installed imports p's module with the Python that runs Harbor, found from
// the harbor script's shebang. A harbor that is not a Python script (a
// wrapper, a test double) cannot be asked, and is taken to have the extra.
func (r *Runner) Installed(p Provider) error {
	py := r.interpreter()
	if py == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, py, "-I", "-c", "import "+p.Module).Run(); err != nil {
		return fmt.Errorf("this Harbor install lacks %s support (cannot import %s); install it with: uv tool install --with 'harbor[%s]' harbor", p.Name, p.Module, p.Name)
	}
	return nil
}

func (r *Runner) interpreter() string {
	path, err := exec.LookPath(r.Harbor)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	if !strings.HasPrefix(line, "#!") {
		return ""
	}
	fields := strings.Fields(line[2:])
	if len(fields) > 1 && filepath.Base(fields[0]) == "env" {
		fields = fields[1:]
	}
	if len(fields) == 0 || !strings.HasPrefix(filepath.Base(fields[0]), "python") {
		return ""
	}
	return fields[0]
}
