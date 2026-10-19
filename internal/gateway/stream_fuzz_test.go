package gateway

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// referenceEvents splits a whole stream into events the simple way, with the input all in memory: lines end at
// \n; a blank line ("\n" or "\r\n") ends the event before it, or is skipped if there is none; an event is its lines
// and that blank line. It is what eventReader does incrementally, in a buffer, under a limit.
func referenceEvents(in []byte, limit int64) (raws, datas []string, end error) {
	if int64(len(in)) > limit {
		// eventReader reads at most limit bytes: it returns the events that end within them, then stops. (This
		// kept limit+1 bytes until the fuzzer found the reference accepting an event that ended one byte past the
		// limit.)
		in = in[:limit]
		defer func() {
			if end == errStreamEnded || end == io.EOF {
				end = errStreamTooBig
			}
		}()
	}
	var raw []byte
	var data [][]byte
	hasData := false
	first := true
	for len(in) > 0 {
		i := bytes.IndexByte(in, '\n')
		if i < 0 {
			return raws, datas, errStreamEnded // a line that never ends
		}
		line := in[:i+1]
		in = in[i+1:]
		text := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if first { // the spec's optional byte order mark, before the first line only
			first = false
			text = bytes.TrimPrefix(text, []byte("\xef\xbb\xbf"))
		}
		if len(text) == 0 {
			if len(raw) == 0 {
				continue
			}
			raws = append(raws, string(raw)+string(line))
			if hasData {
				datas = append(datas, string(bytes.Join(data, []byte("\n"))))
			} else {
				datas = append(datas, "<none>")
			}
			raw, data, hasData = nil, nil, false
			continue
		}
		raw = append(raw, line...)
		if v, ok := bytes.CutPrefix(text, []byte("data:")); ok {
			data, hasData = append(data, bytes.TrimPrefix(v, []byte(" "))), true
		} else if string(text) == "data" {
			data, hasData = append(data, nil), true
		}
	}
	if len(raw) > 0 {
		return raws, datas, errStreamEnded
	}
	return raws, datas, io.EOF
}

func FuzzEventReaderAgreesWithTheReference(f *testing.F) {
	for _, seed := range []string{
		"data: a\n\n", "data: a\r\n\r\ndata: b\n\n", ": ping\n\ndata: x\n\n", "data: a\ndata: b\n\n",
		"\n\n\ndata: a\n\n", "data: a", "data: a\n", "event: e\nid: 1\ndata:x\n\n", "data: [DONE]\n\n",
		"data: " + strings.Repeat("long", 40) + "\n\n", "data: a\r\n\n", "\r\n\r\n", "data: \xff\n\n",
		"\xef\xbb\xbfdata: a\n\n", "\xef\xbb\xbf\ndata: a\n\n", "data: a\n\n\xef\xbb\xbfdata: b\n\n",
		"data\n\n", "data\ndata: b\n\n", "datax\n\n",
	} {
		f.Add([]byte(seed), uint16(1000), uint8(0))
	}
	f.Fuzz(func(t *testing.T, in []byte, limit uint16, chunk uint8) {
		// A tiny buffer, so lines longer than it are common, and the input delivered a few bytes at a time when
		// chunk says so, so line and event boundaries fall anywhere in a read.
		var body io.Reader = bytes.NewReader(in)
		if chunk%2 == 1 {
			body = iotest.HalfReader(body)
		}
		if chunk%3 == 1 {
			body = iotest.OneByteReader(body)
		}
		er := newEventReaderSize(body, int64(limit), 16)
		var raws, datas []string
		var end error
		for {
			ev, err := er.next()
			if err != nil {
				end = err
				break
			}
			raws = append(raws, string(ev.raw))
			if ev.data == nil {
				datas = append(datas, "<none>")
			} else {
				datas = append(datas, string(ev.data))
			}
		}
		wantRaws, wantDatas, wantEnd := referenceEvents(in, int64(limit))
		if strings.Join(raws, "|") != strings.Join(wantRaws, "|") ||
			strings.Join(datas, "|") != strings.Join(wantDatas, "|") || end != wantEnd {
			t.Errorf("input %q, limit %d:\nreader    %q %q %v\nreference %q %q %v", in, limit, raws, datas, end,
				wantRaws, wantDatas, wantEnd)
		}
	})
}
