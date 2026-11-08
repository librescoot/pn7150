package hal

import "testing"

func TestTagDetectorSetting(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		want    byte
	}{
		{false, 0x00},
		{true, 0x01},
	} {
		if got := tagDetectorSetting(tc.enabled); got != tc.want {
			t.Errorf("tagDetectorSetting(%t) = %#x, want %#x", tc.enabled, got, tc.want)
		}
	}
}
