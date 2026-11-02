package hal

import (
	"bytes"
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

func discoveryController() *PN7150 {
	p := &PN7150{tags: make([]Tag, maxTags), rxBuf: make([]byte, nciBufferSize)}
	p.storeState(stateDiscovering)
	return p
}

func discoveryFrame(notificationType byte) []byte {
	return []byte{0x61, 3, 12, 1, 2, 0, 8, 0x44, 0, 4, 1, 2, 3, 4, notificationType}
}

func TestDiscoveryObservations(t *testing.T) {
	for _, tc := range []struct {
		name      string
		frame     []byte
		kind      DiscoveryKind
		wantError bool
	}{
		{"silence", nil, DiscoveryQuiet, false},
		{"credits", []byte{0x60, 6, 3, 1, 0, 1}, DiscoveryPending, false},
		{"status", []byte{0x68, 0, 1, 0}, DiscoveryPending, false},
		{"partial discovery", discoveryFrame(2), DiscoveryPending, false},
		{"completed discovery", discoveryFrame(0), DiscoveryTags, false},
		{"host deactivation", []byte{0x61, 6, 2, 0, 0}, DiscoveryDeactivated, false},
		{"RF link loss", []byte{0x61, 6, 2, 3, 2}, DiscoveryDeactivated, false},
		{"activation failure", []byte{0x60, 7, 1, 0xa1}, DiscoveryPending, true},
		{"RF timeout", []byte{0x60, 8, 2, 0xb2, 0}, DiscoveryPending, true},
		{"controller reset", []byte{0x60, 0, 2, 0, 0}, DiscoveryPending, true},
		{"truncated header", []byte{0x61, 5}, DiscoveryPending, true},
		{"truncated payload", []byte{0x61, 5, 20}, DiscoveryPending, true},
		{"short deactivation", []byte{0x61, 6, 0}, DiscoveryPending, true},
		{"response instead of notification", []byte{0x41, 3, 1, 0}, DiscoveryPending, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := discoveryController()
			r, err := p.consumeDiscovery(tc.frame, func() ([]byte, error) { return nil, nil })
			if (err != nil) != tc.wantError || r.Kind != tc.kind {
				t.Fatalf("result=%+v error=%v", r, err)
			}
			if tc.wantError && !IsHALError(err) {
				t.Fatalf("malformed/controller error not recoverable: %T", err)
			}
			if r.Kind == DiscoveryDeactivated && (r.DeactivationType != tc.frame[3] || r.DeactivationReason != tc.frame[4]) {
				t.Fatal("lost deactivation reason")
			}
		})
	}
}

func TestDiscoveryPreservesFollowupErrorsAndPartialEvidence(t *testing.T) {
	p := discoveryController()
	failure := NewI2CReadError("failed activation read", nil)
	if _, err := p.consumeDiscovery(discoveryFrame(0), func() ([]byte, error) { return nil, failure }); !errors.Is(err, failure) {
		t.Fatalf("lost followup error: %v", err)
	}
	p = discoveryController()
	if r, err := p.consumeDiscovery(discoveryFrame(2), nil); err != nil || r.Kind != DiscoveryPending {
		t.Fatalf("partial=%+v %v", r, err)
	}
	if r, err := p.consumeDiscovery(nil, nil); err != nil || r.Kind != DiscoveryPending {
		t.Fatalf("silence hid an incomplete target list: %+v %v", r, err)
	}
	if _, err := p.consumeDiscovery(discoveryFrame(0), nil); !IsMultipleTagsError(err) {
		t.Fatalf("multiple tags became absence: %v", err)
	}
}

func discoveryActivationFrame() []byte {
	return []byte{0x61, 5, 23, 7, 1, 2, 0, 255, 1, 12, 0x44, 0, 7, 4, 1, 2, 3, 4, 5, 6, 1, 0, 0, 0, 0, 0}
}

