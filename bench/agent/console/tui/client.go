package tui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kavachlabs/kavach/bench/agent/console/api"
	"github.com/kavachlabs/kavach/bench/agent/console/harbor"
)

// client talks to the console's HTTP API (API.md). The terminal console uses
// nothing else, so it sees exactly what the web console sees.
type client struct {
	base string // http://127.0.0.1:4810
	http *http.Client
}

func newClient(base string) *client {
	return &client{base: strings.TrimRight(base, "/"), http: &http.Client{Timeout: 30 * time.Second}}
}

// errNotFound is a 404 from the API.
var errNotFound = errors.New("not found")

func (c *client) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, v)
}

func (c *client) post(ctx context.Context, path string, body, v any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, v)
}

func decode(resp *http.Response, v any) error {
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode/100 != 2 {
		var e struct{ Error string }
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func (c *client) status(ctx context.Context) (api.Status, error) {
	var s api.Status
	return s, c.get(ctx, "/api/status", &s)
}

func (c *client) tasks(ctx context.Context) ([]harbor.Task, error) {
	var t []harbor.Task
	return t, c.get(ctx, "/api/tasks", &t)
}

func (c *client) models(ctx context.Context) ([]api.Model, error) {
	var m []api.Model
	return m, c.get(ctx, "/api/models", &m)
}

func (c *client) jobs(ctx context.Context) ([]harbor.Job, error) {
	var j []harbor.Job
	return j, c.get(ctx, "/api/jobs", &j)
}

func (c *client) job(ctx context.Context, id string) (harbor.Job, error) {
	var j harbor.Job
	return j, c.get(ctx, "/api/jobs/"+url.PathEscape(id), &j)
}

func (c *client) compare(ctx context.Context, id string) ([]harbor.CompareRow, error) {
	var rows []harbor.CompareRow
	return rows, c.get(ctx, "/api/jobs/"+url.PathEscape(id)+"/compare", &rows)
}

func (c *client) trialPath(job, trial string) string {
	return "/api/trials/" + url.PathEscape(job) + "/" + url.PathEscape(trial)
}

func (c *client) trial(ctx context.Context, job, trial string) (harbor.Trial, error) {
	var t harbor.Trial
	return t, c.get(ctx, c.trialPath(job, trial), &t)
}

func (c *client) steps(ctx context.Context, job, trial string) ([]harbor.Step, error) {
	var s []harbor.Step
	return s, c.get(ctx, c.trialPath(job, trial)+"/steps", &s)
}

func (c *client) grade(ctx context.Context, job, trial string) (harbor.Grade, error) {
	var g harbor.Grade
	return g, c.get(ctx, c.trialPath(job, trial)+"/grade", &g)
}

func (c *client) start(ctx context.Context, req harbor.RunRequest) (harbor.Job, error) {
	var j harbor.Job
	return j, c.post(ctx, "/api/jobs", req, &j)
}

func (c *client) stop(ctx context.Context, id string) error {
	return c.post(ctx, "/api/jobs/"+url.PathEscape(id)+"/stop", struct{}{}, nil)
}

// event is one Server-Sent Event from /api/events: a job (without trials)
// or a trial.
type event struct {
	job   *harbor.Job
	trial *harbor.Trial
}

// events streams /api/events into ch until ctx ends or the stream breaks.
func (c *client) events(ctx context.Context, ch chan<- event) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/events", nil)
	if err != nil {
		return err
	}
	// The stream is long-lived: c.http's timeout would cut it.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("events: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var name string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			name = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			data := []byte(line[len("data: "):])
			var e event
			switch name {
			case "job":
				e.job = new(harbor.Job)
				err = json.Unmarshal(data, e.job)
			case "trial":
				e.trial = new(harbor.Trial)
				err = json.Unmarshal(data, e.trial)
			default:
				continue
			}
			if err != nil {
				continue
			}
			select {
			case ch <- e:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.EOF
}
