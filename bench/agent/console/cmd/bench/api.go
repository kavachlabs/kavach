package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
	"github.com/kavachlabs/kavach/bench/agent/console/web"
)

// options are the flags every serving subcommand shares.
type options struct {
	root, jobs, tasks, harbor string
	port                      int
}

func (o *options) register(fs *flag.FlagSet) {
	fs.StringVar(&o.root, "root", "", "the kavach repository (default: found from the current directory)")
	fs.StringVar(&o.jobs, "jobs", "", "Harbor's jobs directory (default: <root>/bench/agent/results/harbor)")
	fs.StringVar(&o.tasks, "tasks", "", "the Harbor tasks directory (default: <root>/bench/agent/harbor/tasks)")
	fs.StringVar(&o.harbor, "harbor", "harbor", "the harbor command")
	fs.IntVar(&o.port, "port", 4810, "port on 127.0.0.1")
}

// config resolves the directories. The jobs directory is made absolute:
// Harbor mounts trial folders into containers from it.
func (o *options) config() (api.Config, error) {
	root := o.root
	if root == "" {
		var err error
		if root, err = findRoot(); err != nil {
			return api.Config{}, err
		}
	}
	c := api.Config{
		JobsDir:  o.jobs,
		TasksDir: o.tasks,
		EnvFile:  filepath.Join(root, "bench", "agent", ".env"),
		Harbor:   o.harbor,
		Root:     root,
		Web:      web.Files,
	}
	if c.JobsDir == "" {
		c.JobsDir = filepath.Join(root, "bench", "agent", "results", "harbor")
	}
	if c.TasksDir == "" {
		c.TasksDir = filepath.Join(root, "bench", "agent", "harbor", "tasks")
	}
	var err error
	if c.JobsDir, err = filepath.Abs(c.JobsDir); err != nil {
		return c, err
	}
	c.TasksDir, err = filepath.Abs(c.TasksDir)
	c.JobsWarning = jobsWarning(c.JobsDir)
	return c, err
}

// jobsWarning flags a jobs directory outside the home directory. Colima,
// the usual Docker on a Mac, shares only the home directory with its VM, so
// the verifier's mounts of trial folders elsewhere are empty and grading
// finds no reward.
func jobsWarning(jobs string) string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if rel, err := filepath.Rel(home, jobs); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return fmt.Sprintf("The jobs folder %s is outside your home folder. Colima shares only your home folder with Docker, so attempts there can't be graded; pass -jobs with a folder under %s.", jobs, home)
}

// findRoot walks up from the current directory to the repository, the
// directory holding bench/agent.
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if st, err := os.Stat(filepath.Join(dir, "bench", "agent")); err == nil && st.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("not inside the kavach repository; pass --root")
		}
		dir = parent
	}
}

// serve serves h on 127.0.0.1:port until SIGINT or SIGTERM, calling ready,
// if set, with the server's URL once it listens. It never binds another
// address: the API starts paid runs.
func serve(h http.Handler, port int, stderr io.Writer, ready func(url string)) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, BaseContext: func(net.Listener) context.Context { return ctx }}
	url := "http://" + ln.Addr().String() + "/"
	fmt.Fprintf(stderr, "bench: serving on %s\n", url)
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	if ready != nil {
		go ready(url)
	}
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdAPI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench api", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	o.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := o.config()
	if err != nil {
		fmt.Fprintf(stderr, "bench api: %v\n", err)
		return 2
	}
	fmt.Fprintf(stderr, "bench: jobs %s, tasks %s\n", cfg.JobsDir, cfg.TasksDir)
	if err := serve(api.New(cfg), o.port, stderr, nil); err != nil {
		fmt.Fprintf(stderr, "bench api: %v\n", err)
		return 1
	}
	return 0
}
