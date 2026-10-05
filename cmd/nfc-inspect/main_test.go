package main

import (
	"bytes"
	"testing"
)

func TestOptions(t *testing.T) {
	for _, args := range [][]string{nil, {"--format", "csv", "--exclusive"}, {"--exclusive", "--max-bytes", "0"}, {"--exclusive", "--count", "0"}, {"--exclusive", "--timeout", "0s"}, {"--exclusive", "surprise"}} {
		if _, err := parseOptions(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	o, err := parseOptions([]string{"--exclusive"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if o.device != "/dev/pn5xx_i2c2" || o.readMemory || o.count != 1 {
		t.Fatalf("unsafe defaults %+v", o)
	}
}
