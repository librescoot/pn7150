package inspect

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	hal "github.com/librescoot/pn7150"
)

type step struct {
	command, response string
	err               error
}
type fake struct {
	t     *testing.T
	steps []step
	calls int
}

func (f *fake) exchange(b []byte) ([]byte, error) {
	f.t.Helper()
	if f.calls >= len(f.steps) {
		f.t.Fatalf("unexpected command %x", b)
	}
	s := f.steps[f.calls]
	f.calls++
	if len(b) > 0 && b[0] == 0x90 {
		switch b[1] {
		case 0x60, 0xaf, 0x5a, 0x6a, 0x6f, 0xf5, 0xbd, 0xad:
		default:
			f.t.Fatalf("non-diagnostic native instruction %x", b)
		}
	} else if len(b) > 1 && b[0] == 0 {
		if b[1] != 0xa4 && b[1] != 0xb0 {
			f.t.Fatalf("non-diagnostic ISO instruction %x", b)
		}
	} else if len(b) > 0 && b[0] != 0x60 && b[0] != 0x30 && b[0] != 0x3c {
		f.t.Fatalf("non-diagnostic frame command %x", b)
	}
	if hex.EncodeToString(b) != s.command {
		f.t.Fatalf("command %d got %x, want %s", f.calls, b, s.command)
	}
	r, err := hex.DecodeString(s.response)
	if err != nil {
		f.t.Fatal(err)
	}
	return r, s.err
}
func (f *fake) ExchangeAPDU(b []byte) ([]byte, error)            { return f.exchange(b) }
func (f *fake) ExchangeDiagnosticFrame(b []byte) ([]byte, error) { return f.exchange(b) }
func (f *fake) done() {
	f.t.Helper()
	if f.calls != len(f.steps) {
		f.t.Fatalf("%d/%d commands sent", f.calls, len(f.steps))
	}
}

func TestNTAGIdentificationAndSignatureWithoutMemory(t *testing.T) {
	for _, c := range []struct {
		size byte
		name string
	}{{15, "NTAG213"}, {17, "NTAG215"}, {19, "NTAG216"}} {
		t.Run(c.name, func(t *testing.T) {
			f := &fake{t: t, steps: []step{{"60", fmt.Sprintf("000404020100%02x0300", c.size), nil}, {"3c00", strings.Repeat("aa", 32) + "00", nil}}}
			r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 2}, Options{})
			f.done()
			if !strings.HasPrefix(r.Type, c.name) {
				t.Fatalf("type %s", r.Type)
			}
			if len(r.Data) != 2 || r.Data[1].Name != "originality-signature" {
				t.Fatalf("data: %+v", r.Data)
			}
			var text bytes.Buffer
			if err := r.WriteText(&text); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(text.String(), "not cryptographically verified") {
				t.Fatal("missing signature warning")
			}
			b, err := json.Marshal(r)
			if err != nil || !bytes.Contains(b, []byte("3c00")) {
				t.Fatalf("JSON: %s %v", b, err)
			}
		})
	}
}

func TestUltralightAESSignatureLength(t *testing.T) {
	f := &fake{t: t, steps: []step{{"60", "0004030204000f0300", nil}, {"3c00", strings.Repeat("ab", 48) + "00", nil}}}
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 2}, Options{})
	f.done()
	if r.Type != "MIFARE Ultralight AES-compatible" || len(r.Data[1].Hex) != 96 {
		t.Fatal(r)
	}
}

func TestLightFileEnumerationDenied(t *testing.T) {
	f := &fake{t: t, steps: []step{{"00a4040007d276000085010100", "9000", nil}, {"9060000000", "04080130001305040801000213059100", nil}, {"906f000000", "91ae", nil}}}
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 4}, Options{})
	f.done()
	if r.Type != "DESFire Light-compatible" || r.Probes[2].Status != "authentication-required" {
		t.Fatal(r)
	}
}

func TestTransportFailureStopsAllProbes(t *testing.T) {
	f := &fake{t: t, steps: []step{{"00a4040007d276000085010100", "", hal.NewI2CTimeoutError("timeout")}}}
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 4}, Options{ReadMemory: true})
	f.done()
	if r.Probes[0].Status != "timeout" {
		t.Fatal(r)
	}
}

