package natsstore

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
)

func TestProbeReportsTargetServer(t *testing.T) {
	srv := natstest.Start(t)
	nc, err := connect(t, map[string]string{EnvURL: srv.URL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	features, err := Probe(testContext(t), nc, js)
	if err != nil {
		t.Fatal(err)
	}
	if !features.AtLeast(TargetMajor, TargetMinor) {
		t.Fatalf("server %s is older than %d.%d", features.Version, TargetMajor, TargetMinor)
	}
	if !features.KeyTTL {
		t.Fatalf("server %s reports no per-key TTL (API level %d)", features.Version, features.APILevel)
	}
}
