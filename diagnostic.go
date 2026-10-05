package hal

import (
	"fmt"
	"time"
)

// DiagnosticTag preserves the controller's activation evidence. Hex strings
// retain wire byte order; NFC-V UIDs are not reversed.
type DiagnosticTag struct {
	DiscoveryID          byte   `json:"discovery_id"`
	Interface            byte   `json:"interface"`
	Protocol             byte   `json:"protocol"`
	Technology           byte   `json:"technology"`
	MaxPayload           byte   `json:"max_payload"`
	InitialCredits       byte   `json:"initial_credits"`
	DataTechnology       byte   `json:"data_technology"`
	TxBitRate            byte   `json:"tx_bit_rate"`
	RxBitRate            byte   `json:"rx_bit_rate"`
	UID                  string `json:"uid,omitempty"`
	ATQA                 string `json:"atqa,omitempty"`
	SAK                  string `json:"sak,omitempty"`
	ATSResponse          string `json:"ats_response,omitempty"`
	TechnologyParameters string `json:"technology_parameters"`
	ActivationParameters string `json:"activation_parameters"`
	RawActivation        string `json:"raw_activation"`
}

func parseDiagnosticActivation(data []byte) (DiagnosticTag, error) {
	if len(data) < 14 || data[0] != 0x61 || data[1] != 0x05 || int(data[2])+3 != len(data) {
		return DiagnosticTag{}, fmt.Errorf("invalid RF_INTF_ACTIVATED_NTF")
	}
	n := int(data[9])
	end := 10 + n
	if end+4 > len(data) || end+4+int(data[end+3]) != len(data) {
		return DiagnosticTag{}, fmt.Errorf("invalid activation parameter lengths")
	}
	params := data[10:end]
	t := DiagnosticTag{DiscoveryID: data[3], Interface: data[4], Protocol: data[5], Technology: data[6],
		MaxPayload: data[7], InitialCredits: data[8], DataTechnology: data[end], TxBitRate: data[end+1], RxBitRate: data[end+2],
		TechnologyParameters: fmt.Sprintf("%x", params), ActivationParameters: fmt.Sprintf("%x", data[end+4:]), RawActivation: fmt.Sprintf("%x", data)}
	switch t.Technology {
	case 0: // NFC-A: SENS_RES, NFCID1 length/value, SEL_RES length/value.
		if len(params) < 4 {
			return t, fmt.Errorf("short NFC-A parameters")
		}
		uidLen := int(params[2])
		if uidLen > 10 || 3+uidLen >= len(params) {
			return t, fmt.Errorf("invalid NFCID1 length")
		}
		sel := 3 + uidLen
		if sel+1+int(params[sel]) != len(params) {
			return t, fmt.Errorf("invalid SEL_RES length")
		}
		t.UID = fmt.Sprintf("%x", params[3:sel])
		t.ATQA = fmt.Sprintf("%x", params[:2])
		t.SAK = fmt.Sprintf("%x", params[sel+1:])
		act := data[end+4:]
		if t.Protocol == 4 && t.Interface == 2 && len(act) > 0 && int(act[0])+1 == len(act) {
			t.ATSResponse = fmt.Sprintf("%x", act[1:])
		}
	case 1: // NFC-B: SENSB_RES length/value, beginning with NFCID0 (PUPI).
		if len(params) < 5 || int(params[0])+1 != len(params) {
			return t, fmt.Errorf("invalid NFC-B parameters")
		}
		t.UID = fmt.Sprintf("%x", params[1:5])
	case 2: // NFC-F: bitrate, SENSF_RES length, NFCID2, PMm.
		if len(params) < 18 || int(params[1])+2 != len(params) {
			return t, fmt.Errorf("invalid NFC-F parameters")
		}
		t.UID = fmt.Sprintf("%x", params[2:10])
	case 6: // PN7150 NFC-15693 extension: FLAGS, DSFID, UID.
		if len(params) != 10 {
			return t, fmt.Errorf("invalid NFC-V parameters")
		}
		t.UID = fmt.Sprintf("%x", params[2:])
	}
	return t, nil
}

// StartDiagnosticDiscovery enables A/B/F/V polling only for diagnostic users.
// Production StartDiscovery retains its NFC-A mapping and polling behavior.
func (p *PN7150) StartDiagnosticDiscovery(pollPeriod uint) error {
	if p.loadState() != stateIdle {
		return fmt.Errorf("diagnostic discovery requires idle controller")
	}
	// UM10936: T1T/T2T/T3T/15693 use FRAME; Classic uses TAG-CMD.
	mapping := []byte{0x21, 0, 19, 6, 1, 1, 1, 2, 1, 1, 3, 1, 1, 4, 1, 2, 6, 1, 1, 0x80, 1, 0x80}
	resp, err := p.transfer(mapping)
	if err != nil {
		return err
	}
	parsed, err := parseNCIResponse(resp)
	if err != nil {
		return err
	}
	if !isSuccessResponse(parsed) {
		return fmt.Errorf("diagnostic mapping rejected: %02x", parsed.Status)
	}
	return p.startDiscovery(pollPeriod, []byte{0x21, 3, 9, 4, 0, 1, 1, 1, 2, 1, 6, 1})
}

