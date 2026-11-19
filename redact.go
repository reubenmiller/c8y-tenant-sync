package main

import (
	"bytes"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
)

// redactedValue replaces secret values in the output
const redactedValue = "***"

// minRedactLength is the shortest value which is masked; shorter values
// (e.g. "1") would mask unrelated output
const minRedactLength = 4

// Redactor masks secret values in text. Values can be added at any time.
type Redactor struct {
	mu     sync.RWMutex
	values []string
}

// Add registers secret values to mask
func (r *Redactor) Add(values ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, value := range values {
		if len(value) < minRedactLength || strings.ContainsAny(value, "\r\n") {
			continue
		}
		duplicate := false
		for _, existing := range r.values {
			if existing == value {
				duplicate = true
				break
			}
		}
		if !duplicate {
			r.values = append(r.values, value)
		}
	}
	// Longest first, so a value containing another value is fully masked
	sort.SliceStable(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

// Redact masks all registered values in the text
func (r *Redactor) Redact(text string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, value := range r.values {
		text = strings.ReplaceAll(text, value, redactedValue)
	}
	return text
}

// redactingWriter masks secrets in the lines written to it. Secrets never
// contain line breaks, so output is masked line by line, and a partial line
// is held back until it is complete (or the writer is flushed).
type redactingWriter struct {
	redactor *Redactor
	out      io.Writer
	mu       sync.Mutex
	pending  []byte
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending = append(w.pending, p...)
	if i := bytes.LastIndexByte(w.pending, '\n'); i >= 0 {
		complete := w.pending[:i+1]
		if _, err := io.WriteString(w.out, w.redactor.Redact(string(complete))); err != nil {
			return 0, err
		}
		w.pending = append([]byte(nil), w.pending[i+1:]...)
	}
	return len(p), nil
}

// Flush writes a pending partial line
func (w *redactingWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	_, err := io.WriteString(w.out, w.redactor.Redact(string(w.pending)))
	w.pending = nil
	return err
}

// RedactStdio routes os.Stdout and os.Stderr through the redactor, so secrets
// are masked in all output, including the HTTP debug output of the go-c8y
// client (which writes to os.Stderr directly). The returned function
// restores them and flushes the remaining output; call it before exiting.
func RedactStdio(redactor *Redactor) (restore func(), err error) {
	type stream struct {
		target   **os.File
		original *os.File
		reader   *os.File
		writer   *os.File
		done     chan struct{}
	}
	var streams []*stream
	for _, target := range []**os.File{&os.Stdout, &os.Stderr} {
		reader, writer, err := os.Pipe()
		if err != nil {
			for _, s := range streams {
				*s.target = s.original
				s.writer.Close()
			}
			return nil, err
		}
		s := &stream{target: target, original: *target, reader: reader, writer: writer, done: make(chan struct{})}
		out := &redactingWriter{redactor: redactor, out: s.original}
		go func() {
			defer close(s.done)
			_, _ = io.Copy(out, s.reader)
			_ = out.Flush()
		}()
		*target = writer
		streams = append(streams, s)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			for _, s := range streams {
				*s.target = s.original
				s.writer.Close()
				<-s.done
				s.reader.Close()
			}
		})
	}, nil
}
