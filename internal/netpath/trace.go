// Package netpath discovers the network path to an endpoint and works out, as
// honestly as the evidence allows, which countries it crosses.
//
// It runs the system's own traceroute (traceroute, traceroute6 or tracert)
// instead of crafting packets itself, because sending and receiving the
// needed ICMP messages takes root on most systems, while the system tools
// are installed with the right privileges (macOS) or work unprivileged
// (Debian's traceroute, Windows tracert).
package netpath

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Responder is one router that answered probes for a hop.
type Responder struct {
	IP      netip.Addr `json:"ip"`
	MinRTT  float64    `json:"rtt_ms"` // smallest round trip seen, in milliseconds
	Replies int        `json:"replies"`
}

// Hop is one TTL step. Several responders mean the path was load-balanced
// across routers at that step.
type Hop struct {
	TTL        int         `json:"ttl"`
	Responders []Responder `json:"responders,omitempty"`
	Timeouts   int         `json:"timeouts,omitempty"`
	Flags      []string    `json:"flags,omitempty"` // traceroute annotations such as !H (host unreachable)
}

// Silent reports whether nothing answered at this hop. Routers that never
// send "time exceeded" replies are common, so this is not an error.
func (h Hop) Silent() bool { return len(h.Responders) == 0 }

// Primary is the responder to use for locating the hop: the one that
// answered most often, ties broken by the lower round trip.
func (h Hop) Primary() (Responder, bool) {
	var best Responder
	found := false
	for _, r := range h.Responders {
		if !found || r.Replies > best.Replies || (r.Replies == best.Replies && r.MinRTT < best.MinRTT) {
			best, found = r, true
		}
	}
	return best, found
}

// Trace is the result of tracing to Dest.
type Trace struct {
	Dest    netip.Addr `json:"dest"`
	Hops    []Hop      `json:"hops"`
	Reached bool       `json:"reached"` // the destination, or something answering for it, ended the trace
	// Terminal is the router that ended the trace when it was not the probed
	// address itself. Anycast and load-balanced endpoints commonly answer from
	// their own unicast address, so this is the real end of the path.
	Terminal netip.Addr `json:"terminal,omitempty"`
	// ConnectMs is the best of a few TCP handshakes to the destination on port
	// 443, 0 if none succeeded. Router replies come from a slow control plane, so
	// this says far more about how close the endpoint really is.
	ConnectMs float64 `json:"connect_ms,omitempty"`
	Partial   bool    `json:"partial,omitempty"` // stopped by the timeout before finishing
}

// ---- parsing ----

func addHop(hops []Hop, h *Hop) []Hop {
	if h != nil {
		return append(hops, *h)
	}
	return hops
}

func (h *Hop) reply(ip netip.Addr, rtt float64) {
	for i := range h.Responders {
		if h.Responders[i].IP == ip {
			if rtt < h.Responders[i].MinRTT {
				h.Responders[i].MinRTT = rtt
			}
			h.Responders[i].Replies++
			return
		}
	}
	h.Responders = append(h.Responders, Responder{IP: ip, MinRTT: rtt, Replies: 1})
}

func (h *Hop) flag(f string) {
	for _, x := range h.Flags {
		if x == f {
			return
		}
	}
	h.Flags = append(h.Flags, f)
}

