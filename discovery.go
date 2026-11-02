package hal

import "fmt"

// DiscoveryKind describes an observation, not whether a physical tag was removed.
type DiscoveryKind uint8

const (
	// DiscoveryPending includes partial discovery and unrelated notifications.
	DiscoveryPending DiscoveryKind = iota
	// DiscoveryQuiet means no notification arrived during this receive wait.
	// It does not mean the controller completed an RF polling cycle.
	DiscoveryQuiet
	// DiscoveryTags means a complete tag list is available. Cached distinguishes
	// a remembered selection from a newly received discovery or activation.
	DiscoveryTags
	// DiscoveryDeactivated means the RF link changed state, including changes
	// requested by the host. It is not evidence of physical tag removal.
	DiscoveryDeactivated
)

// DiscoveryPoller is optional so existing HAL implementations remain compatible.
type DiscoveryPoller interface {
	PollDiscovery() (DiscoveryResult, error)
}

type DiscoveryResult struct {
	Kind               DiscoveryKind
	Tags               []Tag
	Cached             bool
	DeactivationType   byte
	DeactivationReason byte
}

// PollDiscovery receives one discovery observation. Use it without the async
// tag-event reader and serialize it with other operations on this controller.
// Silence, partial discovery and RF deactivation are distinct from transport,
// malformed-message and controller-reported errors. None proves electrical OFF.
func (p *PN7150) PollDiscovery() (DiscoveryResult, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if s := p.loadState(); s != stateDiscovering && s != statePresent {
		return DiscoveryResult{}, NewNCIInvalidDataError(fmt.Sprintf("cannot poll discovery in state: %s", s))
	}
	receive := func() ([]byte, error) { return p.transferLocked(nil, readTimeout) }
	resp, err := receive()
	if err != nil {
		return DiscoveryResult{}, err
	}
	return p.consumeDiscovery(resp, receive)
}

// Caller holds mutex. receive reads the auto-activation following a final
// RF_DISCOVER_NTF; a failed receive must not be hidden behind a cached tag.
func (p *PN7150) consumeDiscovery(resp []byte, receive func() ([]byte, error)) (DiscoveryResult, error) {
	known := func(fallback DiscoveryKind) DiscoveryResult {
		if p.loadState() == statePresent && p.numTags > 0 {
			return DiscoveryResult{Kind: DiscoveryTags, Tags: p.copyDiscoveryTags(), Cached: true}
		}
		if p.numTags > 0 {
			return DiscoveryResult{Kind: DiscoveryPending}
		}
		return DiscoveryResult{Kind: fallback}
	}
	if len(resp) == 0 {
		return known(DiscoveryQuiet), nil
	}
	if len(resp) < 3 || len(resp) != int(resp[2])+3 {
		return DiscoveryResult{}, NewNCIIncompleteMsgError("invalid discovery notification length")
	}
	mt, gid, oid := (resp[0]>>nciMsgTypeBit)&0x03, resp[0]&0x0f, resp[1]&0x3f
	if mt != nciMsgTypeNotification || resp[0]&0x10 != 0 || resp[1]&0xc0 != 0 {
		return DiscoveryResult{}, NewNCIInvalidDataError("expected notification during discovery")
	}
	if gid == nciGroupCore {
		switch oid {
		case nciCoreReset:
			return DiscoveryResult{}, NewNCIUnexpectedResetError("controller reset during discovery")
		case nciCoreGenericError, nciCoreInterfaceError:
			return DiscoveryResult{}, NewNCIInvalidDataError(fmt.Sprintf("controller error during discovery: %X", resp))
		}
	}
	if gid != nciGroupRF {
		return known(DiscoveryPending), nil
	}

	switch oid {
	case nciRFDiscoverOID:
		if len(resp) < 7 {
			return DiscoveryResult{}, NewNCIInvalidDataError("invalid RF_DISCOVER_NTF length")
		}
		if resp[5] != nciRFTechNFCAPassivePoll {
			return DiscoveryResult{Kind: DiscoveryPending}, nil
		}
		if p.loadState() == statePresent {
			p.numTags = 0
			p.tagSelected = false
			p.storeState(stateDiscovering)
		}
		tag, err := parseRFDiscoverTag(resp)
		if err != nil {
			return DiscoveryResult{}, err
		}
		notificationType := resp[len(resp)-1]
		if notificationType > 2 || p.numTags >= maxTags {
			return DiscoveryResult{}, NewNCIInvalidDataError("invalid discovery sequence")
		}
		p.tags[p.numTags] = tag
		p.numTags++
		if notificationType == 2 {
			return DiscoveryResult{Kind: DiscoveryPending}, nil
		}
		if p.numTags > 1 {
			p.numTags = 0
			p.storeState(stateDiscovering)
			p.tagSelected = false
			return DiscoveryResult{}, NewMultipleTagsError("multiple tags not supported")
		}
		p.storeState(statePresent)
		p.tagSelected = false
		next, err := receive()
		if err != nil {
			return DiscoveryResult{}, err
		}
		if len(next) > 0 {
			return p.consumeDiscovery(next, receive)
		}
		return DiscoveryResult{Kind: DiscoveryTags, Tags: p.copyDiscoveryTags()}, nil

	case nciRFIntfActivatedOID:
		if len(resp) < 14 {
			return DiscoveryResult{}, NewNCIInvalidDataError("short activation notification")
		}
		end := 10 + int(resp[9])
		uidLen := int(resp[12])
		if end+4 > len(resp) || end+4+int(resp[end+3]) != len(resp) || uidLen == 0 || uidLen > maxUIDSize || 13+uidLen >= end {
			return DiscoveryResult{}, NewNCIInvalidDataError("invalid activation parameter lengths")
		}
		tag, err := parseRFIntfActivatedNtf(resp)
		if err != nil {
			return DiscoveryResult{}, NewNCIInvalidDataError(fmt.Sprintf("invalid activation: %v", err))
		}
		p.storeState(statePresent)
		p.numTags = 1
		p.tags[0] = *tag
		p.tagSelected = true
		return DiscoveryResult{Kind: DiscoveryTags, Tags: p.copyDiscoveryTags()}, nil

	case nciRFDeactivateOID:
		if len(resp) != 5 || resp[3] > 3 || resp[4] > 3 {
			return DiscoveryResult{}, NewNCIInvalidDataError("invalid RF_DEACTIVATE_NTF")
		}
		p.numTags = 0
		p.tagSelected = false
		if resp[3] == 3 {
			p.storeState(stateDiscovering)
		} else {
			p.storeState(stateIdle)
		}
		return DiscoveryResult{Kind: DiscoveryDeactivated, DeactivationType: resp[3], DeactivationReason: resp[4]}, nil
	}
	return known(DiscoveryPending), nil
}

func (p *PN7150) copyDiscoveryTags() []Tag {
	tags := p.copyTags()
	for i := range tags {
		tags[i].ID = append([]byte(nil), tags[i].ID...)
	}
	return tags
}
