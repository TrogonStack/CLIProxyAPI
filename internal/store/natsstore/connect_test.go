package natsstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore/natstest"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func connect(t *testing.T, env map[string]string, timeout time.Duration) (*nats.Conn, error) {
	t.Helper()
	cfg := SettingsFromEnv(mapEnv(env)).Resolve()
	nc, err := Connect(testContext(t), cfg, mapEnv(env), timeout)
	if err == nil {
		t.Cleanup(nc.Close)
	}
	return nc, err
}

func TestConnectAuthMethods(t *testing.T) {
	const (
		user     = "svc"
		password = "pw-integration-secret"
		token    = "token-integration-secret"
	)
	pair, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := pair.PublicKey()
	seed, _ := pair.Seed()
	dir := t.TempDir()
	conf := filepath.Join(dir, "nats.conf")
	if err := os.WriteFile(conf, []byte("authorization { users = [ { nkey: "+pub+" } ] }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedFile := filepath.Join(dir, "user.seed")
	if err := os.WriteFile(seedFile, append(seed, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("token", func(t *testing.T) {
		srv := natstest.Start(t, natstest.WithArgs("--auth", token))
		for name, value := range map[string]string{"literal": token, "file": "file://" + tokenFile, "env": "env://INTEGRATION_TOKEN"} {
			t.Run(name, func(t *testing.T) {
				if _, err := connect(t, map[string]string{EnvURL: srv.URL, EnvToken: value, "INTEGRATION_TOKEN": token}, 5*time.Second); err != nil {
					t.Fatal(err)
				}
			})
		}
	})

	t.Run("user and password", func(t *testing.T) {
		srv := natstest.Start(t, natstest.WithArgs("--user", user, "--pass", password))
		if _, err := connect(t, map[string]string{EnvURL: srv.URL, EnvUser: user, EnvPassword: password}, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("nkey", func(t *testing.T) {
		srv := natstest.Start(t, natstest.Mount(dir, "/etc/nats-test"), natstest.WithArgs("-c", "/etc/nats-test/nats.conf"))
		for name, value := range map[string]string{"inline": string(seed), "file": seedFile} {
			t.Run(name, func(t *testing.T) {
				if _, err := connect(t, map[string]string{EnvURL: srv.URL, EnvNKey: value}, 5*time.Second); err != nil {
					t.Fatal(err)
				}
			})
		}
	})

	t.Run("wrong credentials fail without leaking them", func(t *testing.T) {
		srv := natstest.Start(t, natstest.WithArgs("--auth", token))
		const wrong = "wrong-token-integration-secret"
		_, err := connect(t, map[string]string{EnvURL: srv.URL, EnvToken: wrong}, 2*time.Second)
		if err == nil {
			t.Fatal("connected with a wrong token")
		}
		if strings.Contains(err.Error(), wrong) || strings.Contains(err.Error(), token) {
			t.Fatalf("error leaks a secret: %v", err)
		}
	})

	t.Run("anonymous is refused by an authenticated server", func(t *testing.T) {
		srv := natstest.Start(t, natstest.WithArgs("--auth", token))
		if _, err := connect(t, map[string]string{EnvURL: srv.URL}, 2*time.Second); err == nil {
			t.Fatal("connected without credentials")
		}
	})
}

func TestConnectErrorsDoNotLeakURLUserinfo(t *testing.T) {
	const url = "nats://svc:urlpass-secret@127.0.0.1:1"
	_, err := connect(t, map[string]string{EnvURL: url}, 500*time.Millisecond)
	if err == nil {
		t.Fatal("connected to a closed port")
	}
	if strings.Contains(err.Error(), "urlpass-secret") {
		t.Fatalf("error leaks URL userinfo: %v", err)
	}
}

func TestTLSClientCertificateNeedsBothHalves(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantSet string
		wantMsg string
	}{
		{"cert alone", map[string]string{EnvCert: "/c.pem"}, EnvCert, "must be set together with " + EnvKey},
		{"key alone", map[string]string{EnvKey: "/k.pem"}, EnvKey, "must be set together with " + EnvCert},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := SettingsFromEnv(mapEnv(tt.env)).Resolve()
			_, err := buildOptions(cfg, OSLookup)
			var setting *SettingError
			if !errors.As(err, &setting) {
				t.Fatalf("err = %v, want SettingError", err)
			}
			if setting.Setting != tt.wantSet || !strings.Contains(setting.Error(), tt.wantMsg) {
				t.Fatalf("err = %v", setting)
			}
		})
	}
	cfg := SettingsFromEnv(mapEnv(map[string]string{EnvCA: "/ca.pem"})).Resolve()
	if opts, err := buildOptions(cfg, OSLookup); err != nil || len(opts) == 0 {
		t.Fatalf("CA alone: %d options, %v", len(opts), err)
	}
}

func TestBuildOptionsRejectsUnsupportedReference(t *testing.T) {
	cfg := SettingsFromEnv(mapEnv(map[string]string{EnvToken: "op://vault/item"})).Resolve()
	_, err := buildOptions(cfg, OSLookup)
	if !errors.Is(err, ErrUnsupportedScheme) {
		t.Fatalf("err = %v", err)
	}
	var setting *SettingError
	if !errors.As(err, &setting) || setting.Setting != EnvToken {
		t.Fatalf("err = %v, want a %s setting error", err, EnvToken)
	}
}

func TestReconnectDelayBacksOffAndCaps(t *testing.T) {
	want := []int{1, 2, 4, 8, 16, 30, 30}
	for i, seconds := range want {
		if got := reconnectDelay(i + 1); got.Seconds() != float64(seconds) {
			t.Errorf("attempt %d: %v, want %ds", i+1, got, seconds)
		}
	}
	if got := reconnectDelay(0); got.Seconds() != 1 {
		t.Errorf("attempt 0: %v", got)
	}
}
