package cliproxy

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

// A hot reload that lists one client key twice must be refused, so the running
// configuration keeps its unambiguous key bindings.
func TestConfigUpdateRejectsDuplicateAPIKeys(t *testing.T) {
	oldCfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: "k", Auths: []string{"a.json"}}}}}
	service := &Service{cfg: oldCfg, coreManager: coreauth.NewManager(nil, nil, nil)}

	dup := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []config.APIKeyEntry{{Key: "k"}, {Key: "k", Auths: []string{"a.json"}}}}}
	if commit := service.commitConfigUpdate(dup); commit.cfg != nil {
		t.Fatal("a config with a duplicated api key was committed")
	}
	if service.cfg != oldCfg {
		t.Fatal("the running config changed after a rejected update")
	}
}
