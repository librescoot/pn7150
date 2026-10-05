package hal

import (
	"bytes"
	"fmt"
	"reflect"
	"testing"

	"golang.org/x/sys/unix"
)

func activation(tech, protocol byte, params, act []byte) []byte {
	b := []byte{0x61, 5, 0, 7, 1, protocol, tech, 0xff, 1, byte(len(params))}
	if protocol == 4 {
		b[4] = 2
	}
	b = append(b, params...)
	b = append(b, tech, 0, 0, byte(len(act)))
	b = append(b, act...)
	b[2] = byte(len(b) - 3)
	return b
}

func TestDiagnosticActivation(t *testing.T) {
	cases := []struct {
		tech, protocol byte
		params, act    []byte
		uid, atqa, sak string
	}{
		{0, 2, []byte{0x44, 0, 7, 4, 1, 2, 3, 4, 5, 6, 1, 0}, nil, "04010203040506", "4400", "00"},
		{0, 4, []byte{4, 0, 4, 1, 2, 3, 4, 1, 0x20}, []byte{3, 0x75, 0x77, 0x81}, "01020304", "0400", "20"},
		{1, 4, []byte{11, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, nil, "01020304", "", ""},
		{2, 3, []byte{1, 16, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, nil, "0102030405060708", "", ""},
		{6, 6, []byte{0, 0, 1, 2, 3, 4, 5, 6, 7, 8}, nil, "0102030405060708", "", ""},
	}
	for _, c := range cases {
		b := activation(c.tech, c.protocol, c.params, c.act)
		tag, err := parseDiagnosticActivation(b)
		if err != nil {
			t.Fatal(err)
		}
		if tag.UID != c.uid || tag.ATQA != c.atqa || tag.SAK != c.sak || tag.DiscoveryID != 7 {
			t.Fatalf("wrong metadata: %+v", tag)
		}
		before := tag
		b[10] ^= 255
		if !reflect.DeepEqual(before, tag) {
			t.Fatal("metadata aliases input")
		}
	}
}

func TestDiagnosticActivationTruncation(t *testing.T) {
	b := activation(0, 2, []byte{0x44, 0, 7, 4, 1, 2, 3, 4, 5, 6, 1, 0}, nil)
	for n := 0; n < len(b); n++ {
		if _, err := parseDiagnosticActivation(b[:n]); err == nil {
			t.Fatalf("accepted truncation at %d", n)
		}
	}
	for _, params := range [][]byte{{}, {0, 0, 255}, {0, 0, 4, 1, 2}, {0, 0, 0, 20}} {
		if _, err := parseDiagnosticActivation(activation(0, 2, params, nil)); err == nil {
			t.Fatalf("accepted %x", params)
		}
	}
}

func TestDiagnosticExchangeRequiresActivatedTag(t *testing.T) {
	p := &PN7150{fd: -1}
	if _, err := p.ExchangeDiagnosticFrame([]byte{0x60}); err == nil {
		t.Fatal("frame without tag accepted")
	}
	if _, err := p.ExchangeDiagnosticAPDU([]byte{0, 0xb0, 0, 0, 2}); err == nil {
		t.Fatal("APDU without tag accepted")
	}
}

func TestDiagnosticExchangeCreditsAndMaximumPayload(t *testing.T) {
	for _, protocol := range []RFProtocol{RFProtocolT2T, RFProtocolISODEP} {
		t.Run(protocol.String(), func(t *testing.T) {
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fds[0])
			defer unix.Close(fds[1])
			p := &PN7150{fd: fds[0], rxBuf: make([]byte, nciBufferSize), numTags: 1, tagSelected: true, tags: []Tag{{RFProtocol: protocol}}}
			p.storeState(statePresent)
			payload := bytes.Repeat([]byte{0xab}, 255)
			done := make(chan error, 1)
			go func() {
				b := make([]byte, 32)
				if _, err := unix.Read(fds[1], b); err != nil {
					done <- err
					return
				}
				response := []byte{0x60, 6, 3, 1, 0, 1, 0, 0, 255}
				response = append(response, payload...)
				_, err := unix.Write(fds[1], response)
				done <- err
			}()
			var got []byte
			if protocol == RFProtocolT2T {
				got, err = p.ExchangeDiagnosticFrame([]byte{0x60})
			} else {
				got, err = p.ExchangeDiagnosticAPDU([]byte{0x90, 0x60, 0, 0, 0})
			}
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("payload length=%d error=%v", len(got), err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiagnosticDiscoveryUsesActualDiscoveryID(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	p := &PN7150{fd: fds[0], rxBuf: make([]byte, nciBufferSize), tags: make([]Tag, 1)}
	p.storeState(stateDiscovering)
	params := []byte{0x44, 0, 7, 4, 1, 2, 3, 4, 5, 6, 1, 0}
	discovery := append([]byte{0x61, 3, byte(5 + len(params)), 7, 2, 0, byte(len(params))}, params...)
	discovery = append(discovery, 0)
	if _, err := unix.Write(fds[1], discovery); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 32)
		n, err := unix.Read(fds[1], buf)
		if err != nil {
			done <- err
			return
		}
		if !bytes.Equal(buf[:n], []byte{0x21, 4, 3, 7, 2, 1}) {
			done <- fmt.Errorf("wrong selection: %x", buf[:n])
			return
		}
		response := append([]byte{0x41, 4, 1, 0}, activation(0, 2, params, nil)...)
		_, err = unix.Write(fds[1], response)
		done <- err
	}()
	tag, err := p.DetectDiagnosticTag()
	if err != nil || tag == nil || tag.DiscoveryID != 7 {
		t.Fatalf("tag=%+v err=%v", tag, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func FuzzDiagnosticActivation(f *testing.F) {
	f.Add(activation(0, 2, []byte{0x44, 0, 7, 4, 1, 2, 3, 4, 5, 6, 1, 0}, nil))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = parseDiagnosticActivation(b) })
}
