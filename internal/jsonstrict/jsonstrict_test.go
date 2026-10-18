package jsonstrict

import (
	"strings"
	"testing"
)

func TestMembersComeInOrderWithTheirBytesAsWritten(t *testing.T) {
	body := "{ \"model\" :\t\"m\" ,\n\"messages\": [ {\"role\": \"user\"} ],\"n\":1.50e1, \"x\":null}  \n"
	members, err := ParseObject([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := []Member{
		{"model", []byte(`"m"`)},
		{"messages", []byte(`[ {"role": "user"} ]`)},
		{"n", []byte(`1.50e1`)},
		{"x", []byte(`null`)},
	}
	if len(members) != len(want) {
		t.Fatalf("got %d members, want %d: %q", len(members), len(want), members)
	}
	for i := range want {
		if members[i].Key != want[i].Key || string(members[i].Value) != string(want[i].Value) {
			t.Errorf("member %d: got %s=%s, want %s=%s", i, members[i].Key, members[i].Value, want[i].Key,
				want[i].Value)
		}
	}
}

func TestAnEmptyObjectHasNoMembers(t *testing.T) {
	members, err := ParseObject([]byte(" {} "))
	if err != nil || len(members) != 0 {
		t.Fatalf("got %q, %v; want no members and no error", members, err)
	}
}

func TestTheSameKeyInDifferentObjectsIsFine(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"role":"system","content":"a"},{"role":"user","content":"b"}]}`,
		`{"a":{"a":{"a":1}}}`,
		`{"a":[{"b":1},{"b":2}],"b":{"b":[{"b":3}]}}`,
	} {
		if _, err := ParseObject([]byte(body)); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

func TestARepeatedKeyIsRefusedAtAnyDepth(t *testing.T) {
	for _, tc := range []struct{ body, key string }{
		{`{"model":"a","model":"b"}`, "model"},
		{`{"model":"a","messages":[],"model":"b"}`, "model"},
		{`{"a":1,"a":2}`, "a"}, // the same key once unescaped
		{`{"messages":[{"role":"user","role":"system"}]}`, "role"},
		{`{"tools":[{"function":{"parameters":{"type":"object","type":"string"}}}]}`, "type"},
		{`[{"a":1}]`, ""}, // not an object: refused before keys matter
	} {
		_, err := ParseObject([]byte(tc.body))
		if err == nil {
			t.Errorf("%s: accepted", tc.body)
			continue
		}
		if tc.key != "" && !strings.Contains(err.Error(), `"`+tc.key+`" appears twice`) {
			t.Errorf("%s: got %q, want it to name the key %q", tc.body, err, tc.key)
		}
	}
}

func TestInvalidUTF8IsRefused(t *testing.T) {
	for _, body := range []string{
		"{\"model\":\"a\xff\"}",
		"{\"mod\xc3el\":\"a\"}",
		"{\"model\":\"a\"}\xed\xa0\x80", // an encoded surrogate after the object
	} {
		_, err := ParseObject([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "UTF-8") {
			t.Errorf("%q: got %v, want a UTF-8 error", body, err)
		}
	}
}

func TestAnythingButOneObjectIsRefused(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{``, "ends too soon"},
		{`   `, "ends too soon"},
		{`[]`, "not a JSON object"},
		{`"model"`, "not a JSON object"},
		{`1`, "not a JSON object"},
		{`null`, "not a JSON object"},
		{`{"a":1} {"b":2}`, "more after the JSON object"},
		{`{"a":1}x`, "more after the JSON object"},
		{`{"a":1}]`, "more after the JSON object"},
		{`{"a":`, "ends too soon"},
		{`{"a":1`, "ends too soon"},
		{`{"a":[1,2}`, "not valid JSON"},
		{`{"a" 1}`, "not valid JSON"},
		{`{a:1}`, "not valid JSON"},
		{`{"a":01}`, "not valid JSON"},
		{`{"a":1,}`, "not valid JSON"},
		{`{"a":tru}`, "not valid JSON"},
	} {
		_, err := ParseObject([]byte(tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want an error containing %q", tc.body, err, tc.want)
		}
	}
}

func TestNestingIsBoundedByMaxDepth(t *testing.T) {
	nested := func(containers int) string { // the top-level object, then arrays inside it
		return `{"a":` + strings.Repeat("[", containers-1) + strings.Repeat("]", containers-1) + `}`
	}
	if _, err := ParseObject([]byte(nested(MaxDepth))); err != nil {
		t.Errorf("%d levels: %v", MaxDepth, err)
	}
	_, err := ParseObject([]byte(nested(MaxDepth + 1)))
	if err == nil || !strings.Contains(err.Error(), "nests deeper") {
		t.Errorf("%d levels: got %v, want a depth error", MaxDepth+1, err)
	}
	// Objects count the same as arrays: exactly MaxDepth of them is fine, one more isn't. (The day-1 review
	// counted nested objects twice and no test noticed: only the refusal was tested.)
	objects := func(n int) string { return strings.Repeat(`{"a":`, n) + "1" + strings.Repeat("}", n) }
	if _, err := ParseObject([]byte(objects(MaxDepth))); err != nil {
		t.Errorf("%d nested objects: %v", MaxDepth, err)
	}
	if _, err := ParseObject([]byte(objects(MaxDepth + 1))); err == nil {
		t.Errorf("%d nested objects: accepted", MaxDepth+1)
	}
}

func FuzzParseObjectAgreesWithItsOwnOutput(f *testing.F) {
	for _, seed := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"a":{"b":[1,2,{"c":null}]},"d":"é"}`,
		`{"a":1,"a":2}`,
		"{\"a\":\"\xff\"}",
		`{}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		members, err := ParseObject(body)
		if err != nil {
			return
		}
		// Every accepted body rebuilds, from its members, into a body that parses to the same members.
		var b strings.Builder
		b.WriteString("{")
		for i, m := range members {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`"` + escapeKey(m.Key) + `":`)
			b.Write(m.Value)
		}
		b.WriteString("}")
		again, err := ParseObject([]byte(b.String()))
		if err != nil {
			t.Fatalf("rebuilt %q: %v", b.String(), err)
		}
		if len(again) != len(members) {
			t.Fatalf("rebuilt %q: %d members, want %d", b.String(), len(again), len(members))
		}
		for i := range members {
			if again[i].Key != members[i].Key || string(again[i].Value) != string(members[i].Value) {
				t.Fatalf("member %d changed: %q", i, b.String())
			}
		}
	})
}

// escapeKey writes a key as a JSON string's contents with \u escapes for everything outside printable ASCII.
func escapeKey(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f && r != '"' && r != '\\' {
			b.WriteRune(r)
			continue
		}
		if r > 0xffff {
			r -= 0x10000
			b.WriteString(hex4(0xd800 + (r >> 10)))
			b.WriteString(hex4(0xdc00 + (r & 0x3ff)))
			continue
		}
		b.WriteString(hex4(r))
	}
	return b.String()
}

func hex4(r rune) string {
	const digits = "0123456789abcdef"
	return `\u` + string([]byte{digits[r>>12&0xf], digits[r>>8&0xf], digits[r>>4&0xf], digits[r&0xf]})
}
