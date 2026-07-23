package schema

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// <user-cache-dir>/glidemind/schema/<instance-host>/<identity>/. The
// identity segment matters: dictionary rows are ACL-filtered per user, so
// metadata cached by one identity must never drive another identity's
// validation or default fields.
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
	// The identity segment must not collide across users: dictionary rows
	// are ACL-filtered, so one identity's metadata must never key another's.
	// safeSegment alone is lossy (svc.user and svc_user both map to
	// svc_user), so pair the readable prefix with a digest of the exact
	// origin+username.
	identity := safeSegment(client.Username()) + "-" + digest(client.BaseURL()+"\x00"+client.Username())
	return &Store{
		Client: client,
		Dir:    filepath.Join(base, "glidemind", "schema", host, identity),
		TTL:    DefaultTTL,
	}, nil
}

// digest returns a short collision-resistant hex tag for s.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
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

// Refetch fetches table metadata live and refreshes the cache, regardless of
// Refresh. It self-heals validation when the cache predates a new field.
func (s *Store) Refetch(ctx context.Context, table string) (*TableMeta, error) {
	meta, err := Fetch(ctx, s.Client, table)
	if err != nil {
		return nil, err
	}
	s.write(table, meta)
	return meta, nil
}

// CachedAt returns when the fresh cached entry for table was fetched and
// whether one exists. It ignores Refresh (a pure cache read) so callers can
// report cache age even when about to refetch.
func (s *Store) CachedAt(table string) (time.Time, bool) {
	if s.Dir == "" {
		return time.Time{}, false
	}
	data, err := os.ReadFile(s.path(table))
	if err != nil {
		return time.Time{}, false
	}
	var entry cacheEntry
	if json.Unmarshal(data, &entry) != nil || entry.Meta == nil {
		return time.Time{}, false
	}
	ttl := s.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if time.Since(entry.FetchedAt) > ttl {
		return time.Time{}, false
	}
	return entry.FetchedAt, true
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
	return filepath.Join(s.Dir, safeSegment(table)+".json")
}

// safeSegment reduces a value to filesystem-safe path-segment characters.
func safeSegment(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, s)
}
