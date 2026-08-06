package server

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFullIOCaptureKeepsCompleteRequestAndOutput(t *testing.T) {
	oldDir := fullCaptureDir
	fullCaptureDir = t.TempDir()
	defer func() { fullCaptureDir = oldDir }()

	recorder := httptest.NewRecorder()
	request := []byte(`{"model":"glm-5.2","input":"hello"}`)
	capture := startFullIOCapture(recorder, request)
	if capture == nil {
		t.Fatal("capture was not created")
	}
	upstreamRequest := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hello"}]}`)
	capture.RecordUpstreamRequest(upstreamRequest)
	_, _ = io.Copy(io.Discard, capture.TeeUpstream(strings.NewReader("data: upstream\n\n")))
	_, _ = capture.Write([]byte("event: response.completed\n"))
	capture.Flush()
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}

	requestPath := filepath.Join(fullCaptureDir, capture.id+"-request.json")
	upstreamRequestPath := filepath.Join(fullCaptureDir, capture.id+"-upstream-request.json")
	upstreamOutputPath := filepath.Join(fullCaptureDir, capture.id+"-upstream-output.raw")
	outputPath := filepath.Join(fullCaptureDir, capture.id+"-output.raw")
	metaPath := filepath.Join(fullCaptureDir, capture.id+"-response-meta.json")
	for _, path := range []string{requestPath, upstreamRequestPath, upstreamOutputPath, outputPath, metaPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	assertCapturedFile(t, requestPath, string(request))
	assertCapturedFile(t, upstreamRequestPath, string(upstreamRequest))
	assertCapturedFile(t, upstreamOutputPath, "data: upstream\n\n")
	assertCapturedFile(t, outputPath, "event: response.completed\n")
	assertCapturedContains(t, metaPath, `"status": 200`)
}

func assertCapturedFile(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("%s=%q want=%q", path, got, want)
	}
}

func assertCapturedContains(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), want) {
		t.Fatalf("%s missing %q: %s", path, want, got)
	}
}
