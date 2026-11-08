package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	hal "github.com/librescoot/pn7150"
	"github.com/librescoot/pn7150/inspect"
	"golang.org/x/sys/unix"
)

type options struct {
	device, format, log          string
	exclusive, readMemory, debug bool
	maxBytes, count              int
	timeout                      time.Duration
}

func parseOptions(args []string, w io.Writer) (options, error) {
	var o options
	f := flag.NewFlagSet("nfc-inspect", flag.ContinueOnError)
	f.SetOutput(w)
	f.StringVar(&o.device, "device", "/dev/pn5xx_i2c2", "MDB-connected keycard reader (physically in the DBC casing)")
	f.StringVar(&o.format, "format", "text", "stdout format: text or json (one JSON object per activation)")
	f.StringVar(&o.log, "log", "", "create private JSONL report file; refuses to overwrite")
	f.BoolVar(&o.exclusive, "exclusive", false, "confirm keycard service is stopped and will remain stopped throughout scanning")
	f.BoolVar(&o.readMemory, "read-memory", false, "read public memory/NDEF; accept automatic read-counter/SDM side effects")
	f.BoolVar(&o.debug, "debug", false, "log raw NCI traffic on stderr (may contain private card data)")
	f.IntVar(&o.maxBytes, "max-bytes", 4096, "maximum memory bytes per activation (1..65536)")
	f.IntVar(&o.count, "count", 1, "number of activations to inspect; a card held in the field can be scanned repeatedly")
	f.DurationVar(&o.timeout, "timeout", time.Minute, "total scan deadline, including waiting for a card")
	if err := f.Parse(args); err != nil {
		return o, err
	}
	if f.NArg() != 0 {
		return o, fmt.Errorf("unexpected positional arguments")
	}
	if o.format != "text" && o.format != "json" {
		return o, fmt.Errorf("format must be text or json")
	}
	if o.maxBytes < 1 || o.maxBytes > 65536 || o.count < 1 || o.count > 1000 || o.timeout <= 0 {
		return o, fmt.Errorf("invalid byte/count/timeout limit")
	}
	if !o.exclusive {
		return o, fmt.Errorf("--exclusive required: stop librescoot-keycard on the MDB and keep it stopped; enrolled card taps with that service active can unlock the vehicle")
	}
	return o, nil
}

func keycardInactive(ctx context.Context) error {
	state, err := exec.CommandContext(ctx, "systemctl", "show", "--property=ActiveState", "--value", "librescoot-keycard.service").Output()
	if err != nil {
		return fmt.Errorf("cannot verify keycard service state: %w", err)
	}
	switch strings.TrimSpace(string(state)) {
	case "inactive", "failed":
		return nil
	default:
		return fmt.Errorf("keycard service is %q; stop it before using the reader", strings.TrimSpace(string(state)))
	}
}

// Check all other process FDs rather than relying on an advisory lock that
// keycard-service does not share. Refuse access if visibility is incomplete.
func readerUnused(device string) error {
	var target unix.Stat_t
	if err := unix.Stat(device, &target); err != nil {
		return err
	}
	if target.Mode&unix.S_IFMT != unix.S_IFCHR {
		return fmt.Errorf("%s is not a character device", device)
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, p := range processes {
		pid, err := strconv.Atoi(p.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		dir := filepath.Join("/proc", p.Name(), "fd")
		fds, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot inspect reader ownership for PID %d: %w", pid, err)
		}
		for _, fd := range fds {
			var stat unix.Stat_t
			if err := unix.Stat(filepath.Join(dir, fd.Name()), &stat); err != nil {
				if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
					continue
				}
				return fmt.Errorf("cannot inspect PID %d fd %s: %w", pid, fd.Name(), err)
			}
			if stat.Mode&unix.S_IFMT == unix.S_IFCHR && stat.Rdev == target.Rdev {
				return fmt.Errorf("reader already open by PID %d", pid)
			}
		}
	}
	return nil
}

func run(ctx context.Context, o options, stdout, stderr io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("run as root on the MDB to verify exclusive device ownership")
	}
	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	if err := keycardInactive(ctx); err != nil {
		return err
	}
	if err := readerUnused(o.device); err != nil {
		return err
	}
	lock, err := os.OpenFile("/run/nfc-inspect.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("another NFC inspection is running: %w", err)
	}
	var log *os.File
	if o.log != "" {
		log, err = os.OpenFile(o.log, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer log.Close()
	}
	reader, err := hal.NewPN7150(o.device, func(level hal.LogLevel, msg string) {
		if o.debug || level <= hal.LogLevelWarning {
			fmt.Fprintf(stderr, "%s: %s\n", level, msg)
		}
	}, nil, false, true, o.debug)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err := reader.Initialize(); err != nil {
		return err
	}
	for n := 0; n < o.count; n++ {
		if err := keycardInactive(ctx); err != nil {
			return err
		}
		if err := readerUnused(o.device); err != nil {
			return err
		}
		if err := reader.StartDiagnosticDiscovery(500); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Waiting for activation %d/%d on %s (present one card)...\n", n+1, o.count, o.device)
		var tag *hal.DiagnosticTag
		checked := time.Now()
		for tag == nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			if time.Since(checked) >= time.Second {
				if err := keycardInactive(ctx); err != nil {
					return err
				}
				if err := readerUnused(o.device); err != nil {
					return err
				}
				checked = time.Now()
			}
			tag, err = reader.DetectDiagnosticTag()
			if err != nil {
				return err
			}
		}
		if err := keycardInactive(ctx); err != nil {
			return err
		}
		if err := readerUnused(o.device); err != nil {
			return err
		}
		r := inspect.Scan(ctx, diagnosticTransport{reader: reader, ctx: ctx, device: o.device}, *tag, inspect.Options{ReadMemory: o.readMemory, MaxBytes: o.maxBytes})
		if o.format == "json" {
			err = json.NewEncoder(stdout).Encode(r)
		} else {
			err = r.WriteText(stdout)
		}
		if err != nil {
			return err
		}
		if log != nil {
			if err := json.NewEncoder(log).Encode(r); err != nil {
				return err
			}
			if err := log.Sync(); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, probe := range r.Probes {
			switch probe.Status {
			case "transport-error", "timeout", "departed", "canceled":
				return fmt.Errorf("scan interrupted at %s: %s", probe.Name, probe.Error)
			}
		}
		if n+1 < o.count {
			// RF field reset permits a fresh activation, not proof of removal.
			if err := reader.StopDiscovery(); err != nil {
				return err
			}
			if err := reader.AwaitReadable(time.Second); err != nil {
				return err
			}
			if _, err := reader.DetectDiagnosticTag(); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return nil
}

type diagnosticTransport struct {
	reader *hal.PN7150
	ctx    context.Context
	device string
}

func (t diagnosticTransport) check() error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	if err := keycardInactive(t.ctx); err != nil {
		return err
	}
	return readerUnused(t.device)
}
func (t diagnosticTransport) ExchangeAPDU(b []byte) ([]byte, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	return t.reader.ExchangeDiagnosticAPDU(b)
}
func (t diagnosticTransport) ExchangeDiagnosticFrame(b []byte) ([]byte, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	return t.reader.ExchangeDiagnosticFrame(b)
}

func main() {
	o, err := parseOptions(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err == nil {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		err = run(ctx, o, os.Stdout, os.Stderr)
		cancel()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "nfc-inspect:", err)
		os.Exit(1)
	}
}
