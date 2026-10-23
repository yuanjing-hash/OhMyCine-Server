package quark

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
)

// Only this finite vocabulary can leave the private receipt. In particular a
// transport error after Do is diagnostic, never evidence of non-dispatch.
func safeUploadSubmitReason(reason string) string {
	switch reason {
	case "transport_canceled", "transport_timeout", "transport_dns", "transport_tls", "transport_error", "object_http_status", "object_body_read", "object_body_limit", "object_etag_missing", "object_etag_invalid":
		return reason
	}
	return ""
}

func uploadTransportReason(ctx context.Context, err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "transport_canceled"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.As(err, &timeout) && timeout.Timeout() {
		return "transport_timeout"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "transport_dns"
	}
	var verification *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	if errors.As(err, &verification) || errors.As(err, &record) {
		return "transport_tls"
	}
	return "transport_error"
}
