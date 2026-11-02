package verification

import (
	"fmt"
	"io"
	"slices"
)

const (
	Pass       = "PASS"
	Fail       = "FAIL"
	Incomplete = "INCOMPLETE"
)

type Report struct {
	Result    string
	Findings  []Finding
	Manifests []Manifest
	Missing   []string
}

// Verify runs every invariant. A missing or truncated source, or an amount that does not parse,
// makes the result INCOMPLETE, never PASS, whatever the invariants say.
func Verify(d *Dataset) Report {
	x := newIndex(d)
	r := Report{Result: Pass, Manifests: d.Manifests, Missing: slices.Clone(d.Incomplete)}
	for _, inv := range invariants {
		r.Findings = append(r.Findings, inv.Check(x)...)
	}
	r.Missing = append(r.Missing, x.malformedReasons()...)
	switch {
	case len(r.Missing) > 0:
		r.Result = Incomplete
	case len(r.Findings) > 0:
		r.Result = Fail
	}
	return r
}

func (r Report) Failed(invariant string) bool {
	for _, f := range r.Findings {
		if f.Invariant == invariant {
			return true
		}
	}
	return false
}

func (r Report) Print(w io.Writer) {
	for _, m := range r.Manifests {
		_, _ = fmt.Fprintf(w, "source %-9s captured %s hash %.16s counts %v\n", m.Source, m.CapturedAt, m.ContentHash, m.Counts)
	}
	for _, missing := range r.Missing {
		_, _ = fmt.Fprintf(w, "INCOMPLETE source: %s\n", missing)
	}
	for _, inv := range invariants {
		status := "ok"
		if r.Failed(inv.ID) {
			status = "BROKEN"
		}
		_, _ = fmt.Fprintf(w, "%-4s %-6s %s\n", inv.ID, status, inv.Title)
	}
	for _, f := range r.Findings {
		_, _ = fmt.Fprintf(w, "  %s %s: %s\n", f.Invariant, f.Subject, f.Detail)
	}
	_, _ = fmt.Fprintln(w, r.Result)
}
