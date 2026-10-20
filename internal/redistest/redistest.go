// Package redistest gives tests a Redis of their own keys: the one SWITCHBOARD_TEST_REDIS names (make test starts
// one on 6382; CI runs one as a service container), with a key prefix no other test uses, emptied when the test ends.
// Without the variable the test is skipped, so `go test ./...` still runs everywhere; with it set and no Redis
// answering, the test fails rather than skips, so CI can't pass by skipping.
package redistest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// Env is the variable that names the test Redis, as host:port.
const Env = "SWITCHBOARD_TEST_REDIS"

// Addr returns the test Redis's address, skipping the test if there is none.
func Addr(t testing.TB) string {
	t.Helper()
	addr := os.Getenv(Env)
	if addr == "" {
		t.Skipf("%s is unset: no Redis to test against (make test starts one)", Env)
	}
	return addr
}

// Prefix returns a key prefix of the test's own, and deletes every key under it when the test ends.
func Prefix(t testing.TB) string {
	t.Helper()
	addr := Addr(t)
	var b [6]byte
	rand.Read(b[:])
	prefix := "switchboard-test:" + hex.EncodeToString(b[:]) + ":"
	rdb := New(addr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		t.Fatalf("%s=%s, but Redis doesn't answer: %v", Env, addr, err)
	}
	t.Cleanup(func() {
		defer rdb.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		iter := rdb.Scan(ctx, 0, prefix+"*", 1000).Iterator()
		for iter.Next(ctx) {
			rdb.Del(ctx, iter.Val())
		}
	})
	return prefix
}

// Client returns a client of the test Redis and a prefix of the test's own.
func Client(t testing.TB) (*redis.Client, string) {
	t.Helper()
	prefix := Prefix(t)
	rdb := New(Addr(t))
	t.Cleanup(func() { rdb.Close() })
	return rdb, prefix
}

// New is a client of addr as the tests use it: no retries, and none of the handshakes a test Redis may not know.
func New(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:                     addr,
		MaxRetries:               -1,
		DialerRetries:            1,
		DisableIdentity:          true,
		MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
	})
}
