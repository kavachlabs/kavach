// Package kavachredis provides a 1-line go-redis/v9 adapter for Kavach v0.2:
//   - Wrap(kv, rdb) installs a redis.Hook on *redis.Client, *redis.ClusterClient,
//     or any redis.UniversalClient while preserving its exact Go type.
//   - Record mode executes real Redis commands and captures Gateway records
//     ("redis:<cmd>", stable key args, typed reply, err).
//   - Replay mode NEVER dials Redis: DialHook, ProcessHook, and ProcessPipelineHook
//     short-circuit offline, serve recorded replies (or injected faults like
//     "LOADING Redis is loading the dataset in memory"), and handle startup PINGs cleanly.
package kavachredis

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/kavachlabs/kavach-go"
	"github.com/redis/go-redis/v9"
)

// ErrReplayDialDisabled is returned by DialHook if anything attempts to open a
// real Redis TCP socket while Kavach is in ModeReplay.
var ErrReplayDialDisabled = errors.New("kavachredis: network dial disabled in replay mode")

// Wrap attaches a Kavach redis.Hook to rdb and returns rdb with its concrete
// type preserved (e.g. *redis.Client -> *redis.Client, *redis.ClusterClient ->
// *redis.ClusterClient, redis.UniversalClient -> redis.UniversalClient).
func Wrap[T redis.UniversalClient](kv *kavach.Kavach, rdb T) T {
	if kv == nil || kv.IsOff() || isNilClient(rdb) {
		return rdb
	}
	rdb.AddHook(NewHook(kv))
	return rdb
}

// NewReplayClient returns a *redis.Client wired with kavachredis.NewHook(kv)
// that never dials any network socket in ModeReplay.
func NewReplayClient(kv *kavach.Kavach) *redis.Client {
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:0",
		DialTimeout: time.Millisecond,
		MaxRetries:  0,
	})
	return Wrap(kv, rdb)
}

// Hook implements redis.Hook for Kavach record and offline replay.
type Hook struct {
	kv *kavach.Kavach
}

var _ redis.Hook = (*Hook)(nil)

// NewHook returns a redis.Hook backed by kv.
func NewHook(kv *kavach.Kavach) redis.Hook {
	return &Hook{kv: kv}
}

// DialHook prevents any TCP dial to Redis in ModeReplay.
func (h *Hook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if h.kv != nil && h.kv.IsReplay() {
			return nil, ErrReplayDialDisabled
		}
		return next(ctx, network, addr)
	}
}

// ProcessHook intercepts single Redis commands.
func (h *Hook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if h.kv == nil || h.kv.IsOff() {
			return next(ctx, cmd)
		}
		if h.kv.IsReplay() {
			return h.replayCmd(ctx, cmd)
		}
		err := next(ctx, cmd)
		h.recordCmd(ctx, cmd, err)
		return err
	}
}

// ProcessPipelineHook intercepts Redis pipelines and TxPipelines.
func (h *Hook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if h.kv == nil || h.kv.IsOff() {
			return next(ctx, cmds)
		}
		if h.kv.IsReplay() {
			var firstErr error
			for _, cmd := range cmds {
				name := strings.ToLower(cmd.Name())
				if name == "multi" || name == "exec" {
					setHandshakeOK(cmd, "OK")
					continue
				}
				if err := h.replayCmd(ctx, cmd); err != nil && firstErr == nil {
					firstErr = err
				}
			}
			return firstErr
		}
		err := next(ctx, cmds)
		for _, cmd := range cmds {
			name := strings.ToLower(cmd.Name())
			if name == "multi" || name == "exec" {
				continue
			}
			h.recordCmd(ctx, cmd, cmd.Err())
		}
		return err
	}
}

func (h *Hook) buildGatewayCall(cmd redis.Cmder) kavach.GatewayCall {
	name := strings.ToLower(strings.TrimSpace(cmd.Name()))
	if name == "" {
		if args := cmd.Args(); len(args) > 0 {
			name = strings.ToLower(argString(args[0]))
		}
	}
	upper := strings.ToUpper(name)
	return kavach.GatewayCall{
		Driver:       "redis",
		Gateway:      "redis:" + name,
		Op:           upper,
		NormalizedOp: upper,
		KeyArgs:      extractKeyArgs(name, cmd.Args()),
	}
}

