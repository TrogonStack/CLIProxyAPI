package natsstore

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Server release the backend is designed and tested against. Older servers that
// still support per-key TTL are accepted and reported, not refused.
const (
	TargetMajor = 2
	TargetMinor = 14

	// keyTTLAPILevel is the JetStream API level that introduced per-key TTL.
	keyTTLAPILevel = 1
)

// ServerFeatures describes what the connected server offers.
type ServerFeatures struct {
	// Version is the server version as reported on connect.
	Version string
	// Major, Minor and Patch are the parsed Version, zero when it did not parse.
	Major, Minor, Patch int
	// APILevel is the JetStream API level the account sees.
	APILevel int
	// KeyTTL reports per-key TTL and bucket limit markers support.
	KeyTTL bool
}

// AtLeast reports whether the server version is at least major.minor.
func (f ServerFeatures) AtLeast(major, minor int) bool {
	return f.Major > major || (f.Major == major && f.Minor >= minor)
}

// Probe reads the server version and JetStream feature level. It fails when
// JetStream is not enabled for the account.
func Probe(ctx context.Context, nc *nats.Conn, js jetstream.JetStream) (ServerFeatures, error) {
	info, err := js.AccountInfo(ctx)
	if err != nil {
		return ServerFeatures{}, fmt.Errorf("JetStream is not available: %w", err)
	}
	features := ServerFeatures{Version: nc.ConnectedServerVersion(), APILevel: info.API.Level}
	features.Major, features.Minor, features.Patch = parseVersion(features.Version)
	features.KeyTTL = features.APILevel >= keyTTLAPILevel
	return features, nil
}

// parseVersion reads `major.minor.patch` and ignores any pre-release suffix.
func parseVersion(version string) (major, minor, patch int) {
	version = strings.TrimPrefix(version, "v")
	if i := strings.IndexAny(version, "-+"); i >= 0 {
		version = version[:i]
	}
	parts := strings.Split(version, ".")
	nums := make([]int, 3)
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return 0, 0, 0
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2]
}
