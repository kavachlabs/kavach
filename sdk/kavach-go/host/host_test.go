package host

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/kavachlabs/kavach-go/matcher"
)

func TestHostProtocolFullLifecycle(t *testing.T) {
	drvToHostR, drvToHostW := io.Pipe()
	hostToDrvR, hostToDrvW := io.Pipe()

	inits := 0
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- Serve(Options{
			SDKVersion: "kavach-go/0.2.0",
			InvariantNames: func() []string {
				return []string{"atomic_single_emit_on_retry"}
			},
			OnInit: func(service, start string, snapshot []byte) error {
				inits++
				return nil
			},
			RunStep: func(in StepInput, env *Env) StepOutcome {
				switch in.Source {
				case "ok":
					now := env.Now()
					var rnd [4]byte
					_, _ = env.ReadRand(rnd[:])
					cfgVal, present := env.Config("FEATURE_X")
					resp, err := env.Query("redis:get", []byte(`{"key":"k1"}`))
					if err != nil {
						return StepOutcome{Err: err}
					}
					// Emit concurrently from two goroutines to verify mutex safety under -race.
					var wg sync.WaitGroup
					wg.Add(2)
					go func() {
						defer wg.Done()
						env.Emit("kafka:out1", append([]byte(now.Format("2006-01-02")+":"), resp...))
					}()
					go func() {
						defer wg.Done()
						if present {
							env.Emit("kafka:out2", cfgVal)
						}
					}()
					wg.Wait()
					return StepOutcome{}
				case "fault":
					_, err := env.Query("redis:zadd", []byte(`{"key":"sub"}`))
					if err != nil {
						if !matcher.IsFault(err, matcher.CodeRedisLoading) {
							return StepOutcome{Err: errors.New("expected REDIS_LOADING fault")}
						}
						return StepOutcome{Err: err}
					}
					return StepOutcome{}
				case "panic":
					panic("boom")
				case "inv":
					return StepOutcome{InvariantName: "atomic_single_emit_on_retry", InvariantDetail: "duplicate tx-1"}
				case "abort":
					_ = env.Now()
					return StepOutcome{}
				default:
					return StepOutcome{}
				}
			},
		}, drvToHostR, hostToDrvW)
	}()

	reader := bufio.NewReader(hostToDrvR)
	send := func(msg map[string]any) {
		t.Helper()
		b, err := json.Marshal(msg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if _, err := drvToHostW.Write(append(b, '\n')); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	recv := func() map[string]any {
		t.Helper()
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read line: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		return m
	}

	// 1. Hello -> Ready
	send(map[string]any{"t": "hello", "protocol": 1, "service": "svc", "start": "genesis", "mode": "process"})
	ready := recv()
	if ready["t"] != "ready" || int(ready["protocol"].(float64)) != 1 || ready["sdk"] != "kavach-go/0.2.0" {
		t.Fatalf("unexpected ready: %+v", ready)
	}
	envMap, ok := ready["environment"].(map[string]any)
	if !ok || envMap["host.runtime"] == nil {
		t.Fatalf("expected host.runtime in environment, got %+v", ready["environment"])
	}

	// 2. Step "ok" exercising clock, rand, config, gateway, concurrent emits
	send(map[string]any{"t": "step", "seq": "1", "source": "ok", "position": "0:1", "data": base64.StdEncoding.EncodeToString([]byte("{}"))})
	if m := recv(); m["t"] != "clock" {
		t.Fatalf("want clock, got %+v", m)
	}
	send(map[string]any{"t": "clock", "unix_nanos": "1741007009000000000"})

	if m := recv(); m["t"] != "rand" || int(m["n"].(float64)) != 4 {
		t.Fatalf("want rand(4), got %+v", m)
	}
	send(map[string]any{"t": "rand", "data": base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4})})

	if m := recv(); m["t"] != "config" || m["key"] != "FEATURE_X" {
		t.Fatalf("want config(FEATURE_X), got %+v", m)
	}
	send(map[string]any{"t": "config", "present": true, "value": base64.StdEncoding.EncodeToString([]byte("on"))})

	if m := recv(); m["t"] != "gateway" || m["gateway"] != "redis:get" {
		t.Fatalf("want gateway(redis:get), got %+v", m)
	}
	send(map[string]any{"t": "gateway", "response": base64.StdEncoding.EncodeToString([]byte("okval"))})

	emits := 0
	for {
		m := recv()
		if m["t"] == "emit" {
			emits++
			continue
		}
		if m["t"] != "done" || m["outcome"] != "ok" {
			t.Fatalf("want done(ok), got %+v", m)
		}
		break
	}
	if emits != 2 {
		t.Fatalf("got %d emits, want 2", emits)
	}

	// 3. Step "fault" with injected REDIS_LOADING error
	send(map[string]any{"t": "step", "seq": "2", "source": "fault", "data": ""})
	if m := recv(); m["t"] != "gateway" {
		t.Fatalf("want gateway, got %+v", m)
	}
	send(map[string]any{"t": "gateway", "error": matcher.FaultRedisLoading})
	if m := recv(); m["t"] != "done" || m["outcome"] != "error" || !strings.Contains(m["message"].(string), "LOADING") {
		t.Fatalf("want done(error LOADING), got %+v", m)
	}

	// 4. Step "panic"
	send(map[string]any{"t": "step", "seq": "3", "source": "panic", "data": ""})
	if m := recv(); m["t"] != "done" || m["outcome"] != "panic" || m["message"] != "boom" || m["detail"] == "" {
		t.Fatalf("want done(panic boom), got %+v", m)
	}

	// 5. Step "inv"
	send(map[string]any{"t": "step", "seq": "4", "source": "inv", "data": ""})
	if m := recv(); m["t"] != "done" || m["outcome"] != "invariant" || m["message"] != "atomic_single_emit_on_retry" {
		t.Fatalf("want done(invariant), got %+v", m)
	}

	// 6. Step "abort"
	send(map[string]any{"t": "step", "seq": "5", "source": "abort", "data": ""})
	if m := recv(); m["t"] != "clock" {
		t.Fatalf("want clock before abort, got %+v", m)
	}
	send(map[string]any{"t": "abort", "detail": "nondeterministic"})
	if m := recv(); m["t"] != "done" || m["outcome"] != "aborted" {
		t.Fatalf("want done(aborted), got %+v", m)
	}

	// 7. Reset -> Ready
	send(map[string]any{"t": "reset", "protocol": 1, "service": "svc", "start": "genesis"})
	if m := recv(); m["t"] != "ready" {
		t.Fatalf("want ready after reset, got %+v", m)
	}

	// 8. End -> clean nil exit
	send(map[string]any{"t": "end"})
	if err := <-doneCh; err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	if inits != 2 {
		t.Fatalf("OnInit called %d times, want 2 (hello + reset)", inits)
	}
}

func TestHostFatalOnBadHello(t *testing.T) {
	in := bytes.NewBufferString(`{"t":"step","seq":"1"}` + "\n")
	var out bytes.Buffer
	err := Serve(Options{
		RunStep: func(StepInput, *Env) StepOutcome { return StepOutcome{} },
	}, in, &out)
	if err == nil {
		t.Fatalf("expected error on bad hello")
	}
	if !strings.Contains(out.String(), `"t":"fatal"`) {
		t.Fatalf("expected fatal wire message, got %q", out.String())
	}
}
