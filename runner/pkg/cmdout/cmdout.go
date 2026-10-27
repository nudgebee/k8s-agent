// Package cmdout collects a command's stdout for an action response.
//
// By default the output is kept as text and returned in "stdout", exactly as
// before. A caller that sends action_params.output_encoding="gzip" gets it
// gzip-compressed as it streams in, returned base64-encoded in
// "stdout_gzip_b64" with "output_encoding":"gzip" beside it. The raw output is
// then never held in memory, and the response the agent pushes over the relay
// is the compressed size: command output (kubectl JSON, logs, Prometheus API
// bodies) typically compresses 10-30x, and base64 needs no JSON escaping, so
// the nested JSON encoding of the Finding envelope stops multiplying it.
//
// An encoding the agent does not know falls back to plain text, the same thing
// an agent that predates this package does. Callers therefore read the
// response's "output_encoding" rather than assume what they asked for.
package cmdout

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"strings"
)

const (
	// Param is the action_params key a caller sets to ask for an encoding.
	Param = "output_encoding"
	// Gzip is the one encoding supported besides plain text.
	Gzip = "gzip"
)

// Output is an io.Writer that collects stdout either as text or gzip-compressed.
// It is written by a single goroutine (os/exec and remotecommand each copy one
// stream on its own goroutine) and is not safe for concurrent writes.
type Output struct {
	text strings.Builder // plain mode; String() hands back its buffer without copying

	zw   *gzip.Writer // gzip mode
	zbuf bytes.Buffer
	n    int64 // bytes written before compression
	done bool
	err  error
}

// New returns an Output for the requested encoding. Anything other than Gzip
// selects plain text.
func New(encoding string) *Output {
	o := &Output{}
	if encoding == Gzip {
		// BestSpeed: the agent sits in a CPU-limited pod, and the first level
		// already takes command output most of the way (484 MB of Prometheus
		// series JSON compresses to 17 MB).
		o.zw, _ = gzip.NewWriterLevel(&o.zbuf, gzip.BestSpeed) // error only for an invalid level
	}
	return o
}

// Write implements io.Writer.
func (o *Output) Write(p []byte) (int, error) {
	o.n += int64(len(p))
	if o.zw != nil {
		return o.zw.Write(p)
	}
	return o.text.Write(p)
}

// Encoding reports the encoding the output is collected in: Gzip or "".
func (o *Output) Encoding() string {
	if o.zw != nil {
		return Gzip
	}
	return ""
}

// Len is the number of bytes written, before any compression.
func (o *Output) Len() int64 { return o.n }

// Text returns the collected output in plain mode, "" in gzip mode.
func (o *Output) Text() string { return o.text.String() }

// GzipBase64 flushes the compressor and returns the compressed output
// base64-encoded. It is "" in plain mode. Safe to call more than once.
func (o *Output) GzipBase64() (string, error) {
	if o.zw == nil {
		return "", nil
	}
	if !o.done {
		o.done = true
		o.err = o.zw.Close()
	}
	if o.err != nil {
		return "", o.err
	}
	return base64.StdEncoding.EncodeToString(o.zbuf.Bytes()), nil
}

// AddTo sets the stdout fields of a response map: "stdout" in plain mode;
// "stdout_gzip_b64", "output_encoding" and "stdout_bytes" in gzip mode.
func (o *Output) AddTo(m map[string]any) error {
	if o.zw == nil {
		m["stdout"] = o.Text()
		return nil
	}
	enc, err := o.GzipBase64()
	if err != nil {
		return err
	}
	m["stdout_gzip_b64"] = enc
	m[Param] = Gzip
	m["stdout_bytes"] = o.n
	return nil
}
