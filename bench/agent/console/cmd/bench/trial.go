package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

const trialUsage = `usage: bench trial <group> <verb> [flags] [args]

  invite add [-label NAME] [-runs N] [-code CODE] [-note TEXT]
  invite list
  invite show <code|label>
  invite revoke <code|label>
  task   add|remove <task>...      list
  model  add|remove <model>...     list

Edits the trial's config file (-config, default $TRIAL_CONFIG or
/etc/kavach/trial.json); a running "bench api -trial" picks the change up
without a restart. Flags go before the arguments.
`

// trialOpts are the flags every bench trial verb shares.
type trialOpts struct {
	config, jobs, root, tasks, envFile, site string
}

func (o *trialOpts) register(fs *flag.FlagSet) {
	cfg := os.Getenv("TRIAL_CONFIG")
	if cfg == "" {
		cfg = api.DefaultTrialConfig
	}
	fs.StringVar(&o.config, "config", cfg, "the trial config file")
	fs.StringVar(&o.jobs, "jobs", "/var/lib/kavach/jobs", "the trial's jobs directory, where invite usage is kept")
	fs.StringVar(&o.root, "root", "", "the kavach repository (default /opt/kavach if it exists, else found from the current directory)")
	fs.StringVar(&o.tasks, "tasks", "", "the Harbor tasks directory (default: <root>/bench/agent/harbor/tasks)")
	fs.StringVar(&o.envFile, "env", "/etc/kavach/trial.env", "the server's environment file, whose TRIAL_TASKS and TRIAL_MODELS a first edit starts from")
	fs.StringVar(&o.site, "url", "https://try.kavachlabs.dev", "the trial's public address, for share links")
}

func (o *trialOpts) tasksDir() (string, error) {
	if o.tasks != "" {
		return o.tasks, nil
	}
	root := o.root
	if root == "" {
		if st, err := os.Stat("/opt/kavach/bench/agent"); err == nil && st.IsDir() {
			root = "/opt/kavach"
		} else if root, err = findRoot(); err != nil {
			return "", err
		}
	}
	return filepath.Join(root, "bench", "agent", "harbor", "tasks"), nil
}

