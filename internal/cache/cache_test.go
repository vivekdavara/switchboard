package cache

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/vivekdavara/switchboard/internal/redistest"
)

func TestTheKeyIsEverythingThatDecidesTheAnswer(t *testing.T) {
	const body = `{"model":"fast","messages":[{"role":"user","content":"hi"}],"temperature":0}`
	k := Key("alice", "fast", []byte(body))
	if len(k) != 64 {
		t.Fatalf("%q", k)
	}
	if Key("alice", "fast", []byte("{\n  \"model\": \"fast\",\n  \"messages\": [{\"role\": \"user\", \"content\": \"hi\"}],"+
		" \"temperature\": 0\n}")) != k {
		t.Error("whitespace changed the key")
	}
	for name, other := range map[string]string{
		"another client":             Key("bob", "fast", []byte(body)),
		"shared":                     Key(Shared, "fast", []byte(body)),
		"another model":              Key("alice", "smart", []byte(body)),
		"another message":            Key("alice", "fast", []byte(`{"model":"fast","messages":[{"role":"user","content":"hj"}],"temperature":0}`)),
		"a parameter":                Key("alice", "fast", []byte(`{"model":"fast","messages":[{"role":"user","content":"hi"}],"temperature":1}`)),
		"keys reordered":             Key("alice", "fast", []byte(`{"messages":[{"role":"user","content":"hi"}],"model":"fast","temperature":0}`)),
		"parts run together":         Key("alic", "efast", []byte(body)),
		"whitespace inside a string": Key("alice", "fast", []byte(`{"model":"fast","messages":[{"role":"user","content":"h i"}],"temperature":0}`)),
	} {
		if other == k {
			t.Errorf("%s: the same key", name)
		}
	}
}

func TestAnEntryComesBackAsItWent(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	c := New(rdb, prefix)
	ctx := context.Background()
	if _, ok, err := c.Get(ctx, "k"); ok || err != nil {
		t.Fatalf("an empty cache: %v %v", ok, err)
	}
	stream := []byte(": ping\n\ndata: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	for _, e := range []Entry{
		{Provider: "p0", UpstreamModel: "m", Body: []byte(`{"id":"x"}`)},
		{Provider: "p1", UpstreamModel: "m/2", Stream: true, Events: 1, Body: stream},
		{Provider: "p2", UpstreamModel: "m", Body: []byte("{}\n\n\x00\xff")}, // bytes, whatever they are
	} {
		if kept, err := c.Put(ctx, e.Provider, e, time.Minute); !kept || err != nil {
			t.Fatalf("%v %v", kept, err)
		}
		got, ok, err := c.Get(ctx, e.Provider)
		if !ok || err != nil || got.Provider != e.Provider || got.UpstreamModel != e.UpstreamModel ||
			got.Stream != e.Stream || got.Events != e.Events || !bytes.Equal(got.Body, e.Body) {
			t.Errorf("%+v came back as %+v (%v %v)", e, got, ok, err)
		}
	}
	if ttl := rdb.PTTL(ctx, c.redisKey("p0")).Val(); ttl <= 59*time.Second || ttl > time.Minute {
		t.Errorf("PTTL %s", ttl)
	}
}

func TestAnEntryTooLargeIsntKept(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	c := New(rdb, prefix)
	ctx := context.Background()
	kept, err := c.Put(ctx, "big", Entry{Provider: "p", Body: make([]byte, MaxEntryBytes+1)}, time.Minute)
	if kept || err != nil {
		t.Errorf("%v %v", kept, err)
	}
	if kept, err = c.Put(ctx, "max", Entry{Provider: "p", Body: make([]byte, MaxEntryBytes)}, time.Minute); !kept ||
		err != nil {
		t.Errorf("exactly the limit: %v %v", kept, err)
	}
	if _, ok, _ := c.Get(ctx, "big"); ok {
		t.Error("kept")
	}
}

func TestAnEntryThatCantBeReadIsAMiss(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	c := New(rdb, prefix)
	ctx := context.Background()
	for i, v := range []string{"no newline", "not json\nbody", `{"provider":""}` + "\nbody", "\n"} {
		rdb.Set(ctx, c.redisKey("k"), v, time.Minute)
		if _, ok, err := c.Get(ctx, "k"); ok || err == nil {
			t.Errorf("%d %q: %v %v", i, v, ok, err)
		}
	}
}

func TestEntriesExpire(t *testing.T) {
	rdb, prefix := redistest.Client(t)
	c := New(rdb, prefix)
	ctx := context.Background()
	c.Put(ctx, "k", Entry{Provider: "p", Body: []byte("{}")}, 50*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	if _, ok, err := c.Get(ctx, "k"); ok || err != nil {
		t.Errorf("after its ttl: %v %v", ok, err)
	}
}
