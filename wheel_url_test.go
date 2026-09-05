package pyodide

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAddWheelURL(t *testing.T) {
	r := newTestRuntime(t)
	wheel := buildWheel("remote-1.0.dist-info", merge(wheelMeta("remote", "1.0"), map[string]string{
		"remote.py": "MESSAGE = 'hello from a URL'\n",
	}))
	native := buildWheel("native-1.0.dist-info", map[string]string{
		"native-1.0.dist-info/WHEEL": "Wheel-Version: 1.0\nTag: cp314-cp314-linux_x86_64\n",
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/redirect.whl":
			http.Redirect(w, req, "/remote-1.0-py3-none-any.whl?download=1", http.StatusFound)
		case "/remote-1.0-py3-none-any.whl":
			w.Write(wheel)
		case "/native.whl":
			w.Write(native)
		case "/broken.whl":
			w.Write([]byte("not a zip file"))
		case "/truncated.whl":
			w.Header().Set("Content-Length", "1000")
			w.Write([]byte("incomplete"))
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	// Verify before installation, including when the distribution is already present.
	good := fmt.Sprintf("%s/redirect.whl#sha256=%x", server.URL, sha256.Sum256(wheel))
	bad := server.URL + "/remote-1.0-py3-none-any.whl#sha256=" + strings.Repeat("0", 64)
	if err := r.AddWheelURL(t.Context(), bad); err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("bad checksum: %v", err)
	}
	if packages, err := r.Packages(); err != nil || len(packages) != 0 {
		t.Fatalf("installed a rejected wheel: %v, %v", packages, err)
	}
	if err := r.AddWheelURL(t.Context(), good); err != nil {
		t.Fatal(err)
	}
	out, err := r.Output(t.Context(), "import remote; print(remote.MESSAGE)")
	if err != nil || out != "hello from a URL\n" {
		t.Fatalf("output %q, error %v", out, err)
	}

	for _, tc := range []struct{ source, want string }{
		{bad, "SHA-256 mismatch"},
		{server.URL + "/missing.whl", "404"},
		{server.URL + "/broken.whl", "zip"},
		{server.URL + "/truncated.whl", "unexpected EOF"},
		{server.URL + "/native.whl", "not pure python"},
		{server.URL + "/remote.whl#sha256=bad", "64 hex digits"},
		{server.URL + "/remote.whl#md5=bad", "sha256="},
		{server.URL + "/not-a-wheel", ".whl filename"},
		{"file:///tmp/remote.whl", "http or https"},
		{"https:///remote.whl", "http or https"},
	} {
		t.Run(tc.want+tc.source, func(t *testing.T) {
			err := r.AddWheelURL(t.Context(), tc.source)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	if err := r.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := r.AddWheelURL(t.Context(), good); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed runtime: %v", err)
	}
}

func TestAddWheelURLCancellation(t *testing.T) {
	r := newTestRuntime(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		cancel()
		<-req.Context().Done()
	}))
	defer server.Close()
	if err := r.AddWheelURL(ctx, server.URL+"/slow.whl"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}
