package hal

import (
	"bytes"
	"testing"
)

func TestParseAPDUData(t *testing.T) {
	want := []byte{0x90, 0x00}
	frame := []byte{0x00, 0x00, 0x02, 0x90, 0x00}
	got, err := parseAPDUData(frame)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("valid DATA: %X, %v", got, err)
	}
	frame[3] = 0x6A
	if !bytes.Equal(got, want) {
		t.Fatal("returned slice aliases receive buffer")
	}
	for _, bad := range [][]byte{
		{0x60, 0x06, 0x02, 0x90, 0x00},
		{0x00, 0x01, 0x02, 0x90, 0x00},
		{0x00, 0x00, 0x04, 0x90, 0x00},
		{0x00, 0x00, 0x00},
	} {
		if _, err := parseAPDUData(bad); err == nil {
			t.Fatalf("accepted invalid NCI frame: %X", bad)
		}
	}
}
