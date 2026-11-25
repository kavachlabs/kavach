package kavachredis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

type recordedZ struct {
	Score  float64 `json:"score"`
	Member any     `json:"member"`
}

type recordedKV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type recordedReply struct {
	Kind       string            `json:"kind"`
	IsNil      bool              `json:"is_nil,omitempty"`
	Str        string            `json:"str,omitempty"`
	Int        int64             `json:"int,omitempty"`
	Uint       uint64            `json:"uint,omitempty"`
	Float      float64           `json:"float,omitempty"`
	Bool       bool              `json:"bool,omitempty"`
	DurNanos   int64             `json:"dur_nanos,omitempty"`
	Time       string            `json:"time,omitempty"`
	StrSlice   []string          `json:"str_slice,omitempty"`
	IntSlice   []int64           `json:"int_slice,omitempty"`
	FloatSlice []float64         `json:"float_slice,omitempty"`
	BoolSlice  []bool            `json:"bool_slice,omitempty"`
	AnySlice   []any             `json:"any_slice,omitempty"`
	MapSS      map[string]string `json:"map_ss,omitempty"`
	MapSI      map[string]int64  `json:"map_si,omitempty"`
	MapSA      map[string]any    `json:"map_sa,omitempty"`
	StrSet     []string          `json:"str_set,omitempty"`
	ZSlice     []recordedZ       `json:"z_slice,omitempty"`
	KVs        []recordedKV      `json:"kvs,omitempty"`
	Cursor     uint64            `json:"cursor,omitempty"`
	AnyVal     any               `json:"any_val,omitempty"`
}

func encodeCmdReply(cmd redis.Cmder) ([]byte, error) {
	var r recordedReply
	switch c := cmd.(type) {
	case *redis.StringCmd:
		r = recordedReply{Kind: "string", Str: c.Val()}
	case *redis.StatusCmd:
		r = recordedReply{Kind: "status", Str: c.Val()}
	case *redis.IntCmd:
		r = recordedReply{Kind: "int", Int: c.Val()}
	case *redis.BoolCmd:
		r = recordedReply{Kind: "bool", Bool: c.Val()}
	case *redis.FloatCmd:
		r = recordedReply{Kind: "float", Float: c.Val()}
	case *redis.DurationCmd:
		r = recordedReply{Kind: "duration", DurNanos: int64(c.Val())}
	case *redis.TimeCmd:
		r = recordedReply{Kind: "time", Time: c.Val().UTC().Format(time.RFC3339Nano)}
	case *redis.SliceCmd:
		r = recordedReply{Kind: "slice", AnySlice: c.Val()}
	case *redis.StringSliceCmd:
		r = recordedReply{Kind: "string_slice", StrSlice: c.Val()}
	case *redis.IntSliceCmd:
		r = recordedReply{Kind: "int_slice", IntSlice: c.Val()}
	case *redis.FloatSliceCmd:
		r = recordedReply{Kind: "float_slice", FloatSlice: c.Val()}
	case *redis.BoolSliceCmd:
		r = recordedReply{Kind: "bool_slice", BoolSlice: c.Val()}
	case *redis.MapStringStringCmd:
		r = recordedReply{Kind: "map_ss", MapSS: c.Val()}
	case *redis.MapStringIntCmd:
		r = recordedReply{Kind: "map_si", MapSI: c.Val()}
	case *redis.MapStringInterfaceCmd:
		r = recordedReply{Kind: "map_sa", MapSA: c.Val()}
	case *redis.StringStructMapCmd:
		m := c.Val()
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		r = recordedReply{Kind: "str_set", StrSet: keys}
	case *redis.ZSliceCmd:
		zs := c.Val()
		out := make([]recordedZ, len(zs))
		for i, z := range zs {
			out[i] = recordedZ{Score: z.Score, Member: z.Member}
		}
		r = recordedReply{Kind: "z_slice", ZSlice: out}
	case *redis.KeyValueSliceCmd:
		kvs := c.Val()
		out := make([]recordedKV, len(kvs))
		for i, kv := range kvs {
			out[i] = recordedKV{Key: kv.Key, Value: kv.Value}
		}
		r = recordedReply{Kind: "kv_slice", KVs: out}
	case *redis.ScanCmd:
		page, cursor := c.Val()
		r = recordedReply{Kind: "scan", StrSlice: page, Cursor: cursor}
	case *redis.Cmd:
		r = recordedReply{Kind: "cmd", AnyVal: c.Val()}
	default:
		r = recordedReply{Kind: "unknown"}
	}
	return json.Marshal(r)
}