func TestUnknownType2DoesNotGuessMemoryMap(t *testing.T) {
	f := &fake{t: t, steps: []step{{"60", "0004030101000b0300", nil}}}
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 2}, Options{ReadMemory: true})
	f.done()
	if r.Type != "NFC Type 2-compatible" {
		t.Fatal(r.Type)
	}
}

func TestType2PartialReadAndNoStatusInData(t *testing.T) {
	f := &fake{t: t, steps: []step{{"60", "0004040201000f0300", nil}, {"3000", "030102030405060708090a0be110120000", nil}, {"3004", "0300", nil}}}
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 2}, Options{ReadMemory: true})
	f.done()
	if r.Probes[1].Status != "ok" || r.Probes[2].Status != "refused" {
		t.Fatalf("probes: %+v", r.Probes)
	}
	if r.Data[1].Hex != "030102030405060708090a0be1101200" {
		t.Fatalf("status leaked: %+v", r.Data)
	}
}

func TestCompleteNTAG215DumpExcludesWrappedPages(t *testing.T) {
	f := &fake{t: t, steps: []step{{"60", "000404020100110300", nil}}}
	for page := 0; page <= 134; page += 4 {
		b := make([]byte, 17)
		for i := 0; i < 16; i++ {
			b[i] = byte(page + i/4)
		}
		if page == 0 {
			copy(b[12:16], []byte{0xe1, 0x10, 0x3e, 0})
		}
		if page == 4 {
			b[0] = 0xfe
		}
		f.steps = append(f.steps, step{fmt.Sprintf("30%02x", page), hex.EncodeToString(b), nil})
	}
	f.steps = append(f.steps, step{"3c00", strings.Repeat("aa", 32) + "00", nil})
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 2}, Options{ReadMemory: true})
	f.done()
	total := 0
	for _, d := range r.Data {
		if d.Name == "memory" {
			total += len(d.Hex) / 2
		}
	}
	if total != 540 {
		t.Fatalf("collected %d, want 540", total)
	}
}

func TestType2ByteLimit(t *testing.T) {
	f := &fake{t: t, steps: []step{{"60", "0004040201000f0300", nil}, {"3000", strings.Repeat("00", 16) + "00", nil}}}
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 2}, Options{ReadMemory: true, MaxBytes: 3})
	f.done()
	if len(r.Data) != 2 || len(r.Data[1].Hex) != 6 {
		t.Fatalf("unbounded data %+v", r.Data)
	}
}

func TestFrameMalformedAndTimeout(t *testing.T) {
	for _, c := range []struct {
		s    step
		want string
	}{
		{step{"60", "0400", nil}, "refused"},
		{step{"60", "", hal.NewI2CTimeoutError("timeout")}, "timeout"},
		{step{"60", "0004040201000f03ff", nil}, "controller-error"},
		{step{"60", "000400", nil}, "malformed"},
	} {
		f := &fake{t: t, steps: []step{c.s}}
		r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 2}, Options{})
		f.done()
		if r.Probes[0].Status != c.want {
			t.Fatalf("%s want %s", r.Probes[0].Status, c.want)
		}
	}
}

func TestType4NDEF(t *testing.T) {
	f := &fake{t: t, steps: []step{
		{"00a4040007d276000085010100", "9000", nil},
		{"9060000000", "6d00", nil},
		{"00a4040007d276000085010100", "9000", nil},
		{"00a4000c02e103", "9000", nil},
		{"00b0000007", "000f20003b00349000", nil},
		{"00b0000708", "0406e104010000ff9000", nil},
		{"00a4000c02e104", "9000", nil},
		{"00b0000002", "00099000", nil},
		{"00b0000209", "d101055402656e68699000", nil},
	}}
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 4}, Options{ReadMemory: true})
	f.done()
	if !strings.Contains(strings.Join(r.Evidence, "\n"), `text="hi"`) {
		t.Fatalf("not decoded: %+v", r)
	}
}

