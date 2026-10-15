// Package logx is the station's line logger: unbuffered UTF-8 lines on stdout, one call per line,
// safe from several goroutines. The launcher redirects stdout to output/station.log.
package logx

import (
	"fmt"
	"os"
	"sync"
)

// Logger is the injectable log function every component takes (the Python code passed log=print).
type Logger func(format string, args ...any)

var mu sync.Mutex

// Printf writes one line to stdout. A trailing newline is added when missing.
func Printf(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	if len(s) == 0 || s[len(s)-1] != '\n' {
		s += "\n"
	}
	mu.Lock()
	defer mu.Unlock()
	_, _ = os.Stdout.WriteString(s)
}

// Discard drops everything (the producer silences the per-node ComfyUI progress this way).
func Discard(string, ...any) {}
