package schema

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tcurtsinger/GlideMind/internal/snow"
)

// DefaultTTL is how long cached table metadata stays fresh. Dictionaries
// change when apps are developed, not minute to minute; --refresh busts.
const DefaultTTL = 7 * 24 * time.Hour

// EnvCacheDir overrides the cache root (containers, tests). Default is the
// OS user cache dir.
const EnvCacheDir = "GLM_CACHE_DIR"

// Store reads table metadata through a per-instance disk cache. The zero
// value (or Dir == "") degrades to live lookups only.
type Store struct {
	Client  *snow.Client
	Dir     string
	TTL     time.Duration
	Refresh bool // bypass cache reads (fresh fetch still writes)
}

// NewStore builds a store whose cache lives under
// <user-cache-dir>/glidemind/schema/<instance-host>/.
func NewStore(client *snow.Client) (*Store, error) {
	base := os.Getenv(EnvCacheDir)
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return nil, err
		}
	}
	host := "instance"
	if u, err := url.Parse(client.BaseURL()); err == nil && u.Host != "" {
		host = strings.ReplaceAll(u.Host, ":", "_")
	}
	return &Store{
		Client: client,
		Dir:    filepath.Join(base, "glidemind", "schema", host),
		TTL:    DefaultTTL,
	}, nil
}

type cacheEntry struct {
	FetchedAt time.Time  `json:"fetched_at"`
	Meta      *TableMeta `json:"meta"`
}

// Get returns table metadata from the cache when fresh, fetching live (and
// re-caching) otherwise.
func (s *Store) Get(ctx context.Context, table string) (*TableMeta, error) {
	if !s.Refresh {
		if meta := s.GetCached(table); meta != nil {
			return meta, nil
		}
	}
	meta, err := Fetch(ctx, s.Client, table)
	if err != nil {
		return nil, err
	}
	s.write(table, meta)
	return meta, nil
}

// GetCached returns fresh cached metadata or nil — never the network. Used
// by pre-flight validation, which must not add API calls or fail on
// instances where the caller lacks dictionary access.
func (s *Store) GetCached(table string) *TableMeta {
	if s.Dir == "" || s.Refresh {
		return nil
	}
	data, err := os.ReadFile(s.path(table))
	if err != nil {
		return nil
	}
	var entry cacheEntry
	if json.Unmarshal(data, &entry) != nil || entry.Meta == nil || len(entry.Meta.Fields) == 0 {
		return nil
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if time.Since(entry.FetchedAt) > ttl {
		return nil
	}
	return entry.Meta
}

// write persists metadata best-effort — a read-only cache dir must never
// break a query.
func (s *Store) write(table string, meta *TableMeta) {
	if s.Dir == "" {
		return
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(cacheEntry{FetchedAt: time.Now(), Meta: meta})
	if err != nil {
		return
	}
	os.WriteFile(s.path(table), data, 0o600) //nolint:errcheck
}

func (s *Store) path(table string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, table)
	return filepath.Join(s.Dir, safe+".json")
}
