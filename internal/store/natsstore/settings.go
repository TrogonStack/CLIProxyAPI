package natsstore

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/nats-io/nkeys"
)

// Connection environment variables, named and read the way the nats CLI does.
const (
	EnvURL      = "NATS_URL"
	EnvCreds    = "NATS_CREDS"
	EnvNKey     = "NATS_NKEY"
	EnvUser     = "NATS_USER"
	EnvPassword = "NATS_PASSWORD"
	EnvToken    = "NATS_TOKEN"
	EnvJWT      = "NATS_JWT"
	EnvSeed     = "NATS_SEED"
	EnvCert     = "NATS_CERT"
	EnvKey      = "NATS_KEY"
	EnvCA       = "NATS_CA"

	// DefaultURL is used when NATS_URL is unset.
	DefaultURL = "localhost:4222"
)

// seedPrefixes are the prefixes of operator, account and user NKey seeds.
var seedPrefixes = []string{"SO", "SA", "SU"}

// Lookup reads one environment variable, reporting whether it is set.
type Lookup func(key string) (string, bool)

// OSLookup reads the process environment.
func OSLookup(key string) (string, bool) { return os.LookupEnv(key) }

// SettingError names the setting a problem belongs to without ever carrying a secret value.
type SettingError struct {
	Setting string
	Err     error
}

func (e *SettingError) Error() string { return fmt.Sprintf("invalid %s: %v", e.Setting, e.Err) }

func (e *SettingError) Unwrap() error { return e.Err }

// ErrUnsupportedScheme marks a reference scheme this package does not resolve.
var ErrUnsupportedScheme = errors.New("unsupported scheme")

func unsupportedScheme(scheme string) error {
	return fmt.Errorf("%w `%s`", ErrUnsupportedScheme, scheme)
}

// ioReason drops the path from a filesystem error, keeping only the cause.
func ioReason(err error) error {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err
	}
	return err
}

// splitScheme splits `scheme:rest`, dropping a leading `//` from rest. It returns
// false when the prefix is not a URI scheme, so `C:\x` and bare values stay literal.
func splitScheme(raw string) (scheme, rest string, ok bool) {
	end := strings.IndexByte(raw, ':')
	if end < 0 {
		return "", "", false
	}
	scheme = raw[:end]
	if len(scheme) < 2 || !isAlpha(scheme[0]) {
		return "", "", false
	}
	for i := 1; i < len(scheme); i++ {
		c := scheme[i]
		if !isAlpha(c) && !(c >= '0' && c <= '9') && c != '+' && c != '-' && c != '.' {
			return "", "", false
		}
	}
	rest = raw[end+1:]
	return scheme, strings.TrimPrefix(rest, "//"), true
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

// Location is a filesystem location written as a bare path or as file://path.
type Location struct {
	path   string
	scheme string
}

// ParseLocation parses raw, reporting false for an empty value.
func ParseLocation(raw string) (Location, bool) {
	if raw == "" {
		return Location{}, false
	}
	scheme, rest, ok := splitScheme(raw)
	switch {
	case !ok:
		return Location{path: raw}, true
	case strings.EqualFold(scheme, "file"):
		return Location{path: rest}, true
	default:
		return Location{scheme: strings.ToLower(scheme)}, true
	}
}

// Path returns the filesystem path, or an error when the scheme is not file.
func (l Location) Path() (string, error) {
	if l.scheme != "" {
		return "", unsupportedScheme(l.scheme)
	}
	return l.path, nil
}

// readSeed reads an NKey seed from a plain or decorated seed file (including a
// .creds file), taking the first line with a seed prefix. Errors never echo the
// path, because a mistyped inline seed is read as a path.
func (l Location) readSeed() (string, error) {
	path, err := l.Path()
	if err != nil {
		return "", err
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the NKey seed file: %w", ioReason(err))
	}
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSpace(line)
		if !hasSeedPrefix(line) {
			continue
		}
		if _, err := nkeys.FromSeed([]byte(line)); err != nil {
			return "", fmt.Errorf("invalid NKey seed: %w", err)
		}
		return line, nil
	}
	return "", errors.New("no NKey seed found in the NKey seed file")
}

