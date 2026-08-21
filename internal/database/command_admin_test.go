package database

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCommandExecutorCommitsParameterizedStatement(t *testing.T) {
	// Scenario: the upper policy layer authorizes an INSERT with bound values.
	// Risk covered: this service preserves placeholders, reports MySQL result
	// metadata, and commits exactly once after successful execution.
	db, mock := newMockDatabase(t)
	executor, err := NewCommandExecutor(db, time.Second)
	if err != nil {
		t.Fatalf("NewCommandExecutor() error = %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO users(name) VALUES (?)")).
		WithArgs("O'Reilly").
		WillReturnResult(sqlmock.NewResult(42, 1))
	mock.ExpectCommit()

	result, err := executor.Exec(context.Background(), "INSERT INTO users(name) VALUES (?)", "O'Reilly")
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if result.ExecutionMode != CommandModeTransaction || !result.RowsAffectedAvailable ||
		result.RowsAffected != 1 || !result.LastInsertIDAvailable || result.LastInsertID != 42 {
		t.Fatalf("Exec() = %#v, want rows=1 lastInsertID=42", result)
	}
	assertExpectations(t, mock)
}

func TestCommandExecutorRunsDDLWithoutMisleadingTransaction(t *testing.T) {
	// Scenario: the policy layer authorizes an ordinary CREATE TABLE.
	// Risk covered: MySQL implicitly commits DDL, so the service must not issue a
	// surrounding BEGIN/COMMIT pair that falsely suggests rollback semantics.
	db, mock := newMockDatabase(t)
	executor, err := NewCommandExecutor(db, time.Second)
	if err != nil {
		t.Fatalf("NewCommandExecutor() error = %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta("CREATE TABLE app.audit_log (id BIGINT PRIMARY KEY)")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	result, err := executor.ExecDDL(context.Background(), "CREATE TABLE app.audit_log (id BIGINT PRIMARY KEY)")
	if err != nil {
		t.Fatalf("ExecDDL() error = %v", err)
	}
	if result.ExecutionMode != CommandModeMySQLImplicitCommit || !result.RowsAffectedAvailable {
		t.Fatalf("ExecDDL() = %#v, want explicit mysql_implicit_commit result", result)
	}
	assertExpectations(t, mock)
}

func TestCommandExecutorDDLDoesNotReportAppliedChangeAsMetadataFailure(t *testing.T) {
	// Scenario: MySQL accepts DDL but its driver result cannot expose affected
	// row metadata. Risk covered: the schema change has already happened and
	// cannot be rolled back, so the service reports success with availability
	// flags instead of encouraging an unsafe blind retry.
	db, mock := newMockDatabase(t)
	executor, err := NewCommandExecutor(db, time.Second)
	if err != nil {
		t.Fatalf("NewCommandExecutor() error = %v", err)
	}
	mock.ExpectExec("ALTER TABLE app.users").
		WillReturnResult(sqlmock.NewErrorResult(errors.New("metadata unavailable")))

	result, err := executor.ExecDDL(context.Background(), "ALTER TABLE app.users ADD COLUMN note TEXT")
	if err != nil {
		t.Fatalf("ExecDDL() error = %v, want applied DDL success", err)
	}
	if result.RowsAffectedAvailable || result.LastInsertIDAvailable {
		t.Fatalf("ExecDDL() = %#v, want unavailable metadata flags", result)
	}
	assertExpectations(t, mock)
}

func TestCommandExecutorRollsBackOnExecutionFailure(t *testing.T) {
	// Scenario: MySQL rejects a DML statement after BeginTx succeeds.
	// Risk covered: no failed transaction leaks into the connection pool and no
	// commit is attempted after an execution error.
	db, mock := newMockDatabase(t)
	executor, err := NewCommandExecutor(db, time.Second)
	if err != nil {
		t.Fatalf("NewCommandExecutor() error = %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE users").WithArgs("duplicate").WillReturnError(errors.New("duplicate key"))
	mock.ExpectRollback()

	_, err = executor.Exec(context.Background(), "UPDATE users SET name = ?", "duplicate")
	if err == nil {
		t.Fatal("Exec() error = nil, want execution error")
	}
	assertExpectations(t, mock)
}

func TestCommandExecutorRollsBackOnCommitFailure(t *testing.T) {
	// Scenario: command execution succeeds but the transaction commit fails.
	// Risk covered: callers are not told a write succeeded when durability is
	// unknown; database/sql marks the transaction done after Commit is attempted.
	db, mock := newMockDatabase(t)
	executor, err := NewCommandExecutor(db, time.Second)
	if err != nil {
		t.Fatalf("NewCommandExecutor() error = %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectExec("DELETE FROM users").WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(errors.New("connection lost during commit"))

	_, err = executor.Exec(context.Background(), "DELETE FROM users WHERE id = ?", int64(7))
	if err == nil {
		t.Fatal("Exec() error = nil, want commit error")
	}
	assertExpectations(t, mock)
}

func TestAdminServiceKillQueryUsesOnlyValidatedDecimalID(t *testing.T) {
	// Scenario: an operator cancels query 18446744073709551615.
	// Risk covered: KILL does not accept placeholders, so the implementation may
	// generate syntax only from a typed uint64—not arbitrary user text.
	db, mock := newMockDatabase(t)
	service, err := NewAdminService(db, time.Second)
	if err != nil {
		t.Fatalf("NewAdminService() error = %v", err)
	}
	mock.ExpectExec(regexp.QuoteMeta("KILL QUERY 18446744073709551615")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := service.KillQuery(context.Background(), ^uint64(0)); err != nil {
		t.Fatalf("KillQuery() error = %v", err)
	}
	assertExpectations(t, mock)
}

func TestAdminServiceRejectsZeroConnectionIDWithoutDatabaseCall(t *testing.T) {
	// Scenario: the adapter supplies the zero value for a missing connection ID.
	// Risk covered: malformed requests are rejected before any admin statement is
	// sent to MySQL.
	db, mock := newMockDatabase(t)
	service, err := NewAdminService(db, time.Second)
	if err != nil {
		t.Fatalf("NewAdminService() error = %v", err)
	}

	err = service.KillQuery(context.Background(), 0)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("KillQuery() error = %v, want ErrInvalidArgument", err)
	}
	assertExpectations(t, mock)
}
