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
	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
	"github.com/kavachlabs/kavach/bench/agent/console/web"
)

// options are the flags every serving subcommand shares.
type options struct {
	root, jobs, tasks, harbor string
	port                      int
}

func (o *options) register(fs *flag.FlagSet) {
	o.registerDirs(fs)
	fs.IntVar(&o.port, "port", 4810, "port on 127.0.0.1")
}

// registerDirs registers the flags that locate what a command reads.
func (o *options) registerDirs(fs *flag.FlagSet) {
	fs.StringVar(&o.root, "root", "", "the kavach repository (default: found from the current directory)")
	fs.StringVar(&o.jobs, "jobs", "", "Harbor's jobs directory (default: <root>/bench/agent/results/harbor)")
	fs.StringVar(&o.tasks, "tasks", "", "the Harbor tasks directory (default: <root>/bench/agent/harbor/tasks)")
	fs.StringVar(&o.harbor, "harbor", "harbor", "the harbor command")
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
	if home, err := os.UserHomeDir(); err == nil {
		c.Docker, c.CacheDir = harbor.CLIDocker{}, filepath.Join(home, ".cache", "kavach-bench", "src")
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

// loopbackAddr is 127.0.0.1:port.
func loopbackAddr(port int) string { return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)) }

// serve serves h on addr until SIGINT or SIGTERM, calling ready, if set,
// with the server's URL once it listens. Callers bind 127.0.0.1 unless the
// server is a trial: the API starts paid runs.
func serve(h http.Handler, addr string, stderr io.Writer, ready func(url string)) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", addr)
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

// syncingServer is a server that also backs finished runs up to R2.
func syncingServer(cfg api.Config) *api.Server {
	s := api.New(cfg)
	s.StartSync()
	return s
}

func cmdAPI(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench api", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	o.register(fs)
	trial := fs.Bool("trial", false, "serve a public trial: visitors run with their own keys (see API.md, Trial mode)")
	addr := fs.String("addr", "", "address to listen on (default 127.0.0.1:<port>); beyond loopback only with -trial")
	origin := fs.String("origin", "", "with -trial: the public origin writes may come from, e.g. https://try.kavachlabs.dev")
	trustCF := fs.Bool("trust-cf", false, "with -trial: take a client's address from CF-Connecting-IP")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	listen := *addr
	if listen == "" {
		listen = loopbackAddr(o.port)
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintf(stderr, "bench api: -addr: %v\n", err)
		return 2
	}
	ip := net.ParseIP(host)
	local := host == "localhost" || (ip != nil && ip.IsLoopback())
	switch {
	case !*trial && !local:
		fmt.Fprintln(stderr, "bench api: -addr beyond loopback needs -trial: the API starts paid runs")
		return 2
	case *trial && *trustCF && !local:
		fmt.Fprintln(stderr, "bench api: -trust-cf needs a loopback -addr: behind a tunnel on this machine, the header can be forged by anyone who reaches the port")
		return 2
	}
	cfg, err := o.config()
	if err != nil {
		fmt.Fprintf(stderr, "bench api: %v\n", err)
		return 2
	}
	if *trial {
		cfg.Trial = api.TrialFromEnv(os.Getenv)
		cfg.Trial.Origin, cfg.Trial.TrustCF = *origin, *trustCF
	}
	fmt.Fprintf(stderr, "bench: jobs %s, tasks %s\n", cfg.JobsDir, cfg.TasksDir)
	if err := serve(syncingServer(cfg), listen, stderr, nil); err != nil {
		fmt.Fprintf(stderr, "bench api: %v\n", err)
		return 1
	}
	return 0
}