func TestType4DoesNotReadWithoutOptIn(t *testing.T) {
	f := &fake{t: t, steps: []step{{"00a4040007d276000085010100", "9000", nil}, {"9060000000", "6d00", nil}}}
	_ = Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 4}, Options{})
	f.done()
}

func TestNativeVersionChaining(t *testing.T) {
	f := &fake{t: t, steps: []step{
		{"00a4040007d276000085010100", "9000", nil},
		{"9060000000", "0404023000110591af", nil},
		{"90af000000", "0404020102110591af", nil},
		{"90af000000", "0102030405060708090a0b0c0d0e9100", nil},
		{"90f50000010100", "91ae", nil},
		{"90f50000010200", "919d", nil},
		{"90f50000010300", "91ae", nil},
	}}
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 4}, Options{})
	f.done()
	if r.Type != "NTAG424 DNA-compatible" || len(r.Data[0].Hex) != 56 {
		t.Fatalf("bad version %+v", r)
	}
}

func TestNativeChainingLimit(t *testing.T) {
	f := &fake{t: t, steps: []step{{"00a4040007d276000085010100", "6a82", nil}, {"9060000000", "0191af", nil}}}
	for i := 1; i < 16; i++ {
		f.steps = append(f.steps, step{"90af000000", "0191af", nil})
	}
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 4}, Options{})
	f.done()
	if r.Probes[len(r.Probes)-1].Status != "limit" {
		t.Fatal(r.Probes)
	}
}

func TestDESFireEnumerationAndPublicRead(t *testing.T) {
	f := &fake{t: t, steps: []step{
		{"00a4040007d276000085010100", "6a82", nil},
		{"9060000000", "04010133001805040101010318059100", nil},
		{"905a00000300000000", "9100", nil},
		{"906a000000", "0102039100", nil},
		{"905a00000301020300", "9100", nil},
		{"906f000000", "019100", nil},
		{"90f50000010100", "0000eeee0300009100", nil},
		{"90bd0000070100000003000000", "aabbcc9100", nil},
	}}
	r := Scan(context.Background(), f, hal.DiagnosticTag{Protocol: 4}, Options{ReadMemory: true})
	f.done()
	if r.Type != "NXP DESFire-family (reported)" || r.Data[len(r.Data)-1].Hex != "aabbcc" {
		t.Fatal(r)
	}
}

func TestAPDUStatuses(t *testing.T) {
	for sw, want := range map[uint16]string{0x9000: "ok", 0x9100: "ok", 0x91af: "more", 0x6982: "authentication-required", 0x91ae: "authentication-required", 0x919d: "denied", 0x6a82: "not-found", 0x6d00: "unsupported", 0xdead: "card-error"} {
		if got := apduStatus(sw); got != want {
			t.Fatalf("%04x %s want %s", sw, got, want)
		}
	}
}

func TestCanceledAndUnsupportedDoNotExchange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fake{t: t}
	for _, protocol := range []byte{2, 4, 0x80, 1, 3, 6} {
		_ = Scan(ctx, f, hal.DiagnosticTag{Protocol: protocol}, Options{ReadMemory: true})
	}
	f.done()
}

func TestNDEFBounds(t *testing.T) {
	valid := []byte{0xd1, 1, 5, 'T', 2, 'e', 'n', 'h', 'i'}
	rs, err := records(valid)
	if err != nil || len(rs) != 1 {
		t.Fatal(rs, err)
	}
	for n := 1; n < len(valid); n++ {
		if _, err := records(valid[:n]); err == nil {
			t.Fatalf("accepted truncation %d", n)
		}
	}
	for _, b := range [][]byte{{0xc1, 1, 255, 255, 255, 255}, {0xf1, 1, 0, 'T'}, {0x91, 1, 0, 'T'}, {0xd1, 1, 0, 'T', 0}} {
		if _, err := records(b); err == nil {
			t.Fatalf("accepted %x", b)
		}
	}
}

func FuzzNDEF(f *testing.F) {
	f.Add([]byte{0xd1, 1, 5, 'T', 2, 'e', 'n', 'h', 'i'})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = records(b) })
}
