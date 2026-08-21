package database

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// CommandExecutor executes caller-authorized DML and DDL statements. It does
// not classify SQL: the Vitess-based policy layer above this package owns that
// decision. Values are passed only as driver arguments and are never joined
// into statement text here.
type CommandExecutor struct {
	db      *sql.DB
	timeout time.Duration
}

const (
	// CommandModeTransaction means the service explicitly began and committed a
	// read-write transaction. Transactional guarantees still depend on the
	// storage engines touched by the statement.
	CommandModeTransaction = "transaction"
	// CommandModeMySQLImplicitCommit identifies DDL executed directly because
	// MySQL implicitly commits most DDL before and after execution.
	CommandModeMySQLImplicitCommit = "mysql_implicit_commit"
)

// CommandResult summarizes a successfully executed command and makes its
// transaction model explicit to MCP clients.
type CommandResult struct {
	ExecutionMode         string        `json:"execution_mode"`
	RowsAffected          int64         `json:"rows_affected,omitempty"`
	RowsAffectedAvailable bool          `json:"rows_affected_available"`
	LastInsertID          int64         `json:"last_insert_id,omitempty"`
	LastInsertIDAvailable bool          `json:"last_insert_id_available"`
	Elapsed               time.Duration `json:"-"`
	ElapsedMillis         int64         `json:"elapsed_ms"`
}

func NewCommandExecutor(db *sql.DB, timeout time.Duration) (*CommandExecutor, error) {
	if db == nil {
		return nil, invalid("new command executor", "nil database")
	}
	if timeout < 0 {
		return nil, invalid("new command executor", "negative timeout")
	}
	if timeout == 0 {
		timeout = defaultQueryTimeout
	}
	return &CommandExecutor{db: db, timeout: timeout}, nil
}

// Exec runs one statement in a read-write transaction and commits only after
// result metadata has been read successfully. Every pre-commit failure is
// followed by a best-effort rollback.
func (e *CommandExecutor) Exec(ctx context.Context, statement string, args ...any) (result CommandResult, err error) {
	started := time.Now()
	result.ExecutionMode = CommandModeTransaction
	defer func() {
		result.Elapsed = time.Since(started)
		result.ElapsedMillis = result.Elapsed.Milliseconds()
	}()

	if ctx == nil {
		return result, invalid("execute command", "nil context")
	}
	if strings.TrimSpace(statement) == "" {
		return result, invalid("execute command", "empty statement")
	}

	execContext, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	tx, err := e.db.BeginTx(execContext, &sql.TxOptions{ReadOnly: false})
	if err != nil {
		return result, wrapDatabaseError("begin read-write transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	sqlResult, err := tx.ExecContext(execContext, statement, args...)
	if err != nil {
		return result, wrapDatabaseError("execute command", err)
	}
	result.RowsAffected, err = sqlResult.RowsAffected()
	if err != nil {
		return result, wrapDatabaseError("read affected row count", err)
	}
	result.RowsAffectedAvailable = true
	if lastInsertID, lastIDErr := sqlResult.LastInsertId(); lastIDErr == nil {
		result.LastInsertID = lastInsertID
		result.LastInsertIDAvailable = true
	}
	if err := tx.Commit(); err != nil {
		return result, wrapDatabaseError("commit command", err)
	}
	return result, nil
}

// ExecDDL executes one DDL statement without wrapping it in a transaction.
// MySQL 5.7 and 8.x implicitly commit most DDL, so a surrounding database/sql
// transaction would suggest rollback semantics that MySQL cannot provide. The
// policy layer must classify and authorize the statement before calling this
// method.
func (e *CommandExecutor) ExecDDL(ctx context.Context, statement string, args ...any) (result CommandResult, err error) {
	started := time.Now()
	result.ExecutionMode = CommandModeMySQLImplicitCommit
	defer func() {
		result.Elapsed = time.Since(started)
		result.ElapsedMillis = result.Elapsed.Milliseconds()
	}()

	if ctx == nil {
		return result, invalid("execute DDL", "nil context")
	}
	if strings.TrimSpace(statement) == "" {
		return result, invalid("execute DDL", "empty statement")
	}

	execContext, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	sqlResult, err := e.db.ExecContext(execContext, statement, args...)
	if err != nil {
		return result, wrapDatabaseError("execute DDL", err)
	}

	// The DDL has already taken effect when ExecContext returns successfully.
	// Result metadata is optional and must not turn an applied schema change into
	// a reported execution failure.
	if rowsAffected, rowsErr := sqlResult.RowsAffected(); rowsErr == nil {
		result.RowsAffected = rowsAffected
		result.RowsAffectedAvailable = true
	}
	if lastInsertID, lastIDErr := sqlResult.LastInsertId(); lastIDErr == nil {
		result.LastInsertID = lastInsertID
		result.LastInsertIDAvailable = true
	}
	return result, nil
}
