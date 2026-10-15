// Package paths locates the repository root and the station's data directories.
//
// The Python station derived everything from __file__; a Go binary lives wherever it was built
// (normally <root>/bin/aitv.exe), so the root is found explicitly: AITV_ROOT if set, else the
// parent of the executable's directory when that holds aitv/web/index.html, else the working
// directory. scripts/run_station.ps1 always runs the station from the root, so the fallback is safe.
package paths

import (
	"os"
	"path/filepath"
	"sync"
)

var (
	once sync.Once
	root string
)

// Root returns the repository root (see the package comment). It is resolved once.
func Root() string {
	once.Do(func() {
		if r := os.Getenv("AITV_ROOT"); r != "" {
			root = r
			return
		}
		if exe, err := os.Executable(); err == nil {
			cand := filepath.Dir(filepath.Dir(exe))
			if isRoot(cand) {
				root = cand
				return
			}
		}
		if wd, err := os.Getwd(); err == nil {
			root = wd
			return
		}
		root = "."
	})
	return root
}

func isRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "aitv", "web", "index.html"))
	return err == nil
}

// Lib is the clip library: <root>/aitv/library (every rendered mp4 plus index.json and scripts/).
func Lib() string { return filepath.Join(Root(), "aitv", "library") }

// Index is the library index: <root>/aitv/library/index.json.
func Index() string { return filepath.Join(Lib(), "index.json") }

// Scripts holds one JSON per written programme: <root>/aitv/library/scripts.
func Scripts() string { return filepath.Join(Lib(), "scripts") }

// Web holds the player page and hls.min.js: <root>/aitv/web.
func Web() string { return filepath.Join(Root(), "aitv", "web") }

// HLS is the live window written by the broadcaster: <root>/aitv/hls (AITV_HLS_DIR overrides it, for tests).
func HLS() string {
	if d := os.Getenv("AITV_HLS_DIR"); d != "" {
		return d
	}
	return filepath.Join(Root(), "aitv", "hls")
}

// Output is ComfyUI's output directory (scripts/run_comfyui.ps1 passes --output-directory <root>/output).
func Output() string { return filepath.Join(Root(), "output") }

// RawDir is where ComfyUI's SaveVideo drops the station's renders (filename_prefix "aitv/seg"): <root>/output/aitv.
func RawDir() string { return filepath.Join(Output(), "aitv") }
