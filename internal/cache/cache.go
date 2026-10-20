// Package cache keeps answers in Redis, for models whose configuration asks (DESIGN.md, "Caching"). An entry is the
// answer exactly as a client got it (a completion's body, or a stream's bytes) and whose answer it is, under a key that is the SHA-256 of
// everything that decides the answer: the scope (the client, or every client for a shared cache), the model's own
// name (so an alias and its model share entries) and the request's body with that name in it, compacted (so
// whitespace doesn't matter; any other difference, the order of keys included, is another entry, which is safe: a
// miss is never wrong, a wrong hit would be).
package cache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// MaxEntryBytes bounds an entry: a larger answer isn't kept.
const MaxEntryBytes = 1 << 20

// Shared is the scope of a cache every client shares. No client's name can be it.
const Shared = "*"

// version starts every key's hash: a change to what an entry holds changes it, so old entries are never misread.
const version = "switchboard-cache-v1"

// Key is the cache key of a request: scope, the model's own name, and the request's body with that name as its
// model.
func Key(scope, model string, body []byte) string {
	h := sha256.New()
	for _, part := range []string{version, scope, model} {
		h.Write([]byte(part))
		h.Write([]byte{0}) // no name holds a NUL, so the parts can't run into each other
	}
	var compact bytes.Buffer
	if json.Compact(&compact, body) == nil {
		h.Write(compact.Bytes())
	} else {
		h.Write(body) // can't happen: the body was accepted as JSON
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Entry is one cached answer.
type Entry struct {
	Provider      string `json:"provider"` // whose answer it is
	UpstreamModel string `json:"upstream_model"`
	Stream        bool   `json:"stream"`
	Events        int    `json:"events,omitempty"` // a stream's data events
	Body          []byte `json:"-"`                // the completion, or the stream's bytes
}

// Cache is the answers in Redis.
type Cache struct {
	rdb    redis.Cmdable
	prefix string
}

// New keeps answers in rdb, under prefix.
func New(rdb redis.Cmdable, prefix string) *Cache {
	return &Cache{rdb: rdb, prefix: prefix}
}

func (c *Cache) redisKey(key string) string { return c.prefix + "cache:" + key }

// errCorrupt is an entry that can't be read: it is a miss.
var errCorrupt = errors.New("a cache entry that can't be read")

// Get returns the entry under key. A miss is (Entry{}, false, nil); an error means Redis couldn't say.
func (c *Cache) Get(ctx context.Context, key string) (Entry, bool, error) {
	b, err := c.rdb.Get(ctx, c.redisKey(key)).Bytes()
	if err == redis.Nil {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, err
	}
	// The entry is its description in JSON, a newline, and the body as it was: encoding/json never writes a raw
	// newline, so the first one ends the description, whatever the body holds.
	head, body, ok := bytes.Cut(b, []byte("\n"))
	var e Entry
	if !ok || json.Unmarshal(head, &e) != nil || e.Provider == "" {
		return Entry{}, false, errCorrupt
	}
	e.Body = body
	return e, true, nil
}

// Put keeps an entry under key for ttl. An entry larger than MaxEntryBytes isn't kept (and isn't an error).
func (c *Cache) Put(ctx context.Context, key string, e Entry, ttl time.Duration) (kept bool, err error) {
	if len(e.Body) > MaxEntryBytes {
		return false, nil
	}
	head, err := json.Marshal(e)
	if err != nil {
		return false, err
	}
	v := make([]byte, 0, len(head)+1+len(e.Body))
	v = append(append(append(v, head...), '\n'), e.Body...)
	if err := c.rdb.Set(ctx, c.redisKey(key), v, ttl).Err(); err != nil {
		return false, err
	}
	return true, nil
}
