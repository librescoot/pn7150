// Package inspect provides bounded, unauthenticated NFC diagnostics. It sends
// only identification, selection and read commands, never writes or key guesses.
package inspect

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	hal "github.com/librescoot/pn7150"
)

type Transport interface {
	ExchangeAPDU([]byte) ([]byte, error)
	ExchangeDiagnosticFrame([]byte) ([]byte, error)
}

type Options struct {
	ReadMemory bool
	MaxBytes   int
}

type Probe struct {
	Name       string `json:"name"`
	Command    string `json:"command"`
	Response   string `json:"response,omitempty"`
	Status     string `json:"status"`
	StatusCode string `json:"status_code,omitempty"`
	Error      string `json:"error,omitempty"`
}

type Data struct {
	Name   string `json:"name"`
	Offset int    `json:"offset"`
	Hex    string `json:"hex"`
}

type Report struct {
	Time     time.Time         `json:"time"`
	Tag      hal.DiagnosticTag `json:"tag"`
	Type     string            `json:"type"`
	Evidence []string          `json:"evidence"`
	Warnings []string          `json:"warnings"`
	Data     []Data            `json:"data"`
	Probes   []Probe           `json:"probes"`
}

type scanner struct {
	ctx       context.Context
	transport Transport
	options   Options
	report    Report
	remaining int
	halted    bool
}

func Scan(ctx context.Context, t Transport, tag hal.DiagnosticTag, o Options) Report {
	if o.MaxBytes <= 0 || o.MaxBytes > 65536 {
		o.MaxBytes = 4096
	}
	s := scanner{ctx: ctx, transport: t, options: o, remaining: o.MaxBytes,
		report: Report{Time: time.Now().UTC(), Tag: tag, Type: "Unidentified", Evidence: []string{}, Data: []Data{}, Probes: []Probe{},
			Warnings: []string{"Identification is card-reported, not proof of authenticity. Secret keys and authenticated memory are not enumerated.", "Discovery identifiers may be random or change between activations."}}}
	if o.ReadMemory {
		s.report.Warnings = append(s.report.Warnings, "Reads may increment automatic NFC/SDM counters or expose mirrored rather than stored data; no write commands are sent.")
	} else {
		s.report.Warnings = append(s.report.Warnings, "Memory/NDEF reads disabled; enable --read-memory to accept implicit read-counter side effects.")
	}
	switch tag.Protocol {
	case 2:
		s.report.Type = "NFC Type 2-compatible"
		s.type2()
	case 4:
		s.report.Type = "ISO-DEP"
		s.isoDEP()
	default:
		s.report.Type = fmt.Sprintf("NCI protocol 0x%02x", tag.Protocol)
		s.report.Probes = append(s.report.Probes, Probe{Name: "card data", Status: "unsupported", Error: "activation metadata only for this protocol; no speculative commands or authentication"})
	}
	return s.report
}

func transportStatus(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "canceled"
	}
	var nfc hal.NFCError
	if errors.As(err, &nfc) && nfc.Code() == hal.ErrCodeI2CTimeout {
		return "timeout"
	}
	if hal.IsTagDepartedError(err) {
		return "departed"
	}
	return "transport-error"
}

func apduStatus(sw uint16) string {
	switch sw {
	case 0x9000, 0x9100:
		return "ok"
	case 0x91af:
		return "more"
	case 0x6982, 0x91ae:
		return "authentication-required"
	case 0x6985, 0x919d:
		return "denied"
	case 0x6a82, 0x91a0, 0x91f0:
		return "not-found"
	case 0x6d00, 0x6e00, 0x6a81, 0x911c:
		return "unsupported"
	default:
		return "card-error"
	}
}

