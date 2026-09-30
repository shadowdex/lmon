package stats

import (
	"bytes"
	"encoding/json"
	"io"
	"os"

	"github.com/shadowdex/lmon/internal/proxy"
)

// Tailer incrementally reads new events appended to a JSONL log. It copes with
// the file not existing yet, being truncated/replaced, and a half-written last
// line (kept until its newline arrives).
type Tailer struct {
	Path string
	// MaxInitial bounds how much of an existing file is read on the first poll
	// (the newest bytes win). 0 means 64 MiB.
	MaxInitial int64

	offset  int64
	partial []byte
	started bool
}

func (t *Tailer) Poll() ([]proxy.Event, error) {
	f, err := os.Open(t.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := st.Size()

	if size < t.offset { // truncated or replaced: start over
		t.offset, t.partial = 0, nil
	}
	skipFirst := false
	if !t.started {
		t.started = true
		max := t.MaxInitial
		if max == 0 {
			max = 64 << 20
		}
		if size > max {
			t.offset = size - max
			skipFirst = true // we landed mid-line
		}
	}
	if size == t.offset {
		return nil, nil
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, size-t.offset))
	if err != nil {
		return nil, err
	}
	t.offset += int64(len(buf))

	data := append(t.partial, buf...)
	t.partial = nil
	if skipFirst {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	var out []proxy.Event
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			t.partial = append([]byte(nil), data...)
			break
		}
		line := bytes.TrimSpace(data[:i])
		data = data[i+1:]
		if len(line) == 0 {
			continue
		}
		var e proxy.Event
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}
