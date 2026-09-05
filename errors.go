package pyodide

import (
	"errors"
	"fmt"
	"strings"
)

// ErrNotPureWheel is returned by AddWheel for wheels that contain compiled
// extension modules (any wheel whose platform tag is not "any").
var ErrNotPureWheel = errors.New("pyodide: wheel is not pure python")

// ErrClosed is returned when the Runtime has been closed.
var ErrClosed = errors.New("pyodide: runtime closed")

// ExitError is returned by Run and friends when the interpreter exits with a
// non-zero status, e.g. after an uncaught exception (status 1) or a call to
// sys.exit(n).
type ExitError struct {
	Code int
	// Stderr is what the interpreter wrote to stderr. Only Output fills it in;
	// the other methods stream stderr to RunOptions.Stderr.
	Stderr string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("pyodide: python exited with status %d", e.Code)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += "\n" + s
	}
	return msg
}
