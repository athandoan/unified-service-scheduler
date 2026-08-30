// Package allocator shared helpers: the periodic expired-HELD reaper.
package reaper

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Start deletes expired HELD occupations on THIS allocator every tick
// (write-path note: expired HELD rows still occupy GiST until deleted). The
// 23P01 retry path uses the same DELETE inline. TTL is fixed at 120s in SQL —
// no env override; the tick interval is only the loop cadence.
func Start(ctx context.Context, pool *pgxpool.Pool, table, name string) {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				tag, err := pool.Exec(ctx,
					`DELETE FROM `+table+` WHERE status = 'HELD' AND hold_expires_at <= now()`)
				if err != nil {
					log.Printf("%s: reaper: %v", name, err)
					continue
				}
				if tag.RowsAffected() > 0 {
					log.Printf("%s: reaper deleted %d expired HELD occupations", name, tag.RowsAffected())
				}
			}
		}
	}()
}
