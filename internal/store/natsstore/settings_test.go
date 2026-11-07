package natsstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nats-io/nkeys"
)

func mapEnv(values map[string]string) Lookup {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func newSeed(t *testing.T) string {
	t.Helper()
	pair, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	seed, err := pair.Seed()
	if err != nil {
		t.Fatal(err)
	}
	return string(seed)
}

func TestSettingsServers(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want []string
	}{
		{"default", "", []string{DefaultURL}},
		{"single", "nats://a:4222", []string{"nats://a:4222"}},
		{"list trimmed", " nats://a:4222 , nats://b:4222,, ", []string{"nats://a:4222", "nats://b:4222"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SettingsFromEnv(mapEnv(map[string]string{EnvURL: tt.url})).Resolve().Servers
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("servers = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSettingsAuthPrecedence(t *testing.T) {
	seed := newSeed(t)
	all := map[string]string{
		EnvCreds:    "/run/creds/user.creds",
		EnvNKey:     seed,
		EnvUser:     "alice",
		EnvPassword: "pw",
		EnvToken:    "tok",
		EnvJWT:      "jwt",
		EnvSeed:     seed,
	}
	steps := []struct {
		drop string
		want string
	}{
		{"", "credentials file"},
		{EnvCreds, "NKey"},
		{EnvNKey, "user/password"},
		{EnvPassword, "token"},
		{EnvToken, "token"}, // user without a password is used as a token
		{EnvUser, "JWT"},
		{EnvSeed, "JWT without signature"},
		{EnvJWT, "none"},
	}
	env := map[string]string{}
	for k, v := range all {
		env[k] = v
	}
	for _, step := range steps {
		delete(env, step.drop)
		got := SettingsFromEnv(mapEnv(env)).Resolve().Auth.Description()
		if got != step.want {
			t.Fatalf("after dropping %q: auth = %q, want %q", step.drop, got, step.want)
		}
	}
}

func TestSettingsEmptyValuesCountAsUnset(t *testing.T) {
	env := map[string]string{EnvCreds: "", EnvNKey: "", EnvUser: "", EnvPassword: "", EnvToken: "", EnvJWT: ""}
	cfg := SettingsFromEnv(mapEnv(env)).Resolve()
	if _, ok := cfg.Auth.(NoAuth); !ok {
		t.Fatalf("auth = %T, want NoAuth", cfg.Auth)
	}
	if cfg.TLS.Configured() {
		t.Fatal("TLS configured from empty values")
	}
}

func TestSettingsNKeyAcceptsInlineSeedOrFile(t *testing.T) {
	seed := newSeed(t)
	path := filepath.Join(t.TempDir(), "user.creds")
	creds := "-----BEGIN NATS USER JWT-----\neyJ.fake.jwt\n------END NATS USER JWT------\n\n-----BEGIN USER NKEY SEED-----\n" + seed + "\n------END USER NKEY SEED------\n"
	if err := os.WriteFile(path, []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{"inline": seed, "path": path, "file url": "file://" + path} {
		t.Run(name, func(t *testing.T) {
			parsed, ok := ParseNKeySeed(raw)
			if !ok {
				t.Fatal("not parsed")
			}
			got, err := parsed.resolve()
			if err != nil {
				t.Fatal(err)
			}
			if got != seed {
				t.Fatal("resolved seed differs")
			}
		})
	}
}

func TestNKeySeedFileErrorsDoNotEchoThePath(t *testing.T) {
	const mistyped = "SUAMISTYPEDINLINESEEDTHATISNOTVALID"
	parsed, _ := ParseNKeySeed(mistyped)
	_, err := parsed.resolve()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), mistyped) {
		t.Fatalf("error echoes the value: %v", err)
	}
}

func TestCredentialValueReferences(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := mapEnv(map[string]string{"TOKEN_VAR": "from-env"})
	tests := []struct {
		raw     string
		want    string
		wantErr error
	}{
		{raw: "literal", want: "literal"},
		{raw: "file://" + path, want: "from-file"},
		{raw: "env://TOKEN_VAR", want: "from-env"},
		{raw: "op://vault/item", wantErr: ErrUnsupportedScheme},
		{raw: "nsc://op/acct/user", wantErr: ErrUnsupportedScheme},
		{raw: "data:abc", wantErr: ErrUnsupportedScheme},
	}
	for _, tt := range tests {
		value, ok := ParseCredentialValue(tt.raw)
		if !ok {
			t.Fatalf("%q not parsed", tt.raw)
		}
		got, err := value.resolve(env)
		if tt.wantErr != nil {
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("%q: err = %v, want %v", tt.raw, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Fatalf("%q: got %q, %v; want %q", tt.raw, got, err, tt.want)
		}
	}
	if _, err := mustCredential(t, "env://UNSET_VAR").resolve(env); err == nil {
		t.Fatal("unset environment reference resolved")
	}
	if _, ok := ParseCredentialValue(""); ok {
		t.Fatal("empty value parsed")
	}
}

func mustCredential(t *testing.T, raw string) CredentialValue {
	t.Helper()
	value, ok := ParseCredentialValue(raw)
	if !ok {
		t.Fatalf("%q not parsed", raw)
	}
	return value
}

func TestSecretsAreRedactedEverywhere(t *testing.T) {
	const (
		password = "hunter2-password-value"
		token    = "tok-secret-value"
		jwt      = "jwt-secret-value"
	)
	seed := newSeed(t)
	settings := SettingsFromEnv(mapEnv(map[string]string{
		EnvURL:      "nats://svc:urlpass@host:4222",
		EnvUser:     "alice",
		EnvPassword: password,
		EnvToken:    token,
		EnvJWT:      jwt,
		EnvNKey:     seed,
		EnvSeed:     seed,
	}))
	cfg := settings.Resolve()
	auths := []Auth{
		cfg.Auth,
		UserPasswordAuth{User: "alice", Password: mustCredential(t, password)},
		TokenAuth{Token: mustCredential(t, token)},
		JWTAuth{JWT: mustCredential(t, jwt), Seed: ptr(mustCredential(t, seed))},
		NKeyAuth{Seed: func() NKeySeed { s, _ := ParseNKeySeed(seed); return s }()},
	}
	secrets := []string{password, token, jwt, seed}
	for _, subject := range []any{settings, cfg, auths} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			out := fmt.Sprintf(format, subject)
			for _, secret := range secrets {
				if strings.Contains(out, secret) {
					t.Fatalf("%s of %T leaks a secret", format, subject)
				}
			}
		}
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("JSON of settings leaks a secret")
		}
	}
}

func ptr[T any](v T) *T { return &v }

func TestRedactServer(t *testing.T) {
	tests := map[string]string{
		"nats://host:4222":             "nats://host:4222",
		"host:4222":                    "host:4222",
		"nats://user:pass@host:4222":   "nats://[redacted]@host:4222",
		"nats://tok@host:4222":         "nats://[redacted]@host:4222",
		"user:pass@host:4222":          "[redacted]@host:4222",
		"tls://u:p@host:4222/path?x=y": "tls://[redacted]@host:4222/path?x=y",
	}
	for in, want := range tests {
		if got := redactServer(in); got != want {
			t.Errorf("redactServer(%q) = %q, want %q", in, got, want)
		}
	}
}
