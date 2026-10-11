package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{"nope"}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 || !strings.Contains(errb.String(), "bench api") {
			t.Errorf("%v: exit %d\n%s", args, code, errb.String())
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"api", "-port", "x"}, &out, &errb); code != 2 {
		t.Errorf("bad flag: exit %d", code)
	}
}
