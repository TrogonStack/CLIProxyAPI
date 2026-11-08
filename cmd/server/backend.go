package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/store"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/store/natsstore"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// backendEnv carries what a backend needs to read its settings and place its spool.
type backendEnv struct {
	// lookup returns the first non-empty value among the given environment keys.
	lookup func(keys ...string) (string, bool)
	// workDir is the process working directory.
	workDir string
	// writableBase is the preferred base for local spools, empty when none is known.
	writableBase string
}

// spoolBase returns the configured local path, falling back to the writable base and then the working directory.
func (e backendEnv) spoolBase(configured string) string {
	if configured != "" {
		return configured
	}
	if e.writableBase != "" {
		return e.writableBase
	}
	return e.workDir
}

// openedBackend is a started remote-backed store together with the paths the server runs from.
type openedBackend struct {
	tokenStore coreauth.Store
	configPath string
	// authDir overrides the configured auth directory once the config is loaded.
	authDir string
	// ready logs that the backend is serving, called only after the config loaded.
	ready func()
}

// openBackendFunc starts a remote-backed store.
type openBackendFunc func() (*openedBackend, error)

// storeBackend describes one remote token store. detect returns nil when the
// environment does not select the backend, otherwise a function that starts it.
type storeBackend struct {
	name   string
	detect func(env backendEnv) openBackendFunc
}

// storeBackends lists the backends in selection priority; the first one whose
// settings are present wins. Adding a backend means adding one entry.
var storeBackends = []storeBackend{
	{name: "postgres", detect: detectPostgresBackend},
	{name: "object", detect: detectObjectBackend},
	{name: "git", detect: detectGitBackend},
	{name: "nats", detect: detectNatsBackend},
}

// selectBackend returns the opener of the first backend selected by the
// environment, or nil. Later backends that are also configured are ignored
// with a warning, since a stray variable is easy to leave behind.
func selectBackend(env backendEnv) openBackendFunc {
	var (
		selected openBackendFunc
		chosen   string
	)
	for _, backend := range storeBackends {
		open := backend.detect(env)
		if open == nil {
			continue
		}
		if selected == nil {
			selected, chosen = open, backend.name
			continue
		}
		log.Warnf("%s store settings are present but ignored because the %s store is selected", backend.name, chosen)
	}
	return selected
}

func detectPostgresBackend(env backendEnv) openBackendFunc {
	dsn, ok := env.lookup("PGSTORE_DSN", "pgstore_dsn")
	if !ok {
		return nil
	}
	schema, _ := env.lookup("PGSTORE_SCHEMA", "pgstore_schema")
	localPath, _ := env.lookup("PGSTORE_LOCAL_PATH", "pgstore_local_path")
	spool := filepath.Join(env.spoolBase(localPath), "pgstore")
	return func() (*openedBackend, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		inst, err := store.NewPostgresStore(ctx, store.PostgresStoreConfig{
			DSN:      dsn,
			Schema:   schema,
			SpoolDir: spool,
		})
		cancel()
		if err != nil {
			return nil, fmt.Errorf("failed to initialize postgres token store: %w", err)
		}
		examplePath := filepath.Join(env.workDir, "config.example.yaml")
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if errBootstrap := inst.Bootstrap(ctx, examplePath); errBootstrap != nil {
			return nil, fmt.Errorf("failed to bootstrap postgres-backed config: %w", errBootstrap)
		}
		return &openedBackend{
			tokenStore: inst,
			configPath: inst.ConfigPath(),
			authDir:    inst.AuthDir(),
			ready: func() {
				log.Infof("postgres-backed token store enabled, workspace path: %s", inst.WorkDir())
			},
		}, nil
	}
}

