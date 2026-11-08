package netdiag

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestTransportFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, Timeout},
		{"dial timeout", &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, Timeout},
		{"wrapped dial timeout", fmt.Errorf("private wrapper: %w", &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}), Timeout},
		{"DNS timeout", &net.DNSError{Name: "private.example", IsTimeout: true}, Timeout},
		{"canceled", context.Canceled, Canceled},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, Refused},
		{"DNS", &net.DNSError{Name: "private.example"}, DNSFailure},
		{"certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, TLSVerificationFailure},
		{"hostname", x509.HostnameError{Host: "private.example"}, TLSVerificationFailure},
		{"unknown", errors.New("private peer detail"), Failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &url.Error{Op: "Post", URL: "https://private.example/webhook", Err: tc.err}
			if got := TransportFailure(err); got != tc.want {
				t.Errorf("TransportFailure = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHTTPClientTransportFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedURL := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	relay := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("untrusted TLS request reached the handler")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer relay.Close()
	client := &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for _, tc := range []struct{ endpoint, want string }{
		{refusedURL, Refused},
		{relay.URL, TLSVerificationFailure},
	} {
		resp, err := client.Post(tc.endpoint, "application/json", nil)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("POST %s unexpectedly succeeded", tc.endpoint)
		}
		if got := TransportFailure(err); got != tc.want {
			t.Errorf("POST %s classified as %q, want %q", tc.endpoint, got, tc.want)
		}
	}
}
