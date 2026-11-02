# PN7150 NFC HAL

Go implementation of a Hardware Abstraction Layer (HAL) for the NXP PN7150 NFC controller.

Part of the [Librescoot](https://librescoot.org/) open-source platform.

## Overview

This library provides a complete NCI (NFC Controller Interface) implementation for the PN7150 NFC reader IC, supporting:

- **NFC-A (ISO14443A) tag detection and communication**
- **Type 2 Tag (T2T) protocol** - MIFARE Ultralight compatible
- **ISO-DEP (ISO14443-4) protocol** - MIFARE DESFire compatible
- **Low Power Card Detection (LPCD)** support
- **Standby mode** for power optimization
- **Comprehensive error handling** with typed error hierarchy

## Features

- Full NCI protocol implementation with I2C transport
- Robust error handling and recovery mechanisms
- Tag arrival/departure event notifications
- Binary read/write operations on NFC tags
- Hardware-level I2C communication with retry logic
- RF parameter configuration and verification
- Async tag event reader with file descriptor polling

## Requirements

- Go 1.24.1 or later
- Linux kernel with PN7150 I2C driver support
- PN7150 NFC controller hardware

## Installation

```bash
go get github.com/librescoot/pn7150
```

## Usage

```go
import "github.com/librescoot/pn7150"

// Create HAL instance
nfcHAL, err := hal.NewPN7150(
    "/dev/pn7150",      // Device path
    logCallback,         // Logging callback
    nil,                 // Application context
    true,                // Enable standby mode
    true,                // Enable LPCD
    false,               // Debug mode
)
if err != nil {
    log.Fatal(err)
}

// Initialize the controller
if err := nfcHAL.Initialize(); err != nil {
    log.Fatal(err)
}

// Start RF discovery
if err := nfcHAL.StartDiscovery(500); err != nil {
    log.Fatal(err)
}

// Enable tag event reader
nfcHAL.SetTagEventReaderEnabled(true)

// Listen for tag events
for event := range nfcHAL.GetTagEventChannel() {
    if event.Type == hal.TagArrival {
        fmt.Printf("Tag arrived: %X\n", event.Tag.ID)

        // Read from tag
        data, err := nfcHAL.ReadBinary(0x00)
        if err != nil {
            log.Printf("Read error: %v", err)
        }
    }
}
```

## Synchronous discovery observations

`PN7150.PollDiscovery()` returns a `DiscoveryResult` and an error. It is available
through the optional `DiscoveryPoller` interface; the existing `HAL` interface
and `DetectTags()` API remain available.

- `DiscoveryQuiet`: the receive wait ended without a notification. This is not
  a completed RF scan or proof that a tag was removed.
- `DiscoveryPending`: discovery is incomplete or another notification arrived.
- `DiscoveryTags`: a complete tag list is available. `Cached` distinguishes a
  remembered selection from a new discovery/activation notification.
- `DiscoveryDeactivated`: the RF link changed state. The result preserves the
  deactivation type and reason, including host-requested transitions.
- Errors: transport failures, malformed messages, controller reset and generic
  or interface error notifications. An activation follow-up error is not hidden
  behind a previously discovered tag.

Use synchronous discovery with exclusive controller ownership and the async tag
reader disabled. The caller owns the overall search deadline and absence policy.
The PN7150 does not report completion of each empty RF polling cycle, and RF
visibility does not establish an application's electrical connection state.
See [UM10936](https://www.nxp.com/docs/en/user-guide/UM10936.pdf), sections 6.3.3,
9.1 and 9.6.2: `TOTAL_DURATION` controls the listen/pause phase, and the proprietary
presence-check command applies to ISO-DEP/NFC-DEP, not Type-2 tags.

## Card inspection tool

[`nfc-inspect`](cmd/nfc-inspect/README.md) is a standalone diagnostic CLI for
the MDB-connected keycard reader. It captures activation metadata, card-reported
versions, raw exchanges and accessible public data, with human and JSONL reports.
Memory reads are opt-in because cards can increment counters even on reads.
Exclusive reader access is required; the tool never stops services or unlocks a
vehicle. See its README for build instructions, safety checks and coverage limits.

## Architecture

### Core Components

- **`pn7150.go`** - Main HAL implementation with state machine
- **`nci.go`** - NCI protocol command builders and parsers
- **`hal.go`** - HAL interface definition
- **`errors.go`** - Typed error hierarchy for precise error handling
- **`types.go`** - Tag types and event definitions

### Error Handling

The library uses a typed error hierarchy to distinguish between:

- **HAL Errors** - Hardware/communication failures requiring reinitialization
  - I2C Errors - Communication errors with retry handling
  - NCI Errors - Protocol-level errors
- **Application Errors** - Expected conditions (tag departed, multiple tags)
- **Transient Errors** - Temporary conditions that can be retried (arbiter busy)

## Hardware Configuration

The PN7150 is configured with:

- 27.12 MHz crystal clock
- Custom RF transition table for optimal performance
- PMU configuration for power management
- Tag detector configuration

## License

Original Librescoot code in this repository is available, at your option, under
[AGPL-3.0-only](LICENSE) or [CC-BY-NC-SA-4.0](LICENSE.CC-BY-NC-SA-4.0).
Third-party code retains its existing license.

The maintainers reserve the right to grant separate licenses for commercial distribution; please contact the maintainers to discuss commercial licensing.

[![CC BY-NC-SA 4.0][cc-by-nc-sa-image]][cc-by-nc-sa]

[cc-by-nc-sa]: http://creativecommons.org/licenses/by-nc-sa/4.0/
[cc-by-nc-sa-image]: https://licensebuttons.net/l/by-nc-sa/4.0/88x31.png

## Contributing

Contributions are welcome! Please ensure:

- Code follows Go best practices
- Error handling uses the typed error hierarchy
- Hardware-specific constants are documented
- Changes are tested with real PN7150 hardware

## References

- [NXP PN7150 Product Page](https://www.nxp.com/products/rfid-nfc/nfc-hf/nfc-readers/high-performance-nfc-controller-with-integrated-firmware-and-nci-interface:PN7150)
- [NCI Specification](https://nfc-forum.org/our-work/specification-releases/specifications/nfc-controller-interface-nci-technical-specification/)
- [ISO/IEC 14443 Standard](https://www.iso.org/standard/73599.html)