// DetectDiagnosticTag is synchronous: no tag-event reader may be running.
// Multiple-target discovery is rejected rather than selecting an arbitrary card.
func (p *PN7150) DetectDiagnosticTag() (*DiagnosticTag, error) {
	resp, err := p.transfer(nil)
	if err != nil || len(resp) == 0 {
		return nil, err
	}
	if len(resp) < 3 || resp[0] != 0x61 {
		return nil, nil
	}
	switch resp[1] {
	case 3:
		if len(resp) < 8 || int(resp[2])+3 != len(resp) {
			return nil, fmt.Errorf("invalid discovery notification")
		}
		if resp[len(resp)-1] > 1 {
			return nil, NewMultipleTagsError("present one card at a time")
		}
		// Consume auto-activation before attempting explicit host selection.
		next, err := p.transferWithTimeout(nil, readTimeout)
		if err != nil {
			return nil, err
		}
		if len(next) > 0 {
			return p.activateDiagnosticTag(next)
		}
		intf := byte(1)
		if resp[4] == 4 {
			intf = 2
		}
		if resp[4] == 0x80 {
			intf = 0x80
		}
		selected, err := p.transfer([]byte{0x21, 4, 3, resp[3], resp[4], intf})
		if err != nil {
			return nil, err
		}
		if len(selected) != 4 || selected[0] != 0x41 || selected[1] != 4 || selected[3] != 0 {
			return nil, fmt.Errorf("diagnostic selection rejected: %x", selected)
		}
		next, err = p.transferWithTimeout(nil, time.Second)
		if err != nil {
			return nil, err
		}
		return p.activateDiagnosticTag(next)
	case 5:
		return p.activateDiagnosticTag(resp)
	case 6:
		p.mutex.Lock()
		p.storeState(stateIdle)
		p.numTags = 0
		p.tagSelected = false
		p.mutex.Unlock()
	}
	return nil, nil
}

func (p *PN7150) activateDiagnosticTag(resp []byte) (*DiagnosticTag, error) {
	tag, err := parseDiagnosticActivation(resp)
	if err != nil {
		return nil, err
	}
	p.mutex.Lock()
	p.storeState(statePresent)
	p.numTags = 1
	p.tagSelected = true
	p.tags[0] = Tag{RFProtocol: RFProtocol(tag.Protocol)}
	p.mutex.Unlock()
	return &tag, nil
}

// ExchangeDiagnosticFrame exchanges a non-ISO-DEP FRAME command. The returned
// payload includes the trailing controller status (UM10936 section 6.1.6).
// Callers must serialize discovery and exchanges; this does not start a reader.
func (p *PN7150) ExchangeDiagnosticFrame(command []byte) ([]byte, error) {
	return p.exchangeDiagnostic(command, RFProtocolT2T)
}

// ExchangeDiagnosticAPDU uses a single absolute deadline across credit
// notifications. Unlike FRAME responses, ISO-DEP has no trailing NCI status.
func (p *PN7150) ExchangeDiagnosticAPDU(command []byte) ([]byte, error) {
	if len(command) < 4 {
		return nil, fmt.Errorf("invalid APDU length")
	}
	return p.exchangeDiagnostic(command, RFProtocolISODEP)
}

func (p *PN7150) exchangeDiagnostic(command []byte, protocol RFProtocol) ([]byte, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if p.loadState() != statePresent || !p.tagSelected || p.numTags != 1 || p.tags[0].RFProtocol != protocol {
		return nil, fmt.Errorf("no activated diagnostic target for %s", protocol)
	}
	if len(command) == 0 || len(command) > 254 {
		return nil, fmt.Errorf("invalid frame command length")
	}
	packet := append([]byte{0, 0, byte(len(command))}, command...)
	deadline := time.Now().Add(time.Second)
	resp, err := p.transferLocked(packet, time.Until(deadline))
	for err == nil && len(resp) >= 3 && resp[0] == 0x60 && resp[1] == nciCoreConnCredits && time.Now().Before(deadline) {
		resp, err = p.transferLocked(nil, time.Until(deadline))
	}
	if err != nil {
		return nil, err
	}
	if len(resp) == 0 || !time.Now().Before(deadline) {
		return nil, NewI2CTimeoutError("diagnostic exchange deadline exceeded")
	}
	if len(resp) < 4 || resp[0] != 0 || resp[1] != 0 || int(resp[2])+3 != len(resp) {
		return nil, NewNCIInvalidDataError("invalid diagnostic DATA response")
	}
	return append([]byte(nil), resp[3:]...), nil
}
