package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// cmdSync backs finished runs up to R2 once, without a server: the way to
// fill the bucket with the runs that exist. A run that is running or paused
// is left alone: it syncs when its Harbor has finished.
func cmdSync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("bench sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	o.registerDirs(fs)
	jobs := fs.String("job", "", "comma-separated runs to sync (default: every finished run)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := o.config()
	if err != nil {
		fmt.Fprintf(stderr, "bench sync: %v\n", err)
		return 2
	}
	sy := api.New(cfg).Syncer()
	if reason := sy.Off(); reason != "" {
		fmt.Fprintf(stderr, "bench sync: R2 sync is off: %s\n", reason)
		return 2
	}
	var ids []string
	if *jobs != "" {
		for _, id := range strings.Split(*jobs, ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	} else {
		all, err := sy.Reader.Jobs()
		if err != nil {
			fmt.Fprintf(stderr, "bench sync: %v\n", err)
			return 1
		}
		for _, j := range all {
			ids = append(ids, j.ID)
		}
	}
	code := 0
	for _, id := range ids {
		res, err := sy.RunOnce(context.Background(), id)
		switch {
		case errors.Is(err, harbor.ErrSyncBusy):
			fmt.Fprintf(stdout, "%s: running or paused, left alone\n", id)
		case err != nil:
			fmt.Fprintf(stdout, "%s: failed: %v\n", id, err)
			code = 1
		default:
			fmt.Fprintf(stdout, "%s: %s\n", id, res)
		}
	}
	return code
}