func (s *scanner) apdu(name string, command []byte) ([]byte, string) {
	if s.halted {
		return nil, "skipped"
	}
	p := Probe{Name: name, Command: hex.EncodeToString(command)}
	if err := s.ctx.Err(); err != nil {
		p.Status = "canceled"
		p.Error = err.Error()
		s.report.Probes = append(s.report.Probes, p)
		return nil, p.Status
	}
	resp, err := s.transport.ExchangeAPDU(command)
	p.Response = hex.EncodeToString(resp)
	if err != nil {
		p.Status = transportStatus(err)
		p.Error = err.Error()
		s.halted = true
	} else if len(resp) < 2 {
		p.Status = "malformed"
		p.Error = "missing APDU status word"
	} else {
		sw := binary.BigEndian.Uint16(resp[len(resp)-2:])
		p.StatusCode = fmt.Sprintf("%04x", sw)
		p.Status = apduStatus(sw)
	}
	s.report.Probes = append(s.report.Probes, p)
	if p.Status == "ok" || p.Status == "more" {
		return resp[:len(resp)-2], p.Status
	}
	return nil, p.Status
}

func (s *scanner) frame(name string, command []byte, expected int) ([]byte, bool) {
	if s.halted {
		return nil, false
	}
	p := Probe{Name: name, Command: hex.EncodeToString(command)}
	if err := s.ctx.Err(); err != nil {
		p.Status = "canceled"
		p.Error = err.Error()
		s.report.Probes = append(s.report.Probes, p)
		return nil, false
	}
	resp, err := s.transport.ExchangeDiagnosticFrame(command)
	p.Response = hex.EncodeToString(resp)
	if err != nil {
		p.Status = transportStatus(err)
		p.Error = err.Error()
		s.halted = true
	} else if len(resp) == 0 {
		p.Status = "malformed"
		p.Error = "missing NCI status"
	} else {
		status := resp[len(resp)-1]
		p.StatusCode = fmt.Sprintf("%02x", status)
		switch {
		case status != 0:
			p.Status = "controller-error"
		case len(resp) == 2:
			p.Status = "refused"
			p.Error = fmt.Sprintf("tag NAK/result %02x; access denial and unsupported commands cannot be distinguished", resp[0])
		case len(resp) != expected+1:
			p.Status = "malformed"
			p.Error = fmt.Sprintf("expected %d card bytes, got %d", expected, len(resp)-1)
		default:
			p.Status = "ok"
		}
	}
	s.report.Probes = append(s.report.Probes, p)
	if p.Status == "ok" {
		return resp[:len(resp)-1], true
	}
	return nil, false
}

func (s *scanner) add(name string, offset int, b []byte) {
	s.report.Data = append(s.report.Data, Data{Name: name, Offset: offset, Hex: hex.EncodeToString(b)})
}

func (s *scanner) type2() {
	// GET_VERSION is defined by NTAG21x and some Ultralight products. Failed
	// probes can leave a tag idle, so do not send further commands on failure.
	v, ok := s.frame("GET_VERSION", []byte{0x60}, 8)
	if !ok {
		s.report.Warnings = append(s.report.Warnings, "GET_VERSION unavailable: memory layout unknown; further Type 2 probes skipped to avoid stale activation or out-of-range reads.")
		return
	}
	s.add("version", 0, v)
	lastPage := 0
	signatureLength := 32
	if v[0] == 0 && v[1] == 4 && v[2] == 4 && v[3] == 2 && v[4] == 1 && v[5] == 0 && v[7] == 3 {
		switch v[6] {
		case 0x0f:
			s.report.Type = "NTAG213-compatible"
			lastPage = 44
		case 0x11:
			s.report.Type = "NTAG215-compatible"
			lastPage = 134
		case 0x13:
			s.report.Type = "NTAG216-compatible"
			lastPage = 230
		}
	}
	if v[0] == 0 && v[1] == 4 && v[2] == 3 && (v[3] == 1 || v[3] == 2) && v[4] == 4 && v[5] == 0 && v[6] == 15 && v[7] == 3 {
		s.report.Type = "MIFARE Ultralight AES-compatible"
		lastPage = 47
		signatureLength = 48
		s.report.Warnings = append(s.report.Warnings, "Ultralight AES key pages are excluded; authenticated/secure-messaging reads are not attempted.")
	}
	s.report.Evidence = append(s.report.Evidence, "GET_VERSION="+hex.EncodeToString(v))
	if lastPage == 0 {
		s.report.Warnings = append(s.report.Warnings, "Unrecognized Type 2 version; only identification returned. No guessed memory extent or signature command.")
		return
	}
	if s.options.ReadMemory {
		var memory []byte
		for page := 0; page <= lastPage && s.remaining > 0; page += 4 {
			b, ok := s.frame(fmt.Sprintf("READ pages %d-%d", page, page+3), []byte{0x30, byte(page)}, 16)
			if !ok {
				break
			}
			// READ can wrap at the last page. Keep only bytes inside the known map.
			n := min(len(b), (lastPage-page+1)*4, s.remaining)
			s.add("memory", page*4, b[:n])
			memory = append(memory, b[:n]...)
			s.remaining -= n
		}
		if len(memory) >= 16 {
			cc := memory[12:16]
			s.add("capability-container", 12, cc)
			s.report.Evidence = append(s.report.Evidence, fmt.Sprintf("Type 2 CC: magic=%02x version=%02x NDEF capacity=%d access=%02x; static lock bytes=%x", cc[0], cc[1], int(cc[2])*8, cc[3], memory[10:12]))
			if cc[0] == 0xe1 {
				s.type2NDEF(memory[16:min(len(memory), 16+int(cc[2])*8)])
			}
		}
		if len(memory) < (lastPage+1)*4 {
			s.report.Warnings = append(s.report.Warnings, "Memory dump is partial (read refusal, departure, cancellation or byte limit); signature probe skipped.")
			return
		}
	}
	if sig, ok := s.frame("READ_SIG", []byte{0x3c, 0}, signatureLength); ok {
		s.add("originality-signature", 0, sig)
		s.report.Warnings = append(s.report.Warnings, "Originality signature captured but not cryptographically verified; a static signature is not anti-cloning authentication.")
	}
}