func hasSeedPrefix(line string) bool {
	for _, prefix := range seedPrefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// NKeySeed is an NKey seed given inline or as a seed file location. A value that
// parses as a seed is used inline; anything else is a bare or file:// path, which
// is how the nats CLI reads NATS_NKEY.
type NKeySeed struct {
	inline Secret
	file   Location
	isFile bool
}

// ParseNKeySeed parses raw, reporting false for an empty value.
func ParseNKeySeed(raw string) (NKeySeed, bool) {
	trimmed := strings.TrimSpace(raw)
	if _, err := nkeys.FromSeed([]byte(trimmed)); err == nil {
		inline, ok := NewSecret(trimmed)
		return NKeySeed{inline: inline}, ok
	}
	location, ok := ParseLocation(trimmed)
	return NKeySeed{file: location, isFile: true}, ok
}

func (s NKeySeed) resolve() (string, error) {
	if s.isFile {
		return s.file.readSeed()
	}
	return s.inline.Reveal(), nil
}

func (s NKeySeed) String() string { return "NKeySeed(" + redacted + ")" }

func (s NKeySeed) GoString() string { return s.String() }

func (s NKeySeed) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

type credentialKind int

const (
	credentialLiteral credentialKind = iota
	credentialFile
	credentialEnv
	credentialUnsupported
)

// CredentialValue is a configured value that is a literal or a reference to where
// it lives (file://path, env://NAME).
type CredentialValue struct {
	literal Secret
	file    string
	env     string
	scheme  string
	kind    credentialKind
}

// ParseCredentialValue parses raw, reporting false for an empty value.
func ParseCredentialValue(raw string) (CredentialValue, bool) {
	if scheme, rest, ok := splitScheme(raw); ok {
		switch {
		case strings.EqualFold(scheme, "file"):
			return CredentialValue{kind: credentialFile, file: rest}, true
		case strings.EqualFold(scheme, "env"):
			return CredentialValue{kind: credentialEnv, env: rest}, true
		case strings.EqualFold(scheme, "op"), strings.EqualFold(scheme, "nsc"), strings.EqualFold(scheme, "data"):
			return CredentialValue{kind: credentialUnsupported, scheme: strings.ToLower(scheme)}, true
		}
	}
	literal, ok := NewSecret(raw)
	return CredentialValue{literal: literal}, ok
}

func (v CredentialValue) resolve(env Lookup) (string, error) {
	switch v.kind {
	case credentialFile:
		contents, err := os.ReadFile(v.file)
		if err != nil {
			return "", fmt.Errorf("cannot read %s: %w", v.file, ioReason(err))
		}
		return strings.TrimRight(string(contents), "\r\n"), nil
	case credentialEnv:
		value, ok := env(v.env)
		if !ok {
			return "", fmt.Errorf("environment variable `%s` is not set", v.env)
		}
		return value, nil
	case credentialUnsupported:
		return "", unsupportedScheme(v.scheme)
	default:
		return v.literal.Reveal(), nil
	}
}

func (v CredentialValue) String() string {
	switch v.kind {
	case credentialFile:
		return "file://" + v.file
	case credentialEnv:
		return "env://" + v.env
	case credentialUnsupported:
		return v.scheme + "://"
	default:
		return redacted
	}
}

func (v CredentialValue) GoString() string { return v.String() }

func (v CredentialValue) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(v.String())) }

// Auth is the authentication method, chosen exclusively by Settings.Resolve.
type Auth interface {
	// Description names the method for logs, never carrying a value.
	Description() string
	isAuth()
}

type (
	// CredentialsAuth authenticates with a user credentials (.creds) file.
	CredentialsAuth struct{ File Location }
	// NKeyAuth authenticates with an NKey seed.
	NKeyAuth struct{ Seed NKeySeed }
	// UserPasswordAuth authenticates with a user and password.
	UserPasswordAuth struct {
		User     string
		Password CredentialValue
	}
	// JWTAuth authenticates with a user JWT, signing the nonce with Seed when present.
	JWTAuth struct {
		JWT  CredentialValue
		Seed *CredentialValue
	}
	// TokenAuth authenticates with a token.
	TokenAuth struct{ Token CredentialValue }
	// NoAuth connects without credentials.
	NoAuth struct{}
)

func (CredentialsAuth) Description() string  { return "credentials file" }
func (NKeyAuth) Description() string         { return "NKey" }
func (UserPasswordAuth) Description() string { return "user/password" }
func (a JWTAuth) Description() string {
	if a.Seed == nil {
		return "JWT without signature"
	}
	return "JWT"
}
func (TokenAuth) Description() string { return "token" }
func (NoAuth) Description() string    { return "none" }

