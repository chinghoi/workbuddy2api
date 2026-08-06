package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// fullCaptureDir intentionally remains enabled during compatibility development.
// data/ is gitignored. Every /v1/responses call gets complete paired artifacts:
// original request, translated upstream request, raw upstream stream, final output,
// and response metadata. Remove this capture after the adapter is stable.
var fullCaptureDir = "./data/full_io"
var fullCaptureSequence atomic.Uint64

type fullIOCaptureWriter struct {
	http.ResponseWriter
	mu              sync.Mutex
	id              string
	output          *os.File
	upstreamOutput  *os.File
	statusCode      int
	wroteHeader     bool
	responseHeaders http.Header
}

func startFullIOCapture(w http.ResponseWriter, requestBody []byte) *fullIOCaptureWriter {
	if err := os.MkdirAll(fullCaptureDir, 0o700); err != nil {
		return nil
	}
	sequence := fullCaptureSequence.Add(1)
	id := fmt.Sprintf("%s-%06d", time.Now().Format("20060102T150405.000000000"), sequence)
	if err := os.WriteFile(filepath.Join(fullCaptureDir, id+"-request.json"), requestBody, 0o600); err != nil {
		return nil
	}
	output, err := os.OpenFile(filepath.Join(fullCaptureDir, id+"-output.raw"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	upstreamOutput, err := os.OpenFile(filepath.Join(fullCaptureDir, id+"-upstream-output.raw"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = output.Close()
		return nil
	}
	return &fullIOCaptureWriter{
		ResponseWriter:  w,
		id:              id,
		output:          output,
		upstreamOutput:  upstreamOutput,
		responseHeaders: make(http.Header),
	}
}

func (w *fullIOCaptureWriter) RecordUpstreamRequest(body []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = os.WriteFile(filepath.Join(fullCaptureDir, w.id+"-upstream-request.json"), body, 0o600)
}

func (w *fullIOCaptureWriter) RecordUpstreamError(status int, body []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.upstreamOutput == nil {
		return
	}
	_, _ = fmt.Fprintf(w.upstreamOutput, "\n--- HTTP %d ---\n", status)
	_, _ = w.upstreamOutput.Write(body)
	_, _ = w.upstreamOutput.Write([]byte("\n"))
}

func (w *fullIOCaptureWriter) TeeUpstream(reader io.Reader) io.Reader {
	return io.TeeReader(reader, lockedCaptureWriter{capture: w, upstream: true})
}

type lockedCaptureWriter struct {
	capture  *fullIOCaptureWriter
	upstream bool
}

func (w lockedCaptureWriter) Write(data []byte) (int, error) {
	w.capture.mu.Lock()
	defer w.capture.mu.Unlock()
	if !w.upstream || w.capture.upstreamOutput == nil {
		return len(data), nil
	}
	return w.capture.upstreamOutput.Write(data)
}

func (w *fullIOCaptureWriter) WriteHeader(statusCode int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.statusCode = statusCode
		w.wroteHeader = true
		w.responseHeaders = w.ResponseWriter.Header().Clone()
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *fullIOCaptureWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.statusCode = http.StatusOK
		w.wroteHeader = true
		w.responseHeaders = w.ResponseWriter.Header().Clone()
	}
	if w.output != nil {
		_, _ = w.output.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *fullIOCaptureWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.wroteHeader {
		w.statusCode = http.StatusOK
		w.wroteHeader = true
		w.responseHeaders = w.ResponseWriter.Header().Clone()
	}
	if w.output != nil {
		_ = w.output.Sync()
	}
	if w.upstreamOutput != nil {
		_ = w.upstreamOutput.Sync()
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *fullIOCaptureWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	if len(w.responseHeaders) == 0 {
		w.responseHeaders = w.ResponseWriter.Header().Clone()
	}
	metadata, _ := json.MarshalIndent(map[string]any{
		"status":  w.statusCode,
		"headers": w.responseHeaders,
	}, "", "  ")
	_ = os.WriteFile(filepath.Join(fullCaptureDir, w.id+"-response-meta.json"), metadata, 0o600)
	var firstErr error
	if w.output != nil {
		firstErr = w.output.Close()
		w.output = nil
	}
	if w.upstreamOutput != nil {
		if err := w.upstreamOutput.Close(); firstErr == nil {
			firstErr = err
		}
		w.upstreamOutput = nil
	}
	return firstErr
}

func (w *fullIOCaptureWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