// envList is the TRIAL_ list a first edit of the file starts from, so
// adding one task does not drop the ones the environment allowed.
func (o *trialOpts) envList(key string) []string {
	vals, _ := harbor.ReadEnv(o.envFile)
	v, ok := vals[key]
	if !ok {
		v = os.Getenv(key)
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func cmdTrial(args []string, stdout, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprint(stderr, trialUsage)
		return 2
	}
	name := args[0] + " " + args[1]
	handlers := map[string]func(*trialOpts, *flag.FlagSet) func(io.Writer) error{
		"invite add":    inviteAdd,
		"invite list":   inviteList,
		"invite show":   inviteShow,
		"invite revoke": inviteRevoke,
		"task add":      func(o *trialOpts, fs *flag.FlagSet) func(io.Writer) error { return taskEdit(o, fs, true) },
		"task remove":   func(o *trialOpts, fs *flag.FlagSet) func(io.Writer) error { return taskEdit(o, fs, false) },
		"task list":     taskList,
		"model add":     func(o *trialOpts, fs *flag.FlagSet) func(io.Writer) error { return modelEdit(o, fs, true) },
		"model remove":  func(o *trialOpts, fs *flag.FlagSet) func(io.Writer) error { return modelEdit(o, fs, false) },
		"model list":    modelList,
	}
	h := handlers[name]
	if h == nil {
		fmt.Fprintf(stderr, "bench trial: unknown command %q\n\n%s", name, trialUsage)
		return 2
	}
	fs := flag.NewFlagSet("bench trial "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o trialOpts
	o.register(fs)
	run := h(&o, fs)
	if err := fs.Parse(args[2:]); err != nil {
		return 2
	}
	if err := run(stdout); err != nil {
		fmt.Fprintf(stderr, "bench trial %s: %v\n", name, err)
		return 1
	}
	return 0
}

const codeAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// newInviteCode is yc-xxxx-xxxx-xxxx-xxxx: 16 characters without the ones
// that read alike, about 78 bits.
func newInviteCode(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString(prefix)
	for i, c := range b {
		if i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(codeAlphabet[int(c)%len(codeAlphabet)])
	}
	return sb.String(), nil
}

func (o *trialOpts) shareURL(code string) string {
	return strings.TrimRight(o.site, "/") + "/#/trial/invite?code=" + url.QueryEscape(code)
}

func inviteAdd(o *trialOpts, fs *flag.FlagSet) func(io.Writer) error {
	label := fs.String("label", "", "who the code is for, e.g. \"Garry (YC)\"")
	runs := fs.Int("runs", 5, "runs the code may start")
	code := fs.String("code", "", "the code (default: generated)")
	note := fs.String("note", "", "a note kept with the code")
	prefix := fs.String("prefix", "yc", "prefix of a generated code")
	return func(stdout io.Writer) error {
		if *label == "" {
			return errors.New("-label is required: one code per partner")
		}
		if *runs < 1 {
			return errors.New("-runs must be at least 1")
		}
		c := *code
		if c == "" {
			var err error
			if c, err = newInviteCode(*prefix); err != nil {
				return err
			}
		}
		err := api.UpdateTrialFile(o.config, func(f *api.TrialFile) error {
			f.Invites = append(f.Invites, api.TrialInvite{Code: c, Label: *label, Runs: *runs, Created: time.Now().UTC().Truncate(time.Second), Note: *note})
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "code  %s\nlabel %s\nruns  %d\nurl   %s\n", c, *label, *runs, o.shareURL(c))
		return nil
	}
}

// findInvite picks the invite a user means by its code, or else by its label
// (ignoring case) among those not revoked: the whole label, else a part of it
// that only one invite has.
func findInvite(f *api.TrialFile, ref string, partial bool) (int, error) {
	if ref == "" {
		return 0, errors.New("name a code or a label")
	}
	for i, inv := range f.Invites {
		if inv.Code == ref {
			return i, nil
		}
	}
	for _, match := range []func(label string) bool{
		func(l string) bool { return strings.EqualFold(l, ref) },
		func(l string) bool { return partial && strings.Contains(strings.ToLower(l), strings.ToLower(ref)) },
	} {
		found := -1
		for i, inv := range f.Invites {
			if match(inv.Label) && !inv.Revoked {
				if found >= 0 {
					return 0, fmt.Errorf("%q matches more than one invite; use a code", ref)
				}
				found = i
			}
		}
		if found >= 0 {
			return found, nil
		}
	}
	if !partial {
		return 0, fmt.Errorf("no invite with code or full label %q (revoke takes the whole of either)", ref)
	}
	return 0, fmt.Errorf("no invite with code or label %q", ref)
}

func loadTrialFile(path string) (*api.TrialFile, error) {
	f, err := api.LoadTrialFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &api.TrialFile{}, nil
	}
	return f, err
}

func inviteList(o *trialOpts, fs *flag.FlagSet) func(io.Writer) error {
	return func(stdout io.Writer) error {
		f, err := loadTrialFile(o.config)
		if err != nil {
			return err
		}
		use, err := api.ReadInviteUse(o.jobs)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "LABEL\tCODE\tRUNS\tSTATE\tLAST USED")
		for _, inv := range f.Invites {
			u := use[api.InviteDigest(inv.Code)]
			used, last := 0, "never"
			if u != nil {
				used = u.Count
				if !u.Last.IsZero() {
					last = u.Last.Format("2006-01-02 15:04Z")
				}
			}
			state := "active"
			if inv.Revoked {
				state = "revoked"
			} else if used >= inv.Runs {
				state = "spent"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\n", inv.Label, inv.Code, used, inv.Runs, state, last)
		}
		return tw.Flush()
	}
}

func inviteShow(o *trialOpts, fs *flag.FlagSet) func(io.Writer) error {
	return func(stdout io.Writer) error {
		f, err := loadTrialFile(o.config)
		if err != nil {
			return err
		}
		i, err := findInvite(f, fs.Arg(0), true)
		if err != nil {
			return err
		}
		inv := f.Invites[i]
		use, err := api.ReadInviteUse(o.jobs)
		if err != nil {
			return err
		}
		u := use[api.InviteDigest(inv.Code)]
		fmt.Fprintf(stdout, "label   %s\ncode    %s\nurl     %s\ncreated %s\nrevoked %v\nnote    %s\n", inv.Label, inv.Code, o.shareURL(inv.Code), inv.Created.Format(time.RFC3339), inv.Revoked, inv.Note)
		if u == nil {
			u = &api.InviteUse{}
		}
		fmt.Fprintf(stdout, "runs    %d of %d\n", u.Count, inv.Runs)
		for _, j := range u.Jobs {
			fmt.Fprintf(stdout, "  %s\n", j)
		}
		return nil
	}
}

func inviteRevoke(o *trialOpts, fs *flag.FlagSet) func(io.Writer) error {
	return func(stdout io.Writer) error {
		var inv api.TrialInvite
		err := api.UpdateTrialFile(o.config, func(f *api.TrialFile) error {
			i, err := findInvite(f, fs.Arg(0), false)
			if err != nil {
				return err
			}
			f.Invites[i].Revoked = true
			inv = f.Invites[i]
			return nil
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "revoked %s (%s); sessions that redeemed it can start no more runs\n", inv.Code, inv.Label)
		return nil
	}
}

// list edits shared by task and model: adding what is missing, removing what
// is there, and refusing to empty a list, which would mean "no limit" or the
// environment's value.
func editList(list []string, seed []string, args []string, add bool) ([]string, error) {
	if len(list) == 0 {
		list = slices.Clone(seed)
	}
	if len(args) == 0 {
		return nil, errors.New("name at least one")
	}
	for _, a := range args {
		switch i := slices.Index(list, a); {
		case add && i < 0:
			list = append(list, a)
		case !add && i >= 0:
			list = slices.Delete(list, i, i+1)
		case !add:
			return nil, fmt.Errorf("%q is not in the list", a)
		}
	}
	if len(list) == 0 {
		return nil, errors.New("that would empty the list, which means no limit; add what should replace it first")
	}
	return list, nil
}

// trialArms are the arms every trial task must have: both sides of an
// incident. A feature task also has workflow and kavach-implement arms.
var trialArms = []string{"control", "kavach"}

// resolveTasks maps what the operator typed to Harbor task names. A base
// name means every arm it has, which must include control and kavach; a
// folder name means itself.
func resolveTasks(all []harbor.Task, args []string) (names []string, notes []string, err error) {
	for _, a := range args {
		var task *harbor.Task
		var folder string
		for i, t := range all {
			if t.Name == a {
				task = &all[i]
			}
			for _, arm := range t.Arms {
				if arm.Task == a {
					task, folder = &all[i], a
				}
			}
		}
		if task == nil {
			return nil, nil, fmt.Errorf("no task %q under the tasks directory", a)
		}
		var have []string
		for _, arm := range task.Arms {
			have = append(have, arm.Arm)
		}
		notes = append(notes, fmt.Sprintf("%s: arms %s", task.Name, strings.Join(have, ", ")))
		if folder != "" {
			names = append(names, folder)
			continue
		}
		for _, want := range trialArms {
			if !slices.Contains(have, want) {
				return nil, nil, fmt.Errorf("task %s has no %s arm (it has %s)", a, want, strings.Join(have, ", "))
			}
		}
		for _, arm := range task.Arms {
			names = append(names, arm.Task)
		}
	}
	return names, notes, nil
}

var imageRef = regexp.MustCompile(`(?m)^\s*docker_image\s*=\s*"([^"]+)"`)

// imagePublished says whether ref is in its registry. A variable so tests do
// not need a registry.
var imagePublished = func(ref string) (bool, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return false, errors.New("docker is not installed here")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "manifest", "inspect", ref).Run() == nil, nil
}

// warnImages tells the operator when a task's prebuilt images are not
// published: a remote sandbox cannot build them, so every run would fail.
func warnImages(w io.Writer, tasksDir string, names []string) {
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(tasksDir, n, "task.toml"))
		if err != nil {
			continue
		}
		refs := imageRef.FindAllSubmatch(b, -1)
		if len(refs) == 0 {
			fmt.Fprintf(w, "warning: %s names no docker_image; a remote sandbox cannot build it\n", n)
		}
		for _, m := range refs {
			ref := string(m[1])
			switch ok, err := imagePublished(ref); {
			case err != nil:
				fmt.Fprintf(w, "warning: could not check that %s is published (%v)\n", ref, err)
			case !ok:
				fmt.Fprintf(w, "warning: %s is not published; run the bench-images workflow before visitors use %s\n", ref, n)
			}
		}
	}
}

