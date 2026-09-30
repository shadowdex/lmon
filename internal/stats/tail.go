package stats

import (
	"bytes"
	"encoding/json"
	"io"
	"os"

	"github.com/shadowdex/lmon/internal/proxy"
)

// Tailer incrementally reads new events appended to a JSONL log. It copes with
// the file not existing yet, being truncated, being rotated, and a half-written
// last line (kept until its newline arrives).
//
// On rotation (the path now points at a different file) it first reads what it
// hadn't yet seen from the old file, now at <path>.1, so no events are lost.
type Tailer struct {
	Path string
	// MaxInitial bounds how much of an existing file is read on the first poll
	// (the newest bytes win). 0 means 64 MiB.
	MaxInitial int64

	offset  int64
	partial []byte
	started bool
	info    os.FileInfo // identity of the file we are reading
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

	var out []proxy.Event
	if t.info != nil && !os.SameFile(t.info, st) { // rotated or replaced
		out = t.drainRotated()
		t.offset, t.partial = 0, nil
	}
	t.info = st
	size := st.Size()

	if size < t.offset { // truncated in place: start over
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
		return out, nil
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return out, err
	}
	buf, err := io.ReadAll(io.LimitReader(f, size-t.offset))
	if err != nil {
		return out, err
	}
	t.offset += int64(len(buf))
	return append(out, t.consume(buf, skipFirst)...), nil
}

// drainRotated reads the unseen tail of the file we were following, which the
// logger renamed to <path>.1. If .1 isn't that file (e.g. rotation is off and
// the file was simply replaced) there is nothing to recover.
func (t *Tailer) drainRotated() []proxy.Event {
	old, err := os.Open(t.Path + ".1")
	if err != nil {
		return nil
	}
	defer old.Close()
	st, err := old.Stat()
	if err != nil || !os.SameFile(t.info, st) || st.Size() <= t.offset {
		return nil
	}
	if _, err := old.Seek(t.offset, io.SeekStart); err != nil {
		return nil
	}
	buf, err := io.ReadAll(io.LimitReader(old, st.Size()-t.offset))
	if err != nil {
		return nil
	}
	evs := t.consume(buf, false)
	t.partial = nil // a half-written line in a closed file will never complete
	return evs
}

// consume parses complete lines from t.partial+buf, keeping any trailing
// incomplete line in t.partial.
func (t *Tailer) consume(buf []byte, skipFirst bool) []proxy.Event {
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
	return out
}
