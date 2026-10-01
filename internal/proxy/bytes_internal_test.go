package proxy

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

// The captured copy is bounded, but the byte count must stay exact however large the response.
func TestTeeBodyCountsPastTheCaptureLimit(t *testing.T) {
	n := int64(maxCapture) + 123456
	var gotCaptured int
	var gotTotal int64
	tb := &teeBody{
		rc:    io.NopCloser(io.LimitReader(zeros{}, n)),
		onEOF: func(body []byte, total int64) { gotCaptured, gotTotal = len(body), total },
	}
	if _, err := io.Copy(io.Discard, tb); err != nil {
		t.Fatal(err)
	}
	if gotTotal != n {
		t.Fatalf("total = %d, want %d", gotTotal, n)
	}
	if gotCaptured < maxCapture || int64(gotCaptured) >= n {
		t.Fatalf("the captured copy should stop near the limit, got %d of %d", gotCaptured, n)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestCountingBodyAndNoDoubleCountOnClose(t *testing.T) {
	var n atomicInt
	cb := &countingBody{rc: io.NopCloser(strings.NewReader("12345")), n: &n.Int64}
	io.Copy(io.Discard, cb)
	cb.Close()
	if n.Load() != 5 {
		t.Fatalf("counted %d", n.Load())
	}
}

type atomicInt struct{ atomic.Int64 }
