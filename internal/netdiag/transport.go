package netdiag

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"syscall"
)

// Transport diagnostics contain only fixed text.
const (
	Timeout                = "transport timeout"
	Canceled               = "transport canceled"
	Refused                = "connection refused"
	DNSFailure             = "DNS lookup failed"
	TLSVerificationFailure = "TLS verification failed"
	Failure                = "transport failure"
)

// TransportFailure returns a fixed diagnostic for an HTTP client
// error. It never includes the endpoint or text supplied by the peer.
func TransportFailure(err error) string {
	var timeout net.Error
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return Timeout
	case errors.Is(err, context.Canceled):
		return Canceled
	case errors.Is(err, syscall.ECONNREFUSED):
		return Refused
	case errors.As(err, &dns):
		return DNSFailure
	case errors.As(err, &cert), errors.As(err, &authority), errors.As(err, &hostname):
		return TLSVerificationFailure
	default:
		return Failure
	}
}
