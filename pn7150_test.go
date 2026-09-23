package hal

import (
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"
)

// Payloads captured from a live PN7150 on an i.MX6 MDB talking to a battery
// pack's NTAG, logged as "NCI RX" with the 3-byte DATA header stripped.
const (
	// STATUS0 of a pack at 51104 mV: 16 data bytes plus the trailing status.
	rxStatus0 = "a0c7d8ff021c2c35e88000001e1e640000"
	// The same block on a pack whose voltage low byte is 0x03. Synthetic, but
	// only in that one byte: this is the frame the old resp[3] test threw away.
	rxStatus0VoltageEndsIn03 = "03c7d8ff021c2c35e88000001e1e640000"
	// The NTAG refusing a read because the I2C side holds the arbiter.
	rxArbiterBusy = "0300"
	// A T2T WRITE acknowledged.
	rxWriteAck = "0a00"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad test fixture %q: %v", s, err)
	}
	return b
}

func TestParseT2TReadPayloadReturnsData(t *testing.T) {
	got, err := parseT2TReadPayload(mustHex(t, rxStatus0))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != t2tReadDataLen {
		t.Fatalf("got %d bytes, want %d", len(got), t2tReadDataLen)
	}
	// The trailing status byte must not be handed to the caller.
	if voltage := uint(got[0]) | uint(got[1])<<8; voltage != 51104 {
		t.Errorf("voltage decoded as %d, want 51104", voltage)
	}
}

// A data byte that happens to equal the arbiter NAK code must not be mistaken
// for a NAK. This is the regression: 1 in 256 pack voltages ends in 0x03, and
// those status reads were all discarded as "NTAG arbiter busy".
func TestParseT2TReadPayloadAcceptsDataStartingWithNakCode(t *testing.T) {
	got, err := parseT2TReadPayload(mustHex(t, rxStatus0VoltageEndsIn03))
	if err != nil {
		t.Fatalf("full-length frame rejected: %v", err)
	}
	if voltage := uint(got[0]) | uint(got[1])<<8; voltage != 50947 {
		t.Errorf("voltage decoded as %d, want 50947", voltage)
	}
}

func TestParseT2TReadPayloadDetectsArbiterBusy(t *testing.T) {
	_, err := parseT2TReadPayload(mustHex(t, rxArbiterBusy))
	if err == nil {
		t.Fatal("expected an error")
	}
	if _, ok := err.(*ArbiterBusyError); !ok {
		t.Fatalf("got %T (%v), want *ArbiterBusyError", err, err)
	}
}

func TestParseT2TReadPayloadRejectsShortAndUnexpected(t *testing.T) {
	for name, payload := range map[string]string{
		"write ack in a read":  rxWriteAck,
		"empty":                "",
		"truncated data block": "a0c7d8ff021c2c35",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseT2TReadPayload(mustHex(t, payload)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestParseT2TReadPayloadRejectsNonZeroStatus(t *testing.T) {
	payload := mustHex(t, rxStatus0)
	payload[t2tReadDataLen] = 0x03
	if _, err := parseT2TReadPayload(payload); err == nil {
		t.Fatal("expected an error for a non-OK status byte")
	}
}

func TestParseT2TReadPayloadCopiesOut(t *testing.T) {
	payload := mustHex(t, rxStatus0)
	got, err := parseT2TReadPayload(payload)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	payload[0] ^= 0xFF
	if got[0] == payload[0] {
		t.Error("result aliases the payload buffer")
	}
}

func TestParseRFDiscoverTag(t *testing.T) {
	frame := []byte{0x61, 0x03, 0x0c, 0x01, byte(RFProtocolT2T), 0x00, 0x00, 0x00, 0x00, 0x04, 0x11, 0x22, 0x33, 0x44, 0x00}
	tag, err := parseRFDiscoverTag(frame)
	if err != nil {
		t.Fatal(err)
	}
	if tag.RFProtocol != RFProtocolT2T || hex.EncodeToString(tag.ID) != "11223344" {
		t.Fatalf("tag = %+v", tag)
	}
	frame[10] = 0xff
	if tag.ID[0] != 0x11 {
		t.Fatal("UID aliases receive frame")
	}
}

func TestParseRFDiscoverTagRejectsInvalidUIDLength(t *testing.T) {
	frame := []byte{0x61, 0x03, 0x09, 0x01, byte(RFProtocolT2T), 0x00, 0x00, 0x00, 0x00, 0x04, 0x11, 0x22}
	zeroUID := append([]byte(nil), frame...)
	zeroUID[9] = 0
	badFrameLength := append([]byte(nil), frame...)
	badFrameLength[2] = 0x08
	oversizedUID := append(append([]byte(nil), frame[:9]...), 0x0b, 0x00)
	oversizedUID[2] = 0x08
	for name, input := range map[string][]byte{
		"missing UID length":  frame[:9],
		"truncated UID":       frame,
		"zero UID":            zeroUID,
		"oversized UID":       oversizedUID,
		"inconsistent length": badFrameLength,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRFDiscoverTag(input); err == nil {
				t.Fatal("expected malformed frame error")
			}
		})
	}
}

func TestAwaitReadablePollResults(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	p := &PN7150{fd: int(reader.Fd())}
	checkCode := func(want int) {
		t.Helper()
		err := p.AwaitReadable(0)
		var nfcErr NFCError
		if !errors.As(err, &nfcErr) || nfcErr.Code() != want {
			t.Fatalf("AwaitReadable error = %v, want code %d", err, want)
		}
	}
	checkCode(ErrCodeI2CTimeout)
	if _, err := writer.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := p.AwaitReadable(time.Millisecond); err != nil {
		t.Fatalf("readable pipe: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	checkCode(ErrCodeI2CPoll)
}

func TestClassifyRFDeactivateResponse(t *testing.T) {
	tests := []struct {
		name      string
		status    uint8
		wantWait  bool
		wantError bool
	}{
		{name: "success awaits notification", status: nciStatusOK, wantWait: true},
		{name: "already idle has no notification", status: nciStatusSemanticError},
		{name: "other error rejected", status: 0x01, wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotWait, err := classifyRFDeactivateResponse(&nciResponse{Status: tt.status})
			if (err != nil) != tt.wantError {
				t.Fatalf("error = %v, wantError %t", err, tt.wantError)
			}
			if gotWait != tt.wantWait {
				t.Errorf("wait = %t, want %t", gotWait, tt.wantWait)
			}
		})
	}
}
