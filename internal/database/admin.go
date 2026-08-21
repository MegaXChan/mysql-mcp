package database

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

// AdminService intentionally exposes narrow, typed administration operations.
// It must not grow an arbitrary "execute admin SQL" method.
type AdminService struct {
	db      *sql.DB
	timeout time.Duration
}

func NewAdminService(db *sql.DB, timeout time.Duration) (*AdminService, error) {
	if db == nil {
		return nil, invalid("new admin service", "nil database")
	}
	if timeout < 0 {
		return nil, invalid("new admin service", "negative timeout")
	}
	if timeout == 0 {
		timeout = defaultQueryTimeout
	}
	return &AdminService{db: db, timeout: timeout}, nil
}

// KillQuery cancels the statement currently running on connectionID without
// terminating the connection. MySQL does not accept a placeholder in KILL, so
// the only generated fragment is a base-10 rendering of a validated uint64.
func (s *AdminService) KillQuery(ctx context.Context, connectionID uint64) error {
	if connectionID == 0 {
		return invalid("kill query", "connection id must be positive")
	}
	execContext, cancel, err := serviceContext(ctx, s.timeout, "kill query")
	if err != nil {
		return err
	}
	defer cancel()
	statement := "KILL QUERY " + strconv.FormatUint(connectionID, 10)
	if _, err := s.db.ExecContext(execContext, statement); err != nil {
		return wrapDatabaseError("kill query", err)
	}
	return nil
}
