package forwarder

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

func TestTransportFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "forward transport timeout"},
		{"dial timeout", &net.OpError{Op: "dial", Err: os.ErrDeadlineExceeded}, "forward transport timeout"},
		{"DNS timeout", &net.DNSError{Name: "private.example", IsTimeout: true}, "forward transport timeout"},
		{"canceled", context.Canceled, "forward transport canceled"},
		{"refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "scanrelay connection refused"},
		{"DNS", &net.DNSError{Name: "private.example"}, "scanrelay DNS lookup failed"},
		{"certificate", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "scanrelay TLS verification failed"},
		{"hostname", x509.HostnameError{Host: "private.example"}, "scanrelay TLS verification failed"},
		{"unknown", errors.New("private peer detail"), "forward transport failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &url.Error{Op: "Post", URL: "https://private.example/webhook", Err: tc.err}
			if got := TransportFailure(err); got != tc.want {
				t.Errorf("TransportFailure = %q, want %q", got, tc.want)
			}
		})
	}
}
