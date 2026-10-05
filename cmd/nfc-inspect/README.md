# NFC card inspection

`nfc-inspect` is a standalone Linux diagnostic tool for the PN7150. On a
Librescoot scooter, the keycard reader is **physically in the DBC casing but
electrically connected to the MDB**, at `/dev/pn5xx_i2c2`. Run the tool on the
MDB. `/dev/pn5xx_i2c0` and `/dev/pn5xx_i2c1` are separate battery readers, not the
keycard reader.

It does not connect to Redis, enroll credentials, unlock the vehicle, write
card memory, provision cards, change keys, authenticate, or guess passwords.
It does initialize/configure the reader, control its RF field, select cards and
applications, and power the reader off on exit. Production HAL discovery keeps
its NFC-A configuration; broader diagnostic discovery is opt-in through a
separate API.

## Build

From the repository root:

```sh
GOTOOLCHAIN=go1.25.7 go build -o nfc-inspect ./cmd/nfc-inspect
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 GOTOOLCHAIN=go1.25.7 \
  go build -trimpath -ldflags='-s -w' -o nfc-inspect-armv7 ./cmd/nfc-inspect
```

## Safe use

Use test samples. With keycard-service running, an enrolled card tap can unlock
the vehicle and a master-card tap can enter learning mode. Stop
`librescoot-keycard.service` **on the MDB** before presenting any card and keep
it stopped throughout the test. Remove all cards from the reader before
restarting the service. Arrange a stationary, safe test environment and prevent
suspend/service restart for the test duration using the target's normal
maintenance procedure.

The tool never stops or restarts services automatically. It requires root,
`--exclusive`, an inactive/failed keycard unit, visibility of other processes'
open file descriptors, and no other process holding the selected character
device. It takes an advisory lock against other `nfc-inspect` instances and
rechecks service/device ownership before each probe. These checks are not an
atomic lock shared with keycard-service: do not start another reader owner
while testing. If ownership cannot be verified, scanning is refused.

After arranging exclusive reader access on the MDB:

```sh
# Identification and public file metadata; no memory/NDEF reads.
./nfc-inspect --exclusive

# Public memory/NDEF reads, human output and a private JSONL log.
./nfc-inspect --exclusive --read-memory --log /data/card-inspection.jsonl

# Machine-readable output and bounded repeated observations.
./nfc-inspect --exclusive --read-memory --format json \
  --count 5 --max-bytes 4096 --timeout 5m

# Include controller-level NCI traffic on stderr.
./nfc-inspect --exclusive --read-memory --debug 2>nci-trace.log
```

`--count` counts **activations**, not unique cards or proven removal/reinsertion.
Between reports the RF field is reset and there is a two-second pause. A card
left in the field can therefore produce several observations. UIDs can be
compared between reports, but stability alone does not establish authenticity.
Only one card may be present. Multiple-target discovery is refused.

The default total deadline is one minute and the default count is one.
`--max-bytes` limits collected memory/file bytes per activation, not metadata or
raw probe logs. A Type 2 READ always transfers four pages even if fewer bytes
remain in the collection budget. Native response chains are bounded to 16
frames/4096 bytes; application/file enumeration is capped at 32 each. Every
transport exchange has an absolute deadline across credit notifications.

Logs contain identifiers and potentially sensitive card contents. JSONL files
are created with mode `0600`, synced after each report, and never overwritten.
Stdout/stderr redirections have the shell's permissions; protect them yourself.

## Report contents and coverage

Each activation includes raw technology/protocol/interface codes, UID/NFCID
where available, NFC-A ATQA/SAK, ISO-DEP ATS response bytes, packet limits,
credits, data technology/rates, raw technology and activation parameters, and
the complete activation notification. Hex values preserve controller byte
order; NFC-V UID bytes are not reversed.

Each probe records command, response, status word/controller status, and error.
Transport errors, timeout, departure, unsupported commands, missing files,
authentication requirements, card refusal and explicit access denial are
separate results. Type 2 NAKs do not uniquely identify the reason: the tool
reports `refused`, not an invented authentication or unsupported diagnosis.

| Family/protocol | Probes |
| --- | --- |
| NTAG213/215/216 | GET_VERSION identification, READ_SIG capture; with `--read-memory`, bounded memory/configuration dump, static lock bytes, CC and NDEF TLVs |
| MIFARE Ultralight AES | GET_VERSION identification and 48-byte READ_SIG capture; public pages up to `0x2f`, excluding AES key pages; protected/secure-messaging responses remain inaccessible |
| Other Type 2 | GET_VERSION and activation evidence; no guessed memory extent or signature format |
| ISO-DEP Type 4 | SELECT NDEF, native GetVersion when available; optional CC and standard NDEF-file reads with text/URI decoding |
| DESFire family | Native GetVersion, application IDs, file IDs/settings; optional unauthenticated plain standard/backup data-file reads |
| DESFire Light | Version identification and file IDs/settings where public; optional plain data-file reads |
| NTAG424 DNA | Version identification, known file settings and optional plain file reads/NDEF; protected files remain inaccessible |
| Type 1, Type 3/FeliCa, Classic, ISO15693 | A/B/F/V discovery and activation metadata only; data probes explicitly reported unsupported |

This is **not a promise to dump every card or every byte**. Card-reported version
information identifies compatible layouts, not genuine silicon. DESFire variant
version bytes are reported without inferring a precise EV generation from UID
or SAK. Originality signatures are captured but not cryptographically verified;
a static signature is not challenge-response authentication or clone resistance.
Java Card applets, authentication, secure messaging, encrypted files,
record/value files, legacy Type 2 cards without GET_VERSION, extended Type 4
files, chunked NDEF and NCI packet fragmentation are not decoded. Raw evidence
is retained; unknown/protected content is not treated as empty memory.

## Read side effects

**No write commands does not mean no persistent card changes.** NTAG21x and
Ultralight AES can increment configured counters on reads. NTAG424 DNA can
increment SDM counters and return dynamic/mirrored NDEF data. `--read-memory`
explicitly opts into those effects. Without it, memory/NDEF READ commands are
not sent. No explicit counter-increment command is implemented.

Unsupported optional commands can leave a card idle. A failed Type 2 version
probe ends that card's probes; a failed memory read produces a partial dump and
skips its signature probe. A transport failure ends further probes rather than
retrying an uncertain activation. After a canceled/failed scan, reader resources
are closed; the service remains stopped for the operator to restore safely.

## Validation and public references

Software tests use synthetic activation frames, scripted card responses and
socket-backed NCI transport. They do not establish RF interoperability, antenna
performance, secure authentication or relay resistance. Validate on physical
samples of each sourced family before accepting a batch.

- [PN7150 user manual UM10936](https://www.nxp.com/docs/en/user-guide/UM10936.pdf)
- [NTAG213/215/216 datasheet](https://www.nxp.com/docs/en/data-sheet/NTAG213_215_216.pdf)
- [MIFARE Ultralight AES datasheet](https://www.nxp.com/docs/en/data-sheet/MF0AES(H)20.pdf)
- [NTAG424 DNA datasheet](https://www.nxp.com/docs/en/data-sheet/NT4H2421Gx.pdf)
- [DESFire Light datasheet](https://www.nxp.com/docs/en/data-sheet/MF2DLHX0.pdf)
