package schema

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestStoreCachesToDisk(t *testing.T) {
	t.Setenv(EnvCacheDir, t.TempDir())
	c := fakeInstance(t)

	s1, err := NewStore(c)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	m1, err := s1.Get(context.Background(), "incident")
	if err != nil {
		t.Fatalf("first get: %v", err)
	}

	// A brand-new store against a DEAD client must serve from disk.
	s2, err := NewStore(c)
	if err != nil {
		t.Fatalf("new store 2: %v", err)
	}
	s2.Client = nil // any network use would panic
	m2 := s2.GetCached("incident")
	if m2 == nil {
		t.Fatal("expected fresh cache hit")
	}
	if m2.DisplayField != m1.DisplayField || len(m2.Fields) != len(m1.Fields) {
		t.Errorf("cache round-trip mismatch: %+v vs %+v", m2, m1)
	}
	if _, err := s2.Get(context.Background(), "incident"); err != nil {
		t.Fatalf("cached Get should not touch the client: %v", err)
	}
}

func TestStoreTTLExpiry(t *testing.T) {
	t.Setenv(EnvCacheDir, t.TempDir())
	c := fakeInstance(t)
	s, _ := NewStore(c)
	if _, err := s.Get(context.Background(), "incident"); err != nil {
		t.Fatalf("get: %v", err)
	}

	// Age the entry past the TTL.
	path := s.path("incident")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var entry cacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("decode cache: %v", err)
	}
	entry.FetchedAt = time.Now().Add(-8 * 24 * time.Hour)
	aged, _ := json.Marshal(entry)
	if err := os.WriteFile(path, aged, 0o600); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	if s.GetCached("incident") != nil {
		t.Error("stale entry must not be served")
	}
	if _, err := s.Get(context.Background(), "incident"); err != nil {
		t.Fatalf("refetch after expiry: %v", err)
	}
	if s.GetCached("incident") == nil {
		t.Error("refetch should have re-cached")
	}
}

func TestStoreRefreshBypassesCache(t *testing.T) {
	t.Setenv(EnvCacheDir, t.TempDir())
	c := fakeInstance(t)
	s, _ := NewStore(c)
	if _, err := s.Get(context.Background(), "incident"); err != nil {
		t.Fatalf("get: %v", err)
	}

	s.Refresh = true
	if s.GetCached("incident") != nil {
		t.Error("Refresh must bypass cache reads")
	}
	if _, err := s.Get(context.Background(), "incident"); err != nil {
		t.Fatalf("refresh get: %v", err)
	}
}

func TestStoreWithoutDirDegradesToLive(t *testing.T) {
	c := fakeInstance(t)
	s := &Store{Client: c}
	if s.GetCached("incident") != nil {
		t.Error("no dir → no cache")
	}
	if _, err := s.Get(context.Background(), "incident"); err != nil {
		t.Fatalf("live get: %v", err)
	}
}