func nativeCommand(cmd byte, args []byte) []byte {
	b := []byte{0x90, cmd, 0, 0}
	if len(args) > 0 {
		b = append(b, byte(len(args)))
		b = append(b, args...)
	}
	return append(b, 0)
}

func (s *scanner) native(name string, cmd byte, args []byte) ([]byte, bool) {
	var result []byte
	for i := 0; i < 16; i++ {
		b, status := s.apdu(name, nativeCommand(cmd, args))
		if status != "ok" && status != "more" {
			return nil, false
		}
		if len(result)+len(b) > 4096 {
			break
		}
		result = append(result, b...)
		if status == "ok" {
			return result, true
		}
		cmd = 0xaf
		args = nil
	}
	s.halted = true
	s.report.Probes = append(s.report.Probes, Probe{Name: name, Status: "limit", Error: "native chaining exceeded 16 frames or 4096 bytes"})
	return nil, false
}

var selectNDEF = []byte{0, 0xa4, 4, 0, 7, 0xd2, 0x76, 0, 0, 0x85, 1, 1, 0}

func (s *scanner) isoDEP() {
	_, selected := s.apdu("SELECT NDEF application", selectNDEF)
	v, versionOK := s.native("GetVersion", 0x60, nil)
	if versionOK {
		s.add("version", 0, v)
		s.report.Evidence = append(s.report.Evidence, "GetVersion="+hex.EncodeToString(v))
		if len(v) >= 14 && v[0] == 4 {
			if v[1] == 1 {
				s.report.Type = "NXP DESFire-family (reported)"
			}
			if v[1] == 4 && v[2]&0x7f == 2 && v[3] == 0x30 && v[5] == 0x11 && v[6] == 5 {
				s.report.Type = "NTAG424 DNA-compatible"
			}
			if v[1] == 8 && (v[2]&0x7f == 1 || v[2]&0x7f == 2) && v[3] == 0x30 && v[5] == 0x13 && v[6] == 5 {
				s.report.Type = "DESFire Light-compatible"
			}
			s.report.Evidence = append(s.report.Evidence, fmt.Sprintf("hardware: vendor=%02x type=%02x subtype=%02x version=%02x.%02x storage-code=%02x protocol=%02x; software=%x", v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7:14]))
		}
	}
	if s.options.ReadMemory && selected == "ok" {
		// Restore the selected application after native diagnostic commands.
		if _, status := s.apdu("SELECT NDEF application for reading", selectNDEF); status == "ok" {
			s.type4()
		}
	}
	if versionOK && len(v) >= 14 && v[0] == 4 && v[1] == 1 {
		s.desfire()
	}
	if s.report.Type == "DESFire Light-compatible" {
		if files, ok := s.native("GetFileIDs", 0x6f, nil); ok {
			s.add("file-ids", 0, files)
			s.nativeFiles("DESFire Light", files, 0xad)
		}
	}
	if s.report.Type == "NTAG424 DNA-compatible" {
		s.nativeFiles("NTAG424 DNA", []byte{1, 2, 3}, 0xad)
	}
}

