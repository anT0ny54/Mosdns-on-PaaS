package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestBackendIdleTimeout(t *testing.T) {
	tests := []struct {
		name  string
		input int
		want  time.Duration
	}{
		{name: "mosdns default sentinel", input: 0, want: 5 * time.Second},
		{name: "minimum valid explicit", input: 6, want: 1 * time.Second},
		{name: "normal", input: 30, want: 25 * time.Second},
		{name: "capped", input: 120, want: 90 * time.Second},
		{name: "large capped", input: 3600, want: 90 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := backendIdleTimeout(tt.input); got != tt.want {
				t.Fatalf("backendIdleTimeout(%d) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

type failingBody struct {
	data []byte
	err  error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		return 0, b.err
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (b *failingBody) Close() error { return nil }

func validDoHResponseBody(t *testing.T) []byte {
	t.Helper()
	m := new(dns.Msg)
	m.SetReply(&dns.Msg{MsgHdr: dns.MsgHdr{Id: 7, Rcode: dns.RcodeSuccess}})
	m.Question = []dns.Question{{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}}
	b, err := m.Pack()
	if err != nil {
		t.Fatalf("Pack() failed: %v", err)
	}
	return b
}

func TestBufferDoHResponseSetsKnownLengthAndPreservesMessage(t *testing.T) {
	body := validDoHResponseBody(t)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        make(http.Header),
		ContentLength: -1,
		Body:          io.NopCloser(bytes.NewReader(body)),
	}
	resp.Header.Set("Content-Type", "application/dns-message; charset=binary")
	if err := bufferDoHResponse(resp); err != nil {
		t.Fatalf("bufferDoHResponse() failed: %v", err)
	}
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("ContentLength = %d, want %d", resp.ContentLength, len(body))
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll() failed: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("buffered body changed")
	}
}

func TestBufferDoHResponseRejectsPrematureEOF(t *testing.T) {
	body := validDoHResponseBody(t)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/dns-message"}},
		ContentLength: -1,
		Body:          &failingBody{data: body[:len(body)-1], err: io.ErrUnexpectedEOF},
	}
	if err := bufferDoHResponse(resp); err == nil {
		t.Fatal("premature EOF must be rejected before client writes begin")
	}
}

func TestBufferDoHResponseRejectsOversizedUnknownLength(t *testing.T) {
	body := bytes.Repeat([]byte{0}, maxDoHResponseBytes+1)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/dns-message"}},
		ContentLength: -1,
		Body:          io.NopCloser(bytes.NewReader(body)),
	}
	if err := bufferDoHResponse(resp); err == nil {
		t.Fatal("unknown-length oversized response must be rejected")
	}
}

func TestBufferDoHResponseRejectsInvalidDNSMessage(t *testing.T) {
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/dns-message"}},
		ContentLength: 4,
		Body:          io.NopCloser(bytes.NewReader([]byte{0, 1, 2, 3})),
	}
	if err := bufferDoHResponse(resp); err == nil {
		t.Fatal("invalid DNS body must be rejected")
	}
}

func TestBufferDoHResponseRejectsDeclaredLengthMismatch(t *testing.T) {
	body := validDoHResponseBody(t)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/dns-message"}},
		ContentLength: int64(len(body) + 1),
		Body:          io.NopCloser(bytes.NewReader(body)),
	}
	if err := bufferDoHResponse(resp); err == nil {
		t.Fatal("declared-length mismatch must be rejected")
	}
}

func newTestDoHProxy(t *testing.T, backendHandler http.Handler) *httputil.ReverseProxy {
	t.Helper()
	backend := httptest.NewServer(backendHandler)
	t.Cleanup(backend.Close)
	target, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("Parse backend URL failed: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = bufferDoHResponse
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}
	return proxy
}

func doProxyRequest(t *testing.T, proxy http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://proxy.test/dns-query", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/dns-message")
	resp := httptest.NewRecorder()
	proxy.ServeHTTP(resp, req)
	return resp
}

func TestReverseProxyBuffersUnknownLengthDoHResponse(t *testing.T) {
	body := validDoHResponseBody(t)
	proxy := newTestDoHProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		flusher := w.(http.Flusher)
		_, _ = w.Write(body)
		flusher.Flush()
	}))

	resp := doProxyRequest(t, proxy, body)
	if resp.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", resp.Code)
	}
	if got := resp.Header().Get("Content-Length"); got != fmt.Sprint(len(body)) {
		t.Fatalf("proxy Content-Length = %q, want %d", got, len(body))
	}
	if !bytes.Equal(resp.Body.Bytes(), body) {
		t.Fatalf("proxy returned a changed DNS body")
	}
}

func TestReverseProxyRejectsTruncatedUnknownLengthDoHResponse(t *testing.T) {
	body := validDoHResponseBody(t)
	proxy := newTestDoHProxy(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/dns-message")
		flusher := w.(http.Flusher)
		_, _ = w.Write(body[:len(body)-1])
		flusher.Flush()
	}))

	resp := doProxyRequest(t, proxy, body)
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("proxy status = %d, want 502", resp.Code)
	}
}