func (CredentialsAuth) isAuth()  {}
func (NKeyAuth) isAuth()         {}
func (UserPasswordAuth) isAuth() {}
func (JWTAuth) isAuth()          {}
func (TokenAuth) isAuth()        {}
func (NoAuth) isAuth()           {}

// TLS holds client TLS material. A client certificate needs both its certificate
// and key, and setting either alone is reported instead of ignored.
type TLS struct {
	Cert *Location
	Key  *Location
	CA   *Location
}

// Configured reports whether any TLS setting is present.
func (t TLS) Configured() bool { return t.Cert != nil || t.Key != nil || t.CA != nil }

// clientCertificate returns the cert and key pair, nil when neither is set.
func (t TLS) clientCertificate() (*[2]Location, error) {
	switch {
	case t.Cert != nil && t.Key != nil:
		return &[2]Location{*t.Cert, *t.Key}, nil
	case t.Cert != nil:
		return nil, &SettingError{Setting: EnvCert, Err: fmt.Errorf("must be set together with %s", EnvKey)}
	case t.Key != nil:
		return nil, &SettingError{Setting: EnvKey, Err: fmt.Errorf("must be set together with %s", EnvCert)}
	}
	return nil, nil
}

// Config is the resolved connection configuration.
type Config struct {
	Servers []string
	Auth    Auth
	TLS     TLS
}

// Settings holds the raw, unparsed connection inputs. Resolve is the single place
// that decides which authentication method wins, with this precedence:
//  1. Creds
//  2. NKey
//  3. User + Password
//  4. Token, or User when it is set without a Password
//  5. JWT (signed with Seed when present)
//
// Empty values count as unset.
type Settings struct {
	URL      string
	User     string
	Password Secret
	Token    Secret
	Creds    string
	NKey     Secret
	JWT      Secret
	Seed     Secret
	Cert     string
	Key      string
	CA       string
}

// SettingsFromEnv reads the connection settings from the environment variables the nats CLI reads.
func SettingsFromEnv(env Lookup) Settings {
	read := func(key string) string {
		value, _ := env(key)
		return value
	}
	secret := func(key string) Secret {
		value, _ := NewSecret(read(key))
		return value
	}
	return Settings{
		URL:      read(EnvURL),
		User:     read(EnvUser),
		Password: secret(EnvPassword),
		Token:    secret(EnvToken),
		Creds:    read(EnvCreds),
		NKey:     secret(EnvNKey),
		JWT:      secret(EnvJWT),
		Seed:     secret(EnvSeed),
		Cert:     read(EnvCert),
		Key:      read(EnvKey),
		CA:       read(EnvCA),
	}
}

// Resolve applies the precedence and returns the connection configuration.
func (s Settings) Resolve() Config {
	return Config{Servers: s.servers(), Auth: s.auth(), TLS: s.tls()}
}

func (s Settings) auth() Auth {
	if location, ok := ParseLocation(s.Creds); ok {
		return CredentialsAuth{File: location}
	}
	if seed, ok := ParseNKeySeed(s.NKey.Reveal()); ok {
		return NKeyAuth{Seed: seed}
	}
	if s.User != "" && s.Password.Reveal() != "" {
		if password, ok := ParseCredentialValue(s.Password.Reveal()); ok {
			return UserPasswordAuth{User: s.User, Password: password}
		}
	}
	token := s.Token.Reveal()
	if token == "" && s.Password.Reveal() == "" {
		token = s.User
	}
	if value, ok := ParseCredentialValue(token); ok {
		return TokenAuth{Token: value}
	}
	if jwt, ok := ParseCredentialValue(s.JWT.Reveal()); ok {
		auth := JWTAuth{JWT: jwt}
		if seed, ok := ParseCredentialValue(s.Seed.Reveal()); ok {
			auth.Seed = &seed
		}
		return auth
	}
	return NoAuth{}
}

func (s Settings) tls() TLS {
	location := func(raw string) *Location {
		if l, ok := ParseLocation(raw); ok {
			return &l
		}
		return nil
	}
	return TLS{Cert: location(s.Cert), Key: location(s.Key), CA: location(s.CA)}
}

func (s Settings) servers() []string {
	url := s.URL
	if url == "" {
		url = DefaultURL
	}
	var servers []string
	for _, server := range strings.Split(url, ",") {
		if server = strings.TrimSpace(server); server != "" {
			servers = append(servers, server)
		}
	}
	return servers
}
