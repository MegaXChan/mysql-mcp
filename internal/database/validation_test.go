package database

import (
	"errors"
	"strings"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

func TestConstructorsRejectInvalidDependenciesAndDurations(t *testing.T) {
	// Scenario: startup wiring supplies nil pools or negative durations.
	// Risk covered: invalid services fail during construction instead of
	// panicking later in an MCP request.
	db, _ := newMockDatabase(t)
	tests := []struct {
		name string
		call func() error
	}{
		{name: "query nil database", call: func() error { _, err := NewQueryExecutor(nil, Limits{}); return err }},
		{name: "command nil database", call: func() error { _, err := NewCommandExecutor(nil, 0); return err }},
		{name: "command negative timeout", call: func() error { _, err := NewCommandExecutor(db, -time.Second); return err }},
		{name: "metadata nil database", call: func() error { _, err := NewMetadataService(nil, 0); return err }},
		{name: "metadata negative timeout", call: func() error { _, err := NewMetadataService(db, -time.Second); return err }},
		{name: "metadata non-positive rows", call: func() error { _, err := NewMetadataServiceWithMaxRows(db, time.Second, 0); return err }},
		{name: "admin nil database", call: func() error { _, err := NewAdminService(nil, 0); return err }},
		{name: "admin negative timeout", call: func() error { _, err := NewAdminService(db, -time.Second); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("constructor error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestFunctionServiceValidatesPolicyConfiguration(t *testing.T) {
	// Scenario: stored-function allow-list configuration is malformed.
	// Risk covered: invalid effects, duplicates, and write policies without a
	// writer pool cannot create a partially authorized service.
	db, _ := newMockDatabase(t)
	tests := []struct {
		name     string
		policies []FunctionPolicy
	}{
		{name: "unknown effect", policies: []FunctionPolicy{{Schema: "app", Name: "fn", Effect: "maybe"}}},
		{name: "writer missing", policies: []FunctionPolicy{{Schema: "app", Name: "fn", Effect: FunctionEffectWrite}}},
		{name: "duplicate", policies: []FunctionPolicy{
			{Schema: "app", Name: "fn", Effect: FunctionEffectRead},
			{Schema: "app", Name: "fn", Effect: FunctionEffectRead},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewFunctionService(db, nil, test.policies, Limits{}); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("NewFunctionService() error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestFunctionMetadataValidationFailsClosed(t *testing.T) {
	// Scenario: INFORMATION_SCHEMA returns incomplete or unknown routine
	// metadata. Risk covered: execution does not proceed when security/data-access
	// semantics or the parameter ordering cannot be proven.
	in := "IN"
	out := "OUT"
	validReturn := &FunctionParameter{OrdinalPosition: 0}
	tests := []struct {
		name        string
		description FunctionDescription
		wantKind    error
	}{
		{name: "missing return", description: FunctionDescription{Function: FunctionInfo{SecurityType: "INVOKER", SQLDataAccess: "NO SQL"}}, wantKind: ErrInvalidArgument},
		{name: "unknown security", description: FunctionDescription{Function: FunctionInfo{SecurityType: "MYSTERY", SQLDataAccess: "NO SQL"}, Return: validReturn}, wantKind: ErrPolicyDenied},
		{name: "unknown access", description: FunctionDescription{Function: FunctionInfo{SecurityType: "INVOKER", SQLDataAccess: "MYSTERY"}, Return: validReturn}, wantKind: ErrPolicyDenied},
		{name: "non-contiguous ordinal", description: FunctionDescription{Function: FunctionInfo{SecurityType: "INVOKER", SQLDataAccess: "NO SQL"}, Return: validReturn, Parameters: []FunctionParameter{{OrdinalPosition: 2, Mode: &in}}}, wantKind: ErrInvalidArgument},
		{name: "out parameter", description: FunctionDescription{Function: FunctionInfo{SecurityType: "INVOKER", SQLDataAccess: "NO SQL"}, Return: validReturn, Parameters: []FunctionParameter{{OrdinalPosition: 1, Mode: &out}}}, wantKind: ErrInvalidArgument},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateFunctionMetadata(test.description); !errors.Is(err, test.wantKind) {
				t.Fatalf("validateFunctionMetadata() error = %v, want %v", err, test.wantKind)
			}
		})
	}
}

func TestDatabaseErrorPreservesCodeAndRedactsAccount(t *testing.T) {
	// Scenario: go-sql-driver/mysql supplies a structured EXECUTE denial.
	// Risk covered: callers can classify permission failures and retain code/state
	// while account host details are removed from the exposed message.
	driverErr := &mysql.MySQLError{
		Number:   1370,
		SQLState: [5]byte{'4', '2', '0', '0', '0'},
		Message:  "execute command denied to user 'mcp'@'10.0.0.1'",
	}
	err := wrapDatabaseError("call", driverErr)
	if !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("wrapDatabaseError() error = %v, want ErrPermissionDenied", err)
	}
	if strings.Contains(err.Error(), "10.0.0.1") || !strings.Contains(err.Error(), "1370") {
		t.Fatalf("wrapDatabaseError() = %v, want redacted message retaining code", err)
	}
}

func TestNilContextsAreRejected(t *testing.T) {
	// Scenario: an internal caller violates the context contract.
	// Risk covered: public APIs return typed errors rather than panicking in
	// context.WithTimeout or database/sql.
	db, _ := newMockDatabase(t)
	query, _ := NewQueryExecutor(db, Limits{})
	command, _ := NewCommandExecutor(db, time.Second)
	admin, _ := NewAdminService(db, time.Second)
	metadata, _ := NewMetadataService(db, time.Second)
	checks := []error{}
	_, err := query.Query(nil, "SELECT 1")
	checks = append(checks, err)
	_, err = query.QueryWithMaxRows(nil, 1, "SELECT 1")
	checks = append(checks, err)
	_, err = command.Exec(nil, "UPDATE t SET a = 1")
	checks = append(checks, err)
	_, err = command.ExecDDL(nil, "CREATE TABLE t (id INT)")
	checks = append(checks, err)
	checks = append(checks, admin.KillQuery(nil, 1))
	_, err = metadata.ListSchemas(nil)
	checks = append(checks, err)
	for index, check := range checks {
		if !errors.Is(check, ErrInvalidArgument) {
			t.Fatalf("nil-context check %d error = %v, want ErrInvalidArgument", index, check)
		}
	}
}
