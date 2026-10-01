package netpath

import (
	"fmt"
	"strings"
	"text/tabwriter"
)

// placeLabel renders "FR Paris" or "FR" (or "?" for a hop we cannot place).
func placeLabel(iso, city string) string {
	switch {
	case iso == "" && city == "":
		return "?"
	case city == "":
		return iso
	case iso == "":
		return city
	}
	return iso + " " + city
}

// Route is the path as a short chain such as
// "you → FR Toulouse → FR Paris → edge", merging repeated places and marking
// hops we could not place or do not trust with "?".
func (p Path) Route() string {
	parts := []string{"you"}
	last := "you"
	for _, h := range p.Hops {
		if h.Class != "public" || h.Dest {
			continue
		}
		label := "?"
		if h.ISO != "" && h.Confidence != "low" {
			label = placeLabel(h.ISO, h.City)
		}
		if label == last {
			continue
		}
		parts = append(parts, label)
		last = label
	}
	switch {
	case !p.Reached:
		parts = append(parts, "(destination did not answer)")
	case p.Edge.Kind == "nearby":
		parts = append(parts, "edge")
	case p.Edge.Kind == "consistent":
		parts = append(parts, placeLabel(p.Edge.ISO, p.Edge.City)+" (destination)")
	default:
		parts = append(parts, "destination")
	}
	return strings.Join(parts, " → ")
}

// Summary lines describe what was and was not observed.
func (p Path) observed() string {
	if len(p.Countries) == 0 {
		return "none could be determined"
	}
	return strings.Join(p.Countries, " → ")
}

func (p Path) family() string {
	if p.Dest.Is6() {
		return "IPv6"
	}
	return "IPv4"
}

// RenderText formats paths for a terminal. With verbose it adds the hop table.
func RenderText(paths []Path, verbose bool) string {
	var b strings.Builder
	for i, p := range paths {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s  %s  %s", p.Host, "→", p.Dest)
		status := "reached"
		if !p.Reached {
			status = "not reached"
		}
		if p.Partial {
			status += ", stopped early"
		}
		fmt.Fprintf(&b, "  (%s, %s, %d hops)\n", p.family(), status, len(p.Hops))
		if p.Vantage != nil {
			fmt.Fprintf(&b, "  viewpoint: %s (%s)\n", p.Vantage.Label, vantageNote(p.Vantage))
		}
		for _, w := range p.Warnings {
			fmt.Fprintf(&b, "  ! %s\n", w)
		}
		fmt.Fprintf(&b, "  route:     %s\n", p.Route())
		fmt.Fprintf(&b, "  countries seen on the path: %s\n", p.observed())
		fmt.Fprintf(&b, "  endpoint:  %s\n", p.Edge.describe())
		b.WriteString("  beyond the edge: not observable from here (the provider's own network)\n")
		if verbose {
			b.WriteString("\n")
			b.WriteString(hopTable(p))
		}
	}
	if len(paths) > 1 {
		b.WriteString("\n")
		b.WriteString(comparison(paths))
	}
	return b.String()
}

func vantageNote(v *Point) string {
	if v.Source == "given" {
		return "as given"
	}
	return "estimated from your first public router; use --from to set it"
}

func (e Edge) describe() string {
	switch e.Kind {
	case "nearby":
		where := placeLabel(e.ISO, e.City)
		if where == "?" || strings.Contains(e.Note, where) { // unknown, or already said in the note
			return e.Note
		}
		return fmt.Sprintf("%s (enters the provider near %s)", e.Note, where)
	case "consistent":
		return e.Note
	}
	return e.Note
}

func hopTable(p Path) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  HOP\tADDRESS\tRTT\tPLACE\tSOURCE\tCONF\tHOSTNAME / NOTES")
	for _, h := range p.Hops {
		switch h.Class {
		case "silent":
			fmt.Fprintf(tw, "  %d\t*\t\t\t\t\t(no reply)\n", h.TTL)
		case "private":
			fmt.Fprintf(tw, "  %d\t%s\t%.1f ms\t(local network)\t\t\t\n", h.TTL, h.IP, h.RTT)
		default:
			extra := h.Host
			if len(h.Notes) > 0 {
				if extra != "" {
					extra += "  "
				}
				extra += "! " + strings.Join(h.Notes, "; ")
			}
			if h.Dest {
				extra = "(destination) " + extra
			}
			fmt.Fprintf(tw, "  %d\t%s\t%.1f ms\t%s\t%s\t%s\t%s\n", h.TTL, h.IP, h.RTT, placeLabel(h.ISO, h.City), h.Source, h.Confidence, extra)
		}
	}
	tw.Flush()
	return b.String()
}

func comparison(paths []Path) string {
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ENDPOINT\tCOUNTRIES ON THE PATH\tWHERE IT ENTERS THE PROVIDER\tREGISTERED (GeoIP)")
	for _, p := range paths {
		enters := "unknown"
		switch p.Edge.Kind {
		case "nearby":
			if where := placeLabel(p.Edge.ISO, p.Edge.City); where != "?" {
				enters = fmt.Sprintf("near you: %s (within ~%.0f km)", where, p.Edge.MaxKm)
			} else {
				enters = fmt.Sprintf("near you (within ~%.0f km)", p.Edge.MaxKm)
			}
		case "consistent":
			enters = "consistent with " + placeLabel(p.Edge.ISO, p.Edge.City)
		}
		reg := placeLabel(p.Edge.Registered.ISO, p.Edge.Registered.City)
		if p.Edge.Registered.ISO == "" {
			reg = "no data"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Host, strings.Join(p.Countries, ", "), enters, reg)
	}
	tw.Flush()
	return b.String()
}
