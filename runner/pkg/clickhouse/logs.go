package clickhouse

import (
	"context"
	"errors"
	"fmt"
)

// LogsTablePresent reports whether database.table exists, which is all the
// agent's ClickHouse logs health claims: ClickHouse answers and the table that
// logs are written to is there. It reads system.tables only, never the logs,
// so it says nothing about whether logs are still arriving. A user without
// access to the table gets no row from system.tables either.
func LogsTablePresent(ctx context.Context, c *Client, database, table string) (bool, error) {
	if c == nil {
		return false, errors.New("clickhouse: not configured")
	}
	q := fmt.Sprintf(
		"SELECT 1 FROM system.tables WHERE database = '%s' AND name = '%s' LIMIT 1",
		escapeLiteral(database), escapeLiteral(table),
	)
	res, err := c.Query(ctx, q, nil)
	if err != nil {
		return false, err
	}
	if res.Error != nil {
		return false, errors.New(*res.Error)
	}
	return len(res.Data) > 0, nil
}
