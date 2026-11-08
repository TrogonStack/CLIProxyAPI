package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// RefreshPolicy says how a provider's refresh grants must be guarded against two
// replicas sending the same grant.
type RefreshPolicy int

const (
	// RefreshSingleUse is for grants that rotate or whose behavior is unknown: the
	// grant is claimed in the journal before it is sent and the claim never
	// expires. It is the zero value, so a provider that declares nothing fails safe.
	RefreshSingleUse RefreshPolicy = iota
	// RefreshNone is for grants that stay valid after use, so sending one twice is harmless.
	RefreshNone
)

// RefreshPolicyProvider is implemented by executors whose grants are not single-use.
type RefreshPolicyProvider interface {
	RefreshPolicy() RefreshPolicy
}

// RefreshPolicyOf returns the policy the executor declares, RefreshSingleUse when it declares none.
func RefreshPolicyOf(exec ProviderExecutor) RefreshPolicy {
	if declared, ok := exec.(RefreshPolicyProvider); ok {
		return declared.RefreshPolicy()
	}
	return RefreshSingleUse
}

// SharedRefreshJournal is implemented by a store whose refresh journal is shared
// by every replica using it. Such a journal guards every single-use provider.
// A store without it keeps guarding only Codex, the one provider that had a
// journal before the stores shared one.
type SharedRefreshJournal interface {
	codex.RefreshJournal
	SharesRefreshJournal() bool
}

// refreshJournalFor picks the journal bound to a refresh of auth, nil for none.
func refreshJournalFor(store Store, auth *Auth, exec ProviderExecutor) (codex.RefreshJournal, error) {
	if RefreshPolicyOf(exec) == RefreshNone {
		return nil, nil
	}
	if shared, ok := store.(SharedRefreshJournal); ok && shared.SharesRefreshJournal() {
		return shared, nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil, nil
	}
	if resolver, ok := store.(interface {
		RefreshJournalFor(*Auth) (codex.RefreshJournal, error)
	}); ok {
		return resolver.RefreshJournalFor(auth)
	}
	if journal, ok := store.(codex.RefreshJournal); ok {
		return journal, nil
	}
	return nil, nil
}