func parseAddr(tok string) (netip.Addr, bool) {
	tok = strings.Trim(tok, "()[]")
	ip, err := netip.ParseAddr(tok)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.WithZone(""), true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// ParseUnix parses the output of BSD/macOS traceroute(6) and Debian-style
// traceroute run with -n. Both print the same tokens; they differ only in
// whether replies from several routers at one hop are split over continuation
// lines (BSD) or kept on one line (Debian), which the parser doesn't need to know.
//
//	1  192.168.0.1  3.8 ms  4.3 ms  3.7 ms
//	2  * 203.0.113.1  7.2 ms
//	   203.0.113.2  7.1 ms
//
// An address is printed when the responder changes; "*" is a lost probe.
func ParseUnix(text string) []Hop {
	var hops []Hop
	var cur *Hop
	var curIP netip.Addr

	for _, line := range strings.Split(text, "\n") {
		toks := strings.Fields(line)
		if len(toks) == 0 || strings.HasPrefix(toks[0], "traceroute") {
			continue
		}
		if isDigits(toks[0]) && len(toks) > 1 {
			hops = addHop(hops, cur)
			ttl, _ := strconv.Atoi(toks[0])
			cur = &Hop{TTL: ttl}
			curIP = netip.Addr{}
			toks = toks[1:]
		}
		if cur == nil {
			continue
		}
		for i := 0; i < len(toks); i++ {
			t := toks[i]
			switch {
			case t == "*":
				cur.Timeouts++
			case strings.HasPrefix(t, "!"):
				cur.flag(t)
			default:
				if ip, ok := parseAddr(t); ok {
					curIP = ip
					continue
				}
				if v, err := strconv.ParseFloat(t, 64); err == nil && i+1 < len(toks) && toks[i+1] == "ms" {
					if curIP.IsValid() {
						cur.reply(curIP, v)
					}
					i++
				}
				// anything else (a hostname printed without -n) is ignored
			}
		}
	}
	return addHop(hops, cur)
}

// ParseWindows parses `tracert -d`. The responder address is the last token
// of the line, and round trips look like "<1 ms", "8 ms" or "*". Only digits,
// "ms", "*" and addresses are interpreted, so localized Windows output parses too.
func ParseWindows(text string) []Hop {
	var hops []Hop
	for _, line := range strings.Split(text, "\n") {
		toks := strings.Fields(line)
		if len(toks) < 2 || !isDigits(toks[0]) {
			continue
		}
		ttl, _ := strconv.Atoi(toks[0])
		h := Hop{TTL: ttl}
		var rtts []float64
		var ip netip.Addr
		for i := 1; i < len(toks); i++ {
			t := toks[i]
			switch {
			case t == "*":
				h.Timeouts++
			case t == "ms":
			case strings.HasPrefix(t, "<") && i+1 < len(toks) && toks[i+1] == "ms":
				rtts = append(rtts, 0.5) // "<1 ms": below the clock's resolution
			default:
				if a, ok := parseAddr(t); ok {
					ip = a
				} else if v, err := strconv.ParseFloat(t, 64); err == nil && i+1 < len(toks) && toks[i+1] == "ms" {
					rtts = append(rtts, v)
				}
			}
		}
		if ip.IsValid() {
			for _, v := range rtts {
				h.reply(ip, v)
			}
			if len(rtts) == 0 { // an address with no timing, e.g. "Destination host unreachable"
				h.Responders = append(h.Responders, Responder{IP: ip})
			}
		}
		hops = append(hops, h)
	}
	return hops
}

// ---- running ----

// ErrNoTraceroute means the system has no traceroute tool installed.
type ErrNoTraceroute struct{ Tool, Hint string }

func (e ErrNoTraceroute) Error() string {
	return fmt.Sprintf("%s not found. %s", e.Tool, e.Hint)
}

// Runner runs the system traceroute. The zero value uses sensible defaults.
type Runner struct {
	MaxHops      int           // default 30
	ProbesPerHop int           // default 2
	Wait         time.Duration // per-probe wait, default 1s (whole seconds, rounded up)

	GOOS string // default runtime.GOOS; for tests
	Exec func(ctx context.Context, name string, args ...string) ([]byte, error)
	Look func(name string) error // default exec.LookPath; for tests
}

func (r Runner) defaults() Runner {
	if r.MaxHops == 0 {
		r.MaxHops = 30
	}
	if r.ProbesPerHop == 0 {
		r.ProbesPerHop = 2
	}
	if r.Wait == 0 {
		r.Wait = time.Second // a silent hop costs ProbesPerHop x Wait, and these are the common case
	}
	if r.GOOS == "" {
		r.GOOS = runtime.GOOS
	}
	if r.Exec == nil {
		r.Exec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			// macOS's traceroute is setuid root, so when the deadline fires we may
			// be unable to signal it. Don't wait for it: return what it printed.
			cmd.WaitDelay = 2 * time.Second
			return cmd.CombinedOutput()
		}
	}
	if r.Look == nil {
		r.Look = func(name string) error { _, err := exec.LookPath(name); return err }
	}
	return r
}

// command picks the tool and arguments for the platform and address family.
func (r Runner) command(dest netip.Addr) (string, []string) {
	secs := int((r.Wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	d := dest.String()
	switch r.GOOS {
	case "windows":
		fam := "-4"
		if dest.Is6() {
			fam = "-6"
		}
		return "tracert", []string{fam, "-d", "-h", strconv.Itoa(r.MaxHops), "-w", strconv.Itoa(secs * 1000), d}
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		name := "traceroute"
		if dest.Is6() {
			name = "traceroute6"
		}
		return name, []string{"-n", "-q", strconv.Itoa(r.ProbesPerHop), "-w", strconv.Itoa(secs), "-m", strconv.Itoa(r.MaxHops), d}
	default: // Linux: Debian-style traceroute takes -6 for IPv6
		args := []string{"-n", "-q", strconv.Itoa(r.ProbesPerHop), "-w", strconv.Itoa(secs), "-m", strconv.Itoa(r.MaxHops)}
		if dest.Is6() {
			args = append([]string{"-6"}, args...)
		}
		return "traceroute", append(args, d)
	}
}

func installHint(goos string) string {
	switch goos {
	case "windows":
		return "tracert ships with Windows; check that %SystemRoot%\\System32 is on your PATH."
	case "darwin":
		return "it normally lives at /usr/sbin/traceroute; check your PATH."
	default:
		return "Install it, e.g. Debian/Ubuntu: apt install traceroute; Fedora: dnf install traceroute; Alpine: apk add traceroute."
	}
}

// Trace runs the tool against dest. If ctx expires first, the hops found so far
// are returned with Partial set.
func (r Runner) Trace(ctx context.Context, dest netip.Addr) (Trace, error) {
	r = r.defaults()
	name, args := r.command(dest)
	if err := r.Look(name); err != nil {
		return Trace{}, ErrNoTraceroute{Tool: name, Hint: installHint(r.GOOS)}
	}
	out, runErr := r.Exec(ctx, name, args...)

	var hops []Hop
	if r.GOOS == "windows" {
		hops = ParseWindows(string(out))
	} else {
		hops = ParseUnix(string(out))
	}
	if len(hops) == 0 {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		if runErr == nil {
			runErr = errors.New("no hops in the output")
		}
		return Trace{}, fmt.Errorf("%s produced no usable output (%v): %s", name, runErr, msg)
	}
	t := Trace{Dest: dest, Hops: hops, Partial: ctx.Err() != nil}
	for _, h := range hops {
		for _, resp := range h.Responders {
			if resp.IP == dest {
				t.Reached = true
			}
		}
	}
	// traceroute stops as soon as something reports the destination reached (a
	// "port unreachable" or echo reply), even from another address. So a trace
	// that ended early on a plain reply reached the end of the path; one that
	// ended on an unreachable flag, a silent hop, the hop limit or a timeout did not.
	if last := hops[len(hops)-1]; !t.Reached && !t.Partial && len(hops) < r.MaxHops && !last.Silent() && len(last.Flags) == 0 {
		if p, ok := last.Primary(); ok {
			t.Reached, t.Terminal = true, p.IP
		}
	}
	return t, nil
}