func decodeCmdReply(cmd redis.Cmder, data []byte) error {
	if len(data) == 0 {
		cmd.SetErr(nil)
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var r recordedReply
	if err := dec.Decode(&r); err != nil {
		// Allow simple raw string/int JSON values in hand-crafted fixtures.
		return decodeRawFallback(cmd, data)
	}
	if r.IsNil {
		cmd.SetErr(redis.Nil)
		return redis.Nil
	}

	switch c := cmd.(type) {
	case *redis.StringCmd:
		c.SetVal(r.Str)
	case *redis.StatusCmd:
		c.SetVal(r.Str)
	case *redis.IntCmd:
		c.SetVal(r.Int)
	case *redis.BoolCmd:
		c.SetVal(r.Bool)
	case *redis.FloatCmd:
		c.SetVal(r.Float)
	case *redis.DurationCmd:
		c.SetVal(time.Duration(r.DurNanos))
	case *redis.TimeCmd:
		if r.Time != "" {
			if t, err := time.Parse(time.RFC3339Nano, r.Time); err == nil {
				c.SetVal(t.UTC())
			}
		}
	case *redis.SliceCmd:
		c.SetVal(normalizeAnySlice(r.AnySlice))
	case *redis.StringSliceCmd:
		c.SetVal(r.StrSlice)
	case *redis.IntSliceCmd:
		c.SetVal(r.IntSlice)
	case *redis.FloatSliceCmd:
		c.SetVal(r.FloatSlice)
	case *redis.BoolSliceCmd:
		c.SetVal(r.BoolSlice)
	case *redis.MapStringStringCmd:
		if r.MapSS == nil {
			r.MapSS = map[string]string{}
		}
		c.SetVal(r.MapSS)
	case *redis.MapStringIntCmd:
		if r.MapSI == nil {
			r.MapSI = map[string]int64{}
		}
		c.SetVal(r.MapSI)
	case *redis.MapStringInterfaceCmd:
		c.SetVal(normalizeAnyMap(r.MapSA))
	case *redis.StringStructMapCmd:
		m := make(map[string]struct{}, len(r.StrSet))
		for _, k := range r.StrSet {
			m[k] = struct{}{}
		}
		c.SetVal(m)
	case *redis.ZSliceCmd:
		zs := make([]redis.Z, len(r.ZSlice))
		for i, rz := range r.ZSlice {
			zs[i] = redis.Z{Score: rz.Score, Member: normalizeAny(rz.Member)}
		}
		c.SetVal(zs)
	case *redis.KeyValueSliceCmd:
		kvs := make([]redis.KeyValue, len(r.KVs))
		for i, kv := range r.KVs {
			kvs[i] = redis.KeyValue{Key: kv.Key, Value: kv.Value}
		}
		c.SetVal(kvs)
	case *redis.ScanCmd:
		c.SetVal(r.StrSlice, r.Cursor)
	case *redis.Cmd:
		c.SetVal(normalizeAny(r.AnyVal))
	}
	cmd.SetErr(nil)
	return nil
}

func decodeRawFallback(cmd redis.Cmder, data []byte) error {
	s := string(bytes.TrimSpace(data))
	switch c := cmd.(type) {
	case *redis.StringCmd:
		var v string
		if json.Unmarshal(data, &v) == nil {
			c.SetVal(v)
		} else {
			c.SetVal(s)
		}
	case *redis.StatusCmd:
		var v string
		if json.Unmarshal(data, &v) == nil {
			c.SetVal(v)
		} else {
			c.SetVal(s)
		}
	case *redis.IntCmd:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			c.SetVal(n)
		}
	case *redis.BoolCmd:
		if b, err := strconv.ParseBool(s); err == nil {
			c.SetVal(b)
		}
	case *redis.FloatCmd:
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			c.SetVal(f)
		}
	case *redis.Cmd:
		var v any
		if json.Unmarshal(data, &v) == nil {
			c.SetVal(v)
		} else {
			c.SetVal(s)
		}
	default:
		return fmt.Errorf("kavachredis: cannot decode reply for %T", cmd)
	}
	cmd.SetErr(nil)
	return nil
}

func normalizeAny(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	case []any:
		return normalizeAnySlice(x)
	case map[string]any:
		return normalizeAnyMap(x)
	default:
		return v
	}
}

func normalizeAnySlice(in []any) []any {
	if in == nil {
		return nil
	}
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = normalizeAny(v)
	}
	return out
}

func normalizeAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = normalizeAny(v)
	}
	return out
}
