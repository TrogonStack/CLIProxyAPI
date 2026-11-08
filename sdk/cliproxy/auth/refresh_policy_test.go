package auth

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
)

// policyExecutor is only inspected for its policy, so no method is ever called.
type policyExecutor struct {
	ProviderExecutor
}

type noneExecutor struct{ policyExecutor }

func (noneExecutor) RefreshPolicy() RefreshPolicy { return RefreshNone }

// plainStore is a store with no journal at all.
type plainStore struct{ Store }

// codexJournalStore is a store that only ever journaled Codex.
type codexJournalStore struct {
	plainStore
	codex.RefreshJournal
}

// sharedJournalStore is a store whose journal every replica shares.
type sharedJournalStore struct {
	plainStore
	codex.RefreshJournal
}

func (sharedJournalStore) SharesRefreshJournal() bool { return true }

func TestRefreshJournalForFollowsPolicyAndStore(t *testing.T) {
	journal := codex.NewMemoryRefreshJournal()
	cases := []struct {
		name     string
		store    Store
		provider string
		exec     ProviderExecutor
		want     bool
	}{
		{"shared codex", sharedJournalStore{RefreshJournal: journal}, "codex", policyExecutor{}, true},
		{"shared claude", sharedJournalStore{RefreshJournal: journal}, "claude", policyExecutor{}, true},
		{"shared kimi", sharedJournalStore{RefreshJournal: journal}, "kimi", policyExecutor{}, true},
		{"shared xai", sharedJournalStore{RefreshJournal: journal}, "xai", policyExecutor{}, true},
		{"shared unknown provider fails safe", sharedJournalStore{RefreshJournal: journal}, "newcomer", nil, true},
		{"shared reusable grant", sharedJournalStore{RefreshJournal: journal}, "antigravity", noneExecutor{}, false},
		{"shared reusable codex", sharedJournalStore{RefreshJournal: journal}, "codex", noneExecutor{}, false},
		{"unshared codex", codexJournalStore{RefreshJournal: journal}, "codex", policyExecutor{}, true},
		{"unshared claude keeps no journal", codexJournalStore{RefreshJournal: journal}, "claude", policyExecutor{}, false},
		{"no journal in the store", plainStore{}, "codex", policyExecutor{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := refreshJournalFor(tc.store, &Auth{Provider: tc.provider}, tc.exec)
			if err != nil {
				t.Fatalf("refreshJournalFor() error = %v", err)
			}
			if (got != nil) != tc.want {
				t.Fatalf("journal bound = %v, want %v", got != nil, tc.want)
			}
		})
	}
}

func TestRefreshPolicyOfDefaultsToSingleUse(t *testing.T) {
	if got := RefreshPolicyOf(policyExecutor{}); got != RefreshSingleUse {
		t.Fatalf("policy = %v, want single use", got)
	}
	if got := RefreshPolicyOf(noneExecutor{}); got != RefreshNone {
		t.Fatalf("policy = %v, want none", got)
	}
}
