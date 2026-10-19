package signaling

import (
	"context"
	"errors"
)

// DirectService issues per-device DNS names and certificates so hosts can
// accept direct HTTPS connections. Implemented in direct_acme.go.
type DirectService struct{ impl directImpl }

type directImpl interface {
	Register(deviceID, deviceKey, ip string) (string, error)
	IssueCert(ctx context.Context, deviceID, deviceKey, ip, csr string) (string, error)
}

// Register binds a device to a hostname pointing at ip.
func (d *DirectService) Register(deviceID, deviceKey, ip string) (string, error) {
	if d == nil || d.impl == nil {
		return "", errors.New("direct HTTPS not configured")
	}
	return d.impl.Register(deviceID, deviceKey, ip)
}

// IssueCert signs the host's CSR for its hostname.
func (d *DirectService) IssueCert(ctx context.Context, deviceID, deviceKey, ip, csr string) (string, error) {
	if d == nil || d.impl == nil {
		return "", errors.New("direct HTTPS not configured")
	}
	return d.impl.IssueCert(ctx, deviceID, deviceKey, ip, csr)
}