func taskEdit(o *trialOpts, fs *flag.FlagSet, add bool) func(io.Writer) error {
	return func(stdout io.Writer) error {
		dir, err := o.tasksDir()
		if err != nil {
			return err
		}
		all, err := harbor.FindTasks(dir)
		if err != nil {
			return err
		}
		var names, notes []string
		if add {
			if names, notes, err = resolveTasks(all, fs.Args()); err != nil {
				return err
			}
		} else {
			// A base name removes whichever of its arms are listed.
			for _, a := range fs.Args() {
				names = append(names, a)
				for _, arm := range harbor.Arms {
					names = append(names, a+"-"+arm)
				}
			}
		}
		var final []string
		err = api.UpdateTrialFile(o.config, func(f *api.TrialFile) error {
			if add {
				list, err := editList(f.Tasks, o.envList("TRIAL_TASKS"), names, true)
				f.Tasks, final = list, list
				return err
			}
			list := f.Tasks
			if len(list) == 0 {
				list = o.envList("TRIAL_TASKS")
			}
			before := len(list)
			list = slices.DeleteFunc(slices.Clone(list), func(s string) bool { return slices.Contains(names, s) })
			if len(list) == before {
				return fmt.Errorf("none of %s is in the trial's tasks", strings.Join(fs.Args(), ", "))
			}
			if len(list) == 0 {
				return errors.New("that would empty the list, which means every task; add what should replace it first")
			}
			f.Tasks, final = list, list
			return nil
		})
		if err != nil {
			return err
		}
		for _, n := range notes {
			fmt.Fprintln(stdout, n)
		}
		if add {
			warnImages(stdout, dir, names)
		}
		fmt.Fprintf(stdout, "trial tasks: %s\n", strings.Join(final, ", "))
		return nil
	}
}

