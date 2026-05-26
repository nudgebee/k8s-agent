package podexec

import (
	"archive/tar"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path/filepath"
)

// base64Std is a tiny alias so the call site `base64Std.EncodeToString`
// reads obviously without forcing a per-package import for the trivial
// usage. Same encoding kubectl uses for binary file blocks on the wire.
var base64Std = base64.StdEncoding

// extractTarSingleFile pulls the named file's bytes out of a tar
// stream. `kubectl cp` (and our copyFileFromPod) runs `tar cf - <path>`
// inside the source container; the resulting stream contains exactly
// the requested file (no extras). We tolerate the leading "/" being
// stripped by tar (the standard behaviour) and fall back to matching
// by basename if the absolute paths don't line up.
//
// Returns (bytes, nil) on success. Returns an error when the tar
// stream is empty, malformed, or doesn't contain the requested file.
func extractTarSingleFile(r io.Reader, wantPath string) ([]byte, error) {
	tr := tar.NewReader(r)
	wantAbs := wantPath
	wantBase := filepath.Base(wantPath)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar header: %w", err)
		}
		if !tarEntryMatches(hdr.Name, wantAbs, wantBase) {
			continue
		}
		buf, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read tar entry %q: %w", hdr.Name, err)
		}
		return buf, nil
	}
	return nil, fmt.Errorf("file %q not found in tar stream", wantPath)
}

// tarEntryMatches accepts the entry whose name equals the requested
// path (modulo a leading slash that tar usually strips) OR whose
// basename equals the requested basename. The basename fallback covers
// cases where the source path was already-relative and tar emits it
// unchanged, or where the profiler writes to /tmp/<file> but tar's
// header drops the leading "/" so the entry name is "tmp/<file>".
func tarEntryMatches(entryName, wantAbs, wantBase string) bool {
	if entryName == wantAbs {
		return true
	}
	// Strip leading "/" — tar's standard behaviour.
	if len(wantAbs) > 0 && wantAbs[0] == '/' && entryName == wantAbs[1:] {
		return true
	}
	if filepath.Base(entryName) == wantBase {
		return true
	}
	return false
}