func selectFile(id []byte) []byte { return []byte{0, 0xa4, 0, 0x0c, 2, id[0], id[1]} }

func (s *scanner) binary(name string, offset, n int) ([]byte, bool) {
	b, status := s.apdu(name, []byte{0, 0xb0, byte(offset >> 8), byte(offset), byte(n)})
	if status != "ok" {
		return nil, false
	}
	if len(b) != n {
		s.report.Probes = append(s.report.Probes, Probe{Name: name, Status: "malformed", Error: fmt.Sprintf("requested %d bytes, received %d", n, len(b))})
		return nil, false
	}
	return b, true
}

func (s *scanner) type4() {
	if _, status := s.apdu("SELECT CC file", selectFile([]byte{0xe1, 3})); status != "ok" {
		return
	}
	header, ok := s.binary("READ CC header", 0, 7)
	if !ok {
		return
	}
	n := int(binary.BigEndian.Uint16(header[:2]))
	if n < 7 || n > 1024 {
		s.report.Warnings = append(s.report.Warnings, "Invalid/unsupported CC length; no file reads attempted.")
		return
	}
	cc := append([]byte(nil), header...)
	maxRead := min(int(binary.BigEndian.Uint16(header[3:5])), 240)
	if maxRead < 2 {
		s.report.Warnings = append(s.report.Warnings, "Invalid CC MLe")
		return
	}
	for len(cc) < n {
		b, ok := s.binary("READ CC", len(cc), min(maxRead, n-len(cc)))
		if !ok {
			return
		}
		cc = append(cc, b...)
	}
	s.add("capability-container", 0, cc)
	// Standard NDEF File Control TLVs only; extended files are reported raw.
	for pos := 7; pos < len(cc); {
		t := cc[pos]
		pos++
		if t == 0 {
			continue
		}
		if t == 0xfe {
			break
		}
		if pos >= len(cc) {
			s.report.Warnings = append(s.report.Warnings, "Truncated CC TLV")
			return
		}
		l := int(cc[pos])
		pos++
		if l > len(cc)-pos {
			s.report.Warnings = append(s.report.Warnings, "Truncated CC TLV")
			return
		}
		v := cc[pos : pos+l]
		pos += l
		if t != 4 || l != 6 {
			s.report.Probes = append(s.report.Probes, Probe{Name: "CC file control", Status: "unsupported", Error: fmt.Sprintf("TLV type %02x length %d retained raw; only standard NDEF files are decoded", t, l)})
			continue
		}
		name := fmt.Sprintf("NDEF file %x", v[:2])
		capacity := int(binary.BigEndian.Uint16(v[2:4]))
		s.report.Evidence = append(s.report.Evidence, fmt.Sprintf("%s capacity=%d read-access=%02x write-access=%02x", name, capacity, v[4], v[5]))
		if v[4] != 0 {
			status := "authentication-required"
			if v[4] == 0xff {
				status = "denied"
			}
			s.report.Probes = append(s.report.Probes, Probe{Name: name, Status: status, Error: "CC does not advertise free read access"})
			continue
		}
		if _, status := s.apdu("SELECT "+name, selectFile(v[:2])); status != "ok" {
			continue
		}
		nlen, ok := s.binary("READ NLEN", 0, 2)
		if !ok {
			continue
		}
		length := int(binary.BigEndian.Uint16(nlen))
		if capacity < 2 || length > capacity-2 {
			s.report.Warnings = append(s.report.Warnings, "NLEN exceeds NDEF file capacity")
			continue
		}
		var data []byte
		for len(data) < length && s.remaining > 0 {
			n := min(maxRead, length-len(data), s.remaining)
			b, ok := s.binary("READ "+name, 2+len(data), n)
			if !ok {
				break
			}
			data = append(data, b...)
			s.remaining -= len(b)
		}
		s.add(name, 2, data)
		if len(data) == length {
			s.decodeNDEF(data)
		} else {
			s.report.Warnings = append(s.report.Warnings, name+" is partial")
		}
	}
}

