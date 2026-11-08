package natsstore

import (
	"strings"
	"testing"
)

func TestEncodeKeyRoundTrips(t *testing.T) {
	ids := []string{
		"a.json",
		"sub/dir/foo@bar.com.json",
		"codex-user@example.com-plus.json",
		"nested/../odd name.json",
		"ünïcödé/日本.json",
		".hidden",
		"trailing.",
		"x",
	}
	seen := map[string]string{}
	for _, id := range ids {
		key, err := EncodeKey(id)
		if err != nil {
			t.Fatalf("EncodeKey(%q): %v", id, err)
		}
		if strings.ContainsAny(key, ".*> =/") {
			t.Fatalf("key %q for %q is not one safe token", key, id)
		}
		if other, dup := seen[key]; dup {
			t.Fatalf("%q and %q collide on %q", id, other, key)
		}
		seen[key] = id
		back, err := DecodeKey(key)
		if err != nil || back != id {
			t.Fatalf("DecodeKey(%q) = %q, %v; want %q", key, back, err, id)
		}
	}
}

func TestEncodeKeyRejectsEmptyAndOversized(t *testing.T) {
	if _, err := EncodeKey(""); err == nil {
		t.Fatal("empty id accepted")
	}
	if _, err := EncodeKey(strings.Repeat("a", 400)); err == nil {
		t.Fatal("oversized id accepted")
	}
	if _, err := EncodeKey(strings.Repeat("a", 380)); err != nil {
		t.Fatalf("id at the limit rejected: %v", err)
	}
}

func TestDecodeKeyRejectsForeignKeys(t *testing.T) {
	for _, key := range []string{"", "not base64!", "a.b", "config/main"} {
		if _, err := DecodeKey(key); err == nil {
			t.Fatalf("DecodeKey(%q) accepted a foreign key", key)
		}
	}
}

func FuzzEncodeKey(f *testing.F) {
	for _, seed := range []string{"a", "x/y@z.json", "\xff\xfe", "日本"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, id string) {
		key, err := EncodeKey(id)
		if err != nil {
			return
		}
		back, err := DecodeKey(key)
		if err != nil || back != id {
			t.Fatalf("round trip of %q gave %q, %v", id, back, err)
		}
	})
}