func (h *Hook) recordCmd(ctx context.Context, cmd redis.Cmder, execErr error) {
	if _, ok := h.kv.ActiveStep(ctx); !ok {
		return
	}
	call := h.buildGatewayCall(cmd)
	err := execErr
	if err == nil {
		err = cmd.Err()
	}
	if err != nil {
		call.Error = err.Error()
		h.kv.RecordGateway(ctx, call)
		return
	}
	encoded, encErr := encodeCmdReply(cmd)
	if encErr == nil {
		call.Response = encoded
	}
	h.kv.RecordGateway(ctx, call)
}

func (h *Hook) replayCmd(ctx context.Context, cmd redis.Cmder) error {
	name := strings.ToLower(strings.TrimSpace(cmd.Name()))
	if _, ok := h.kv.ActiveStep(ctx); !ok {
		// Allow startup healthchecks / connection handshakes (like Lago's NewRedisDB Ping)
		// to succeed offline before any replay step begins.
		switch name {
		case "ping":
			setHandshakeOK(cmd, "PONG")
			return nil
		case "hello", "auth", "select", "client", "readonly", "readwrite", "echo":
			setHandshakeOK(cmd, "OK")
			return nil
		default:
			cmd.SetErr(kavach.ErrNoActiveStep)
			return kavach.ErrNoActiveStep
		}
	}

	call := h.buildGatewayCall(cmd)
	respBytes, err := h.kv.ReplayGateway(ctx, call)
	if err != nil {
		if err.Error() == redis.Nil.Error() {
			cmd.SetErr(redis.Nil)
			return redis.Nil
		}
		cmd.SetErr(err)
		return err
	}

	if err := decodeCmdReply(cmd, respBytes); err != nil {
		cmd.SetErr(err)
		return err
	}
	return cmd.Err()
}

func setHandshakeOK(cmd redis.Cmder, val string) {
	switch c := cmd.(type) {
	case *redis.StatusCmd:
		c.SetVal(val)
		c.SetErr(nil)
	case *redis.StringCmd:
		c.SetVal(val)
		c.SetErr(nil)
	case *redis.Cmd:
		c.SetVal(val)
		c.SetErr(nil)
	default:
		cmd.SetErr(nil)
	}
}

// extractKeyArgs extracts ONLY stable key arguments from a Redis command,
// deliberately omitting volatile values, TTLs, scores, and time-bucketed members
// (such as Lago FlagStore.Flag's ZADD score and "org:sub|bucket" member).
func extractKeyArgs(cmdName string, args []any) []string {
	if len(args) < 2 {
		return nil
	}
	switch cmdName {
	case "ping", "echo", "auth", "hello", "select", "client", "info", "dbsize", "multi", "exec", "discard", "quit":
		return nil
	case "hget", "hexists", "hincrby", "hincrbyfloat":
		if len(args) >= 3 {
			return []string{argString(args[1]), argString(args[2])}
		}
		return []string{argString(args[1])}
	case "mget", "del", "unlink", "exists":
		keys := make([]string, 0, len(args)-1)
		for _, a := range args[1:] {
			keys = append(keys, argString(a))
		}
		return keys
	case "mset", "msetnx":
		keys := make([]string, 0, len(args)/2)
		for i := 1; i < len(args); i += 2 {
			keys = append(keys, argString(args[i]))
		}
		return keys
	case "eval", "evalsha", "eval_ro", "evalsha_ro":
		out := []string{argString(args[1])}
		if len(args) >= 3 {
			numKeys, _ := strconv.Atoi(argString(args[2]))
			for i := 0; i < numKeys && 3+i < len(args); i++ {
				out = append(out, argString(args[3+i]))
			}
		}
		return out
	default:
		// Standard single-key Redis commands: GET, SET, SETEX, EXPIRE, ZADD, ZREM,
		// ZRANGE, HSET, HGETALL, LPUSH, RPUSH, SADD, INCR, DECR, etc.
		return []string{argString(args[1])}
	}
}

func argString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func isNilClient[T redis.UniversalClient](rdb T) bool {
	v := reflect.ValueOf(rdb)
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func:
		return v.IsNil()
	}
	return false
}