func (s *scanner) desfire() {
	if _, ok := s.native("Select PICC", 0x5a, []byte{0, 0, 0}); !ok {
		return
	}
	apps, ok := s.native("GetApplicationIDs", 0x6a, nil)
	if !ok {
		return
	}
	if len(apps)%3 != 0 {
		s.report.Warnings = append(s.report.Warnings, "Malformed application ID list")
		return
	}
	s.add("application-ids", 0, apps)
	if len(apps) > 32*3 {
		s.report.Warnings = append(s.report.Warnings, "Application enumeration exceeds 32 applications; truncating probes")
		apps = apps[:32*3]
	}
	for pos := 0; pos < len(apps) && s.ctx.Err() == nil; pos += 3 {
		aid := apps[pos : pos+3]
		name := fmt.Sprintf("application %x", aid)
		if _, ok := s.native("Select "+name, 0x5a, aid); !ok {
			continue
		}
		files, ok := s.native("GetFileIDs "+name, 0x6f, nil)
		if !ok {
			continue
		}
		s.add(name+" file-ids", 0, files)
		s.nativeFiles(name, files, 0xbd)
	}
	if s.remaining == 0 {
		s.report.Warnings = append(s.report.Warnings, "Memory byte limit reached")
	}
}

func (s *scanner) nativeFiles(application string, files []byte, readCommand byte) {
	if len(files) > 32 {
		s.report.Warnings = append(s.report.Warnings, "File enumeration exceeds 32 files; truncating")
		files = files[:32]
	}
	for _, file := range files {
		name := fmt.Sprintf("%s file %02x", application, file)
		settings, ok := s.native("GetFileSettings "+name, 0xf5, []byte{file})
		if !ok {
			continue
		}
		s.add(name+" settings", 0, settings)
		if !s.options.ReadMemory {
			continue
		}
		if len(settings) < 7 || settings[0] > 1 || settings[1] != 0 {
			s.report.Probes = append(s.report.Probes, Probe{Name: "Read " + name, Status: "unsupported", Error: "only plain standard/backup data files are read without authentication"})
			continue
		}
		size := int(settings[4]) | int(settings[5])<<8 | int(settings[6])<<16
		for offset := 0; offset < size && s.remaining > 0; {
			n := min(128, size-offset, s.remaining)
			args := []byte{file, byte(offset), byte(offset >> 8), byte(offset >> 16), byte(n), 0, 0}
			b, ok := s.native("ReadData "+name, readCommand, args)
			if !ok {
				break
			}
			if len(b) != n {
				s.report.Warnings = append(s.report.Warnings, "Unexpected ReadData length")
				break
			}
			s.add(name, offset, b)
			offset += n
			s.remaining -= n
		}
	}
}

func (r Report) WriteText(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "\n%s  UID=%s  %s\nTechnology=%02x protocol=%02x interface=%02x ATQA=%s SAK=%s ATS-response=%s\nTechnology parameters=%s\nActivation parameters=%s\nRaw activation=%s\n", r.Time.Format(time.RFC3339), r.Tag.UID, r.Type, r.Tag.Technology, r.Tag.Protocol, r.Tag.Interface, r.Tag.ATQA, r.Tag.SAK, r.Tag.ATSResponse, r.Tag.TechnologyParameters, r.Tag.ActivationParameters, r.Tag.RawActivation); err != nil {
		return err
	}
	for _, e := range r.Evidence {
		if _, err := fmt.Fprintln(w, "  Evidence:", e); err != nil {
			return err
		}
	}
	for _, e := range r.Warnings {
		if _, err := fmt.Fprintln(w, "  Warning:", e); err != nil {
			return err
		}
	}
	for _, d := range r.Data {
		if _, err := fmt.Fprintf(w, "  %s @%04x: %s\n", d.Name, d.Offset, d.Hex); err != nil {
			return err
		}
	}
	for _, p := range r.Probes {
		if _, err := fmt.Fprintf(w, "  [%s] %s TX=%s RX=%s status=%s %s\n", p.Status, p.Name, p.Command, p.Response, p.StatusCode, p.Error); err != nil {
			return err
		}
	}
	return nil
}
