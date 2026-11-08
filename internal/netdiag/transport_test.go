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
		{"deadline", context.DeadlineExceeded, "transport timeout"},
		{"dial timeout", &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, "transport timeout"},
		{"wrapped dial timeout", fmt.Errorf("private wrapper: %w", &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}), "transport timeout"},
		{"DNS timeout", &net.DNSError{Name: "private.example", IsTimeout: true}, "transport timeout"},
		{"canceled", context.Canceled, "transport canceled"},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "connection refused"},
		{"DNS", &net.DNSError{Name: "private.example"}, "DNS lookup failed"},
		{"certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "TLS verification failed"},
		{"hostname", x509.HostnameError{Host: "private.example"}, "TLS verification failed"},
		{"unknown", errors.New("private peer detail"), "transport failure"},
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
	client := &http.Client{Transport: &http.Transport{}, Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	listen := func(t *testing.T) net.Listener {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if errors.Is(err, os.ErrPermission) {
			t.Skipf("loopback listener unavailable: %v", err)
		}
		if err != nil {
			t.Fatal(err)
		}
		return listener
	}
	t.Run("refused", func(t *testing.T) {
		listener := listen(t)
		endpoint := "http://" + listener.Addr().String()
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		resp, err := client.Post(endpoint, "application/json", nil)
		if err == nil {
			resp.Body.Close()
			t.Skip("closed loopback port was reused")
		}
		if got := TransportFailure(err); got != Refused {
			t.Errorf("closed loopback port classified as %q, want %q", got, Refused)
		}
	})
	t.Run("untrusted TLS", func(t *testing.T) {
		relay := &httptest.Server{
			Listener: listen(t),
			Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("untrusted TLS request reached the handler")
				w.WriteHeader(http.StatusNoContent)
			})},
		}
		relay.StartTLS()
		defer relay.Close()
		resp, err := client.Post(relay.URL, "application/json", nil)
		if err == nil {
			resp.Body.Close()
			t.Fatal("POST with untrusted certificate unexpectedly succeeded")
		}
		if got := TransportFailure(err); got != TLSVerificationFailure {
			t.Errorf("untrusted TLS classified as %q, want %q", got, TLSVerificationFailure)
		}
	})
}