func taskList(o *trialOpts, fs *flag.FlagSet) func(io.Writer) error {
	return func(stdout io.Writer) error {
		f, err := loadTrialFile(o.config)
		if err != nil {
			return err
		}
		list, src := f.Tasks, "the config file"
		if len(list) == 0 {
			list, src = o.envList("TRIAL_TASKS"), "TRIAL_TASKS"
		}
		if len(list) == 0 {
			fmt.Fprintln(stdout, "the trial allows every task under the tasks directory")
			return nil
		}
		fmt.Fprintf(stdout, "from %s:\n", src)
		dir, derr := o.tasksDir()
		var all []harbor.Task
		if derr == nil {
			all, _ = harbor.FindTasks(dir)
		}
		for _, name := range list {
			arms := "not found in the tasks directory"
			for _, t := range all {
				for _, a := range t.Arms {
					if a.Task == name || t.Name == name {
						arms = "arm " + a.Arm
						if t.Name == name {
							arms = "every arm"
						}
					}
				}
			}
			fmt.Fprintf(stdout, "  %s (%s)\n", name, arms)
		}
		return nil
	}
}

func modelEdit(o *trialOpts, fs *flag.FlagSet, add bool) func(io.Writer) error {
	return func(stdout io.Writer) error {
		if add {
			for _, m := range fs.Args() {
				if _, ok := api.ModelKeyVar(m); !ok {
					return fmt.Errorf("model %q: the provider before the first slash is not one the trial has a key variable for", m)
				}
			}
		}
		var final []string
		err := api.UpdateTrialFile(o.config, func(f *api.TrialFile) error {
			list, err := editList(f.Models, o.envList("TRIAL_MODELS"), fs.Args(), add)
			f.Models, final = list, list
			return err
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "trial models: %s\n", strings.Join(final, ", "))
		return nil
	}
}

func modelList(o *trialOpts, fs *flag.FlagSet) func(io.Writer) error {
	return func(stdout io.Writer) error {
		f, err := loadTrialFile(o.config)
		if err != nil {
			return err
		}
		list, src := f.Models, "the config file"
		if len(list) == 0 {
			list, src = o.envList("TRIAL_MODELS"), "TRIAL_MODELS"
		}
		if len(list) == 0 {
			fmt.Fprintln(stdout, "no models set: the trial allows its default, openrouter/z-ai/glm-5.3")
			return nil
		}
		fmt.Fprintf(stdout, "from %s:\n", src)
		for _, m := range list {
			fmt.Fprintf(stdout, "  %s\n", m)
		}
		return nil
	}
}