func TestDiscoveryActivationAndCache(t *testing.T) {
	p := discoveryController()
	frame := discoveryActivationFrame()
	r, err := p.consumeDiscovery(discoveryFrame(0), func() ([]byte, error) { return frame, nil })
	if err != nil || r.Kind != DiscoveryTags || r.Cached || !p.tagSelected {
		t.Fatalf("activation=%+v %v", r, err)
	}
	uid := append([]byte(nil), r.Tags[0].ID...)
	r.Tags[0].ID[0] ^= 0xff
	r, err = p.consumeDiscovery(nil, nil)
	if err != nil || r.Kind != DiscoveryTags || !r.Cached || !bytes.Equal(uid, r.Tags[0].ID) {
		t.Fatalf("cache=%+v %v", r, err)
	}
	if _, err := p.consumeDiscovery([]byte{0x60, 8, 2, 0xb2, 0}, nil); err == nil {
		t.Fatal("cached tag hid a controller error")
	}
	r, err = p.consumeDiscovery([]byte{0x61, 6, 2, 3, 2}, nil)
	if err != nil || r.Kind != DiscoveryDeactivated || p.numTags != 0 || p.tagSelected {
		t.Fatalf("deactivation=%+v %v", r, err)
	}
	r, err = p.consumeDiscovery(nil, nil)
	if err != nil || r.Kind != DiscoveryQuiet || len(r.Tags) != 0 {
		t.Fatalf("post-deactivation=%+v %v", r, err)
	}
}

func TestPollDiscoveryTransport(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	p := discoveryController()
	p.fd = fds[0]
	r, err := p.PollDiscovery()
	if err != nil || r.Kind != DiscoveryQuiet {
		t.Fatalf("quiet=%+v %v", r, err)
	}
	frame := discoveryActivationFrame()
	if _, err := unix.Write(fds[1], frame); err != nil {
		t.Fatal(err)
	}
	r, err = p.PollDiscovery()
	if err != nil || r.Kind != DiscoveryTags || r.Cached {
		t.Fatalf("activation=%+v %v", r, err)
	}
	if err := unix.Shutdown(fds[1], unix.SHUT_RDWR); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PollDiscovery(); !IsHALError(err) {
		t.Fatalf("transport hangup became a discovery result: %v", err)
	}
	p.storeState(stateIdle)
	if _, err := p.PollDiscovery(); !IsHALError(err) {
		t.Fatalf("idle poll=%v", err)
	}
}

func TestDiscoveryFinalNotificationFollowedByCredits(t *testing.T) {
	p := discoveryController()
	r, err := p.consumeDiscovery(discoveryFrame(0), func() ([]byte, error) { return []byte{0x60, 6, 3, 1, 0, 1}, nil })
	if err != nil || r.Kind != DiscoveryTags || len(r.Tags) != 1 {
		t.Fatalf("completed tag list was lost behind credits: %+v %v", r, err)
	}
}

func TestLegacyDetectTagsUsesExplicitDiscoveryParser(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	p := discoveryController()
	p.fd = fds[0]
	if tags, err := p.DetectTags(); err != nil || len(tags) != 0 {
		t.Fatalf("legacy quiet: %v %v", tags, err)
	}
	frame := discoveryActivationFrame()
	if _, err := unix.Write(fds[1], frame); err != nil {
		t.Fatal(err)
	}
	if tags, err := p.DetectTags(); err != nil || len(tags) != 1 {
		t.Fatalf("legacy tag: %v %v", tags, err)
	}
	if _, err := unix.Write(fds[1], []byte{0x60, 8, 2, 0xb2, 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.DetectTags(); !IsHALError(err) {
		t.Fatalf("legacy controller error hidden: %v", err)
	}
}

func FuzzDiscoveryNotification(f *testing.F) {
	f.Add(discoveryFrame(0))
	f.Add([]byte{0x61, 6, 2, 3, 2})
	f.Add(discoveryActivationFrame())
	f.Fuzz(func(t *testing.T, data []byte) {
		p := discoveryController()
		_, _ = p.consumeDiscovery(data, func() ([]byte, error) { return nil, nil })
	})
}
