package enrich

import (
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
)

// probeError assigns a bounded, operationally useful stage to a fingerprint
// failure. The wrapped error stays available for logs, while Stage is safe as a
// Prometheus label (unlike peer-controlled error text).
type probeError struct {
	Stage string
	Err   error
}

func (e *probeError) Error() string { return fmt.Sprintf("%s: %v", e.Stage, e.Err) }
func (e *probeError) Unwrap() error { return e.Err }

func atProbeStage(stage string, err error) error {
	if err == nil {
		err = errors.New("probe failed")
	}
	return &probeError{Stage: stage, Err: err}
}

// ProbeFailureKind returns a bounded failure category suitable for metrics.
func ProbeFailureKind(err error) string {
	var pe *probeError
	if errors.As(err, &pe) {
		return pe.Stage
	}
	return "unknown"
}

// ProbeErrorClass returns a bounded transport-level class, separating silent rejections (EOF/reset, e.g. Nethermind's recent-IP filter) from timeouts.
func ProbeErrorClass(err error) string {
	var ne net.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof"
	case errors.Is(err, syscall.ECONNRESET):
		return "reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	default:
		return "other"
	}
}
