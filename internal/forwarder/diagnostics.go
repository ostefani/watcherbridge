package forwarder

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"syscall"
)

// TransportFailure returns a fixed diagnostic for a ScanRelay HTTP client
// error. It never includes the endpoint or text supplied by the peer.
func TransportFailure(err error) string {
	var timeout net.Error
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return "forward transport timeout"
	case errors.Is(err, context.Canceled):
		return "forward transport canceled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "scanrelay connection refused"
	case errors.As(err, &dns):
		return "scanrelay DNS lookup failed"
	case errors.As(err, &cert), errors.As(err, &authority), errors.As(err, &hostname):
		return "scanrelay TLS verification failed"
	default:
		return "forward transport failure"
	}
}
