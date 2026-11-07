// Package natstest runs a real NATS server in a Docker container for tests, so
// they exercise the same server the backend runs against. Tests skip cleanly
// when Docker is unavailable.
package natstest

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// DefaultImage is the server image the tests run against.
const DefaultImage = "nats:2.14.9"

// EnvImage overrides DefaultImage, for trying another server version.
const EnvImage = "NATS_TEST_IMAGE"

// Server is a running containerized NATS server.
type Server struct {
	// URL is the client address on the host loopback.
	URL string
	// Container is the container name.
	Container string
}

type settings struct {
	serverArgs []string
	mounts     []string
}

// Option customizes the container.
type Option func(*settings)

// WithArgs appends raw nats-server arguments after the defaults.
func WithArgs(args ...string) Option {
	return func(s *settings) { s.serverArgs = append(s.serverArgs, args...) }
}

// Mount makes hostPath available read-only inside the container at containerPath.
func Mount(hostPath, containerPath string) Option {
	return func(s *settings) { s.mounts = append(s.mounts, hostPath+":"+containerPath+":ro") }
}

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errOut.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Start launches a JetStream-enabled server and stops it when the test ends. It
// skips the test when Docker or the image is unavailable.
func Start(t testing.TB, opts ...Option) *Server {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if _, err := docker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Skipf("docker is not available: %v", err)
	}
	image := DefaultImage
	if v := os.Getenv(EnvImage); v != "" {
		image = v
	}
	if _, err := docker(ctx, "image", "inspect", image); err != nil {
		if _, err := docker(ctx, "pull", image); err != nil {
			t.Skipf("image %s is not available: %v", image, err)
		}
	}

	var cfg settings
	for _, opt := range opts {
		opt(&cfg)
	}
	args := []string{"run", "-d", "--rm", "-p", "127.0.0.1::4222"}
	for _, mount := range cfg.mounts {
		args = append(args, "-v", mount)
	}
	args = append(args, image, "-js", "-sd", "/data")
	args = append(args, cfg.serverArgs...)

	id, err := docker(ctx, args...)
	if err != nil {
		t.Skipf("cannot start %s: %v", image, err)
	}
	t.Cleanup(func() {
		stopCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_, _ = docker(stopCtx, "rm", "-f", id)
	})

	hostPort, err := docker(ctx, "port", id, "4222/tcp")
	if err != nil {
		t.Fatalf("cannot resolve the published port: %v", err)
	}
	// Output may list IPv4 and IPv6 bindings on separate lines; the loopback one is first.
	addr := strings.SplitN(hostPort, "\n", 2)[0]
	_, port, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		t.Fatalf("unexpected port mapping %q: %v", hostPort, err)
	}
	srv := &Server{URL: "nats://127.0.0.1:" + port, Container: id}
	waitReady(t, srv.URL[len("nats://"):])
	return srv
}

// waitReady waits until the server answers the NATS INFO handshake.
func waitReady(t testing.TB, addr string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			buf := make([]byte, 5)
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			n, _ := conn.Read(buf)
			_ = conn.Close()
			if string(buf[:n]) == "INFO " {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("NATS server at %s did not become ready", addr)
}
