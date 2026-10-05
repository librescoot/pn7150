package inspect

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

func (s *scanner) type2NDEF(data []byte) {
	for pos := 0; pos < len(data); {
		t := data[pos]
		pos++
		if t == 0 {
			continue
		}
		if t == 0xfe {
			return
		}
		if pos >= len(data) {
			s.report.Warnings = append(s.report.Warnings, "Truncated Type 2 TLV length")
			return
		}
		n := int(data[pos])
		pos++
		if n == 255 {
			if pos+2 > len(data) {
				s.report.Warnings = append(s.report.Warnings, "Truncated extended TLV length")
				return
			}
			n = int(binary.BigEndian.Uint16(data[pos : pos+2]))
			pos += 2
		}
		if n > len(data)-pos {
			s.report.Warnings = append(s.report.Warnings, "Truncated Type 2 TLV data")
			return
		}
		if t == 3 {
			s.add("NDEF", pos+16, data[pos:pos+n])
			s.decodeNDEF(data[pos : pos+n])
		}
		pos += n
	}
}

type record struct {
	tnf              byte
	typ, id, payload []byte
}

func records(b []byte) ([]record, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var result []record
	ended := false
	for pos := 0; pos < len(b); {
		if ended {
			return nil, fmt.Errorf("data after final NDEF record")
		}
		if pos+2 > len(b) {
			return nil, fmt.Errorf("truncated NDEF header")
		}
		h := b[pos]
		typeLen := int(b[pos+1])
		pos += 2
		if (len(result) == 0) != (h&0x80 != 0) {
			return nil, fmt.Errorf("invalid NDEF message-begin flag")
		}
		if h&0x20 != 0 {
			return nil, fmt.Errorf("chunked NDEF records are not decoded")
		}
		var payloadLen uint32
		if h&0x10 != 0 {
			if pos >= len(b) {
				return nil, fmt.Errorf("truncated NDEF payload length")
			}
			payloadLen = uint32(b[pos])
			pos++
		} else {
			if pos+4 > len(b) {
				return nil, fmt.Errorf("truncated NDEF payload length")
			}
			payloadLen = binary.BigEndian.Uint32(b[pos : pos+4])
			pos += 4
		}
		idLen := 0
		if h&8 != 0 {
			if pos >= len(b) {
				return nil, fmt.Errorf("truncated NDEF ID length")
			}
			idLen = int(b[pos])
			pos++
		}
		if uint64(typeLen)+uint64(idLen)+uint64(payloadLen) > uint64(len(b)-pos) {
			return nil, fmt.Errorf("truncated NDEF body")
		}
		r := record{tnf: h & 7, typ: b[pos : pos+typeLen]}
		pos += typeLen
		r.id = b[pos : pos+idLen]
		pos += idLen
		r.payload = b[pos : pos+int(payloadLen)]
		pos += int(payloadLen)
		result = append(result, r)
		ended = h&0x40 != 0
	}
	if !ended {
		return nil, fmt.Errorf("missing NDEF message-end flag")
	}
	return result, nil
}

func (s *scanner) decodeNDEF(b []byte) {
	rs, err := records(b)
	if err != nil {
		s.report.Warnings = append(s.report.Warnings, "NDEF: "+err.Error())
		return
	}
	for i, r := range rs {
		text := fmt.Sprintf("NDEF record %d: TNF=%d type=%q id=%s payload=%s", i, r.tnf, r.typ, hex.EncodeToString(r.id), hex.EncodeToString(r.payload))
		if r.tnf == 1 && string(r.typ) == "T" && len(r.payload) > 0 {
			n := int(r.payload[0] & 63)
			if len(r.payload) >= n+1 && r.payload[0]&0x80 == 0 {
				text += fmt.Sprintf(" language=%q text=%q", r.payload[1:1+n], r.payload[1+n:])
			}
		}
		if r.tnf == 1 && string(r.typ) == "U" && len(r.payload) > 0 {
			prefixes := []string{"", "http://www.", "https://www.", "http://", "https://", "tel:", "mailto:"}
			p := int(r.payload[0])
			if p < len(prefixes) {
				text += fmt.Sprintf(" URI=%q", prefixes[p]+string(r.payload[1:]))
			}
		}
		s.report.Evidence = append(s.report.Evidence, text)
	}
}