func detectObjectBackend(env backendEnv) openBackendFunc {
	endpoint, ok := env.lookup("OBJECTSTORE_ENDPOINT", "objectstore_endpoint")
	if !ok {
		return nil
	}
	access, _ := env.lookup("OBJECTSTORE_ACCESS_KEY", "objectstore_access_key")
	secret, _ := env.lookup("OBJECTSTORE_SECRET_KEY", "objectstore_secret_key")
	bucket, _ := env.lookup("OBJECTSTORE_BUCKET", "objectstore_bucket")
	localPath, _ := env.lookup("OBJECTSTORE_LOCAL_PATH", "objectstore_local_path")
	return func() (*openedBackend, error) {
		root := filepath.Join(env.spoolBase(localPath), "objectstore")
		resolvedEndpoint := strings.TrimSpace(endpoint)
		useSSL := true
		if strings.Contains(resolvedEndpoint, "://") {
			parsed, errParse := url.Parse(resolvedEndpoint)
			if errParse != nil {
				return nil, fmt.Errorf("failed to parse object store endpoint %q: %w", endpoint, errParse)
			}
			switch strings.ToLower(parsed.Scheme) {
			case "http":
				useSSL = false
			case "https":
				useSSL = true
			default:
				return nil, fmt.Errorf("unsupported object store scheme %q (only http and https are allowed)", parsed.Scheme)
			}
			if parsed.Host == "" {
				return nil, fmt.Errorf("object store endpoint %q is missing host information", endpoint)
			}
			resolvedEndpoint = parsed.Host
			if parsed.Path != "" && parsed.Path != "/" {
				resolvedEndpoint = strings.TrimSuffix(parsed.Host+parsed.Path, "/")
			}
		}
		resolvedEndpoint = strings.TrimRight(resolvedEndpoint, "/")
		inst, err := store.NewObjectTokenStore(store.ObjectStoreConfig{
			Endpoint:  resolvedEndpoint,
			Bucket:    bucket,
			AccessKey: access,
			SecretKey: secret,
			LocalRoot: root,
			UseSSL:    useSSL,
			PathStyle: true,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to initialize object token store: %w", err)
		}
		examplePath := filepath.Join(env.workDir, "config.example.yaml")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if errBootstrap := inst.Bootstrap(ctx, examplePath); errBootstrap != nil {
			return nil, fmt.Errorf("failed to bootstrap object-backed config: %w", errBootstrap)
		}
		return &openedBackend{
			tokenStore: inst,
			configPath: inst.ConfigPath(),
			authDir:    inst.AuthDir(),
			ready: func() {
				log.Infof("object-backed token store enabled, bucket: %s", bucket)
			},
		}, nil
	}
}

func detectGitBackend(env backendEnv) openBackendFunc {
	remoteURL, ok := env.lookup("GITSTORE_GIT_URL", "gitstore_git_url")
	if !ok {
		return nil
	}
	user, _ := env.lookup("GITSTORE_GIT_USERNAME", "gitstore_git_username")
	password, _ := env.lookup("GITSTORE_GIT_TOKEN", "gitstore_git_token")
	localPath, _ := env.lookup("GITSTORE_LOCAL_PATH", "gitstore_local_path")
	branch, _ := env.lookup("GITSTORE_GIT_BRANCH", "gitstore_git_branch")
	return func() (*openedBackend, error) {
		root := filepath.Join(env.spoolBase(localPath), "gitstore")
		inst := store.NewGitTokenStore(remoteURL, user, password, branch)
		inst.SetBaseDir(filepath.Join(root, "auths"))
		if errRepo := inst.EnsureRepository(); errRepo != nil {
			return nil, fmt.Errorf("failed to prepare git token store: %w", errRepo)
		}
		configPath := inst.ConfigPath()
		if configPath == "" {
			configPath = filepath.Join(root, "config", "config.yaml")
		}
		if _, statErr := os.Stat(configPath); errors.Is(statErr, fs.ErrNotExist) {
			examplePath := filepath.Join(env.workDir, "config.example.yaml")
			if _, errExample := os.Stat(examplePath); errExample != nil {
				return nil, fmt.Errorf("failed to find template config file: %w", errExample)
			}
			if errCopy := misc.CopyConfigTemplate(examplePath, configPath); errCopy != nil {
				return nil, fmt.Errorf("failed to bootstrap git-backed config: %w", errCopy)
			}
			if errCommit := persistSeededConfig(context.Background(), configPath, inst.PersistConfig); errCommit != nil {
				return nil, fmt.Errorf("failed to commit initial git-backed config: %w", errCommit)
			}
			log.Infof("git-backed config initialized from template: %s", configPath)
		} else if statErr != nil {
			return nil, fmt.Errorf("failed to inspect git-backed config: %w", statErr)
		}
		return &openedBackend{
			tokenStore: inst,
			configPath: configPath,
			authDir:    inst.AuthDir(),
			ready: func() {
				log.Infof("git-backed token store enabled, repository path: %s", root)
			},
		}, nil
	}
}

func detectNatsBackend(env backendEnv) openBackendFunc {
	if _, ok := env.lookup(natsstore.EnvURL); !ok {
		return nil
	}
	lookup := func(key string) (string, bool) { return env.lookup(key) }
	localPath, _ := env.lookup("NATS_STORE_LOCAL_PATH", "nats_store_local_path")
	spool := filepath.Join(env.spoolBase(localPath), "natsstore")
	return func() (*openedBackend, error) {
		settings, err := natsstore.StoreSettingsFromEnv(lookup)
		if err != nil {
			return nil, fmt.Errorf("failed to read nats store settings: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		nc, err := natsstore.Connect(ctx, natsstore.SettingsFromEnv(lookup).Resolve(), lookup, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to connect nats token store: %w", err)
		}
		boot, err := natsstore.Bootstrap(ctx, nc, settings)
		if err != nil {
			nc.Close()
			return nil, fmt.Errorf("failed to bootstrap nats store buckets: %w", err)
		}
		inst, err := natsstore.NewStore(nc, boot, settings, spool)
		if err != nil {
			nc.Close()
			return nil, fmt.Errorf("failed to initialize nats token store: %w", err)
		}
		examplePath := filepath.Join(env.workDir, "config.example.yaml")
		if errBootstrap := inst.Bootstrap(ctx, examplePath); errBootstrap != nil {
			_ = inst.Close()
			return nil, fmt.Errorf("failed to bootstrap nats-backed config: %w", errBootstrap)
		}
		return &openedBackend{
			tokenStore: inst,
			configPath: inst.ConfigPath(),
			authDir:    inst.AuthDir(),
			ready: func() {
				log.Infof("nats-backed token store enabled, bucket prefix: %s, workspace path: %s", settings.Prefix, inst.WorkDir())
			},
		}, nil
	}
}

// loadBackendConfig loads the config a backend placed on disk and points it at the backend's auth directory.
func loadBackendConfig(opened *openedBackend, isCloudDeploy bool) (*config.Config, error) {
	cfg, err := config.LoadConfigOptional(opened.configPath, isCloudDeploy)
	if err != nil {
		return cfg, err
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.AuthDir = opened.authDir
	opened.ready()
	return cfg, nil
}
