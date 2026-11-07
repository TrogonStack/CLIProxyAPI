package natsstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultConnectTimeout bounds one connection attempt and the initial wait.
	DefaultConnectTimeout = 30 * time.Second
	// MaxReconnectDelay caps the exponential reconnect backoff.
	MaxReconnectDelay = 30 * time.Second

	clientName = "cliproxyapi"
)

// ErrNotConnected is returned when the initial connection is not established in time.
var ErrNotConnected = errors.New("not connected to NATS")

// reconnectDelay doubles per attempt from one second and stops at MaxReconnectDelay.
func reconnectDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 6 {
		return MaxReconnectDelay
	}
	return min(MaxReconnectDelay, time.Duration(1<<uint(attempts-1))*time.Second)
}

// buildOptions turns the resolved configuration into client options. Secrets are
// resolved here so a bad reference fails before any network traffic, and no error
// it returns carries a secret value.
func buildOptions(cfg Config, env Lookup) ([]nats.Option, error) {
	var opts []nats.Option
	switch auth := cfg.Auth.(type) {
	case CredentialsAuth:
		path, err := auth.File.Path()
		if err != nil {
			return nil, &SettingError{Setting: EnvCreds, Err: err}
		}
		opts = append(opts, nats.UserCredentials(path))
	case NKeyAuth:
		seed, err := auth.Seed.resolve()
		if err != nil {
			return nil, &SettingError{Setting: EnvNKey, Err: err}
		}
		pair, err := nkeys.FromSeed([]byte(seed))
		if err != nil {
			return nil, &SettingError{Setting: EnvNKey, Err: fmt.Errorf("invalid NKey seed: %w", err)}
		}
		pub, err := pair.PublicKey()
		if err != nil {
			return nil, &SettingError{Setting: EnvNKey, Err: err}
		}
		opts = append(opts, nats.Nkey(pub, pair.Sign))
	case UserPasswordAuth:
		password, err := auth.Password.resolve(env)
		if err != nil {
			return nil, &SettingError{Setting: EnvPassword, Err: err}
		}
		opts = append(opts, nats.UserInfo(auth.User, password))
	case JWTAuth:
		jwt, err := auth.JWT.resolve(env)
		if err != nil {
			return nil, &SettingError{Setting: EnvJWT, Err: err}
		}
		jwt = strings.TrimSpace(jwt)
		sign := func([]byte) ([]byte, error) { return nil, nil }
		if auth.Seed != nil {
			seed, err := auth.Seed.resolve(env)
			if err != nil {
				return nil, &SettingError{Setting: EnvSeed, Err: err}
			}
			pair, err := nkeys.FromSeed([]byte(strings.TrimSpace(seed)))
			if err != nil {
				return nil, &SettingError{Setting: EnvSeed, Err: fmt.Errorf("invalid NKey seed: %w", err)}
			}
			sign = pair.Sign
		}
		opts = append(opts, nats.UserJWT(func() (string, error) { return jwt, nil }, sign))
	case TokenAuth:
		token, err := auth.Token.resolve(env)
		if err != nil {
			return nil, &SettingError{Setting: EnvToken, Err: err}
		}
		opts = append(opts, nats.Token(token))
	}

	if !cfg.TLS.Configured() {
		return opts, nil
	}
	opts = append(opts, nats.Secure())
	pair, err := cfg.TLS.clientCertificate()
	if err != nil {
		return nil, err
	}
	if pair != nil {
		cert, err := pair[0].Path()
		if err != nil {
			return nil, &SettingError{Setting: EnvCert, Err: err}
		}
		key, err := pair[1].Path()
		if err != nil {
			return nil, &SettingError{Setting: EnvKey, Err: err}
		}
		opts = append(opts, nats.ClientCert(cert, key))
	}
	if cfg.TLS.CA != nil {
		ca, err := cfg.TLS.CA.Path()
		if err != nil {
			return nil, &SettingError{Setting: EnvCA, Err: err}
		}
		opts = append(opts, nats.RootCAs(ca))
	}
	return opts, nil
}

// Connect opens a client connection that keeps reconnecting with capped backoff,
// and waits until the first connection is up or the timeout or context ends.
// Server addresses are logged with any URL userinfo removed, and secrets never are.
func Connect(ctx context.Context, cfg Config, env Lookup, timeout time.Duration) (*nats.Conn, error) {
	if timeout <= 0 {
		timeout = DefaultConnectTimeout
	}
	opts, err := buildOptions(cfg, env)
	if err != nil {
		return nil, err
	}

	var lastErr atomic.Value
	remember := func(err error) {
		if err != nil {
			lastErr.Store(err.Error())
		}
	}
	servers := redactServers(cfg.Servers)
	opts = append(opts,
		nats.Name(clientName),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.Timeout(timeout),
		nats.CustomReconnectDelay(reconnectDelay),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			remember(err)
			log.WithError(err).Warn("NATS disconnected, will attempt reconnect")
		}),
		nats.ReconnectHandler(func(*nats.Conn) { log.Info("NATS reconnected") }),
		nats.ClosedHandler(func(*nats.Conn) { log.Info("NATS connection closed") }),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			remember(err)
			log.WithError(err).Warn("NATS error")
		}),
	)

	log.Infof("connecting to NATS servers %v with %s authentication", servers, cfg.Auth.Description())
	nc, err := nats.Connect(strings.Join(cfg.Servers, ","), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to NATS servers %v: %w", servers, scrub(err, cfg.Servers))
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for !nc.IsConnected() {
		var reason error
		select {
		case <-ctx.Done():
			reason = ctx.Err()
		case <-deadline.C:
			reason = ErrNotConnected
		case <-tick.C:
			if nc.IsClosed() {
				reason = ErrNotConnected
			}
		}
		if reason != nil {
			nc.Close()
			if last, ok := lastErr.Load().(string); ok {
				return nil, fmt.Errorf("failed to connect to NATS servers %v: %w (last error: %s)", servers, reason, last)
			}
			return nil, fmt.Errorf("failed to connect to NATS servers %v: %w", servers, reason)
		}
	}
	log.Infof("connected to NATS server %s", nc.ConnectedUrlRedacted())
	return nc, nil
}

// scrub removes every configured server string from an error message so URL userinfo cannot leak.
func scrub(err error, servers []string) error {
	msg := err.Error()
	for _, server := range servers {
		if safe := redactServer(server); safe != server {
			msg = strings.ReplaceAll(msg, server, safe)
		}
	}
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}
