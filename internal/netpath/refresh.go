package netpath

import (
	"context"
	"fmt"
	"time"
)

// Refresh traces each host in turn, saves a Summary of every path it manages to
// measure, and returns the paths it measured along with one error per host that
// failed. One host failing never stops the others. Tracing is sequential on
// purpose: this is meant to run quietly in the background.
func Refresh(ctx context.Context, hosts []string, o Options, store Store, perHost time.Duration, now func() time.Time) ([]Path, []error) {
	if now == nil {
		now = time.Now
	}
	var paths []Path
	var errs []error
	var sums []Summary
	for _, h := range hosts {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		hctx, cancel := context.WithTimeout(ctx, perHost)
		p, err := Run(hctx, h, o)
		cancel()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		paths = append(paths, p)
		sums = append(sums, p.Summary(now()))
	}
	if len(sums) > 0 {
		if err := store.Save(sums...); err != nil {
			errs = append(errs, fmt.Errorf("saving %s: %w", store.Path, err))
		}
	}
	return paths, errs
}
