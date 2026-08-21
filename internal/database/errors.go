package database

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	mysql "github.com/go-sql-driver/mysql"
)

// Sentinel errors let transports map failures without matching human-readable
// database messages.
var (
	ErrInvalidArgument  = errors.New("invalid argument")
	ErrUnsupported      = errors.New("capability unsupported")
	ErrPermissionDenied = errors.New("database permission denied")
	ErrNotFound         = errors.New("database object not found")
	ErrPolicyDenied     = errors.New("database policy denied")
)

// ServiceError adds a stable category and operation while retaining a
// sanitized underlying error for diagnostics.
type ServiceError struct {
	Kind   error
	Op     string
	Detail string
	Cause  error
}

func (e *ServiceError) Error() string {
	parts := make([]string, 0, 3)
	if e.Op != "" {
		parts = append(parts, e.Op)
	}
	if e.Detail != "" {
		parts = append(parts, e.Detail)
	}
	if e.Kind != nil {
		parts = append(parts, e.Kind.Error())
	}
	// Cause is already converted to DBError by databaseError before a
	// permission ServiceError is constructed. Including that sanitized cause
	// preserves the numeric MySQL code and SQLSTATE needed for diagnostics.
	if e.Cause != nil {
		parts = append(parts, e.Cause.Error())
	}
	return strings.Join(parts, ": ")
}

func (e *ServiceError) Unwrap() error { return e.Cause }

func (e *ServiceError) Is(target error) bool {
	return target != nil && (target == e.Kind || errors.Is(e.Cause, target))
}

// DBError is safe to expose to an adapter. Message has credentials and
// account host information redacted; the original driver error is not stored.
type DBError struct {
	Code     uint16
	SQLState string
	Message  string
}

func (e *DBError) Error() string {
	if e.Code == 0 {
		return e.Message
	}
	if e.SQLState == "" {
		return fmt.Sprintf("mysql error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("mysql error %d (%s): %s", e.Code, e.SQLState, e.Message)
}

var (
	accountPattern  = regexp.MustCompile(`(?i)'[^']*'@'[^']*'`)
	passwordPattern = regexp.MustCompile(`(?i)(password\s*[=:]\s*)([^\s,;]+)`)
)

func sanitizeMessage(message string) string {
	message = accountPattern.ReplaceAllString(message, "'[redacted]'@'[redacted]'")
	return passwordPattern.ReplaceAllString(message, "${1}[redacted]")
}

func databaseError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var serviceErr *ServiceError
	if errors.As(err, &serviceErr) {
		return err
	}

	dbErr := &DBError{Message: sanitizeMessage(err.Error())}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		dbErr.Code = mysqlErr.Number
		dbErr.SQLState = strings.TrimRight(string(mysqlErr.SQLState[:]), "\x00")
		dbErr.Message = sanitizeMessage(mysqlErr.Message)
	}

	if isPermissionError(dbErr.Code, dbErr.Message) {
		return &ServiceError{Kind: ErrPermissionDenied, Cause: dbErr}
	}
	return dbErr
}

func wrapDatabaseError(op string, err error) error {
	if err == nil {
		return nil
	}
	converted := databaseError(err)
	if errors.Is(converted, context.Canceled) || errors.Is(converted, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", op, converted)
	}
	var serviceErr *ServiceError
	if errors.As(converted, &serviceErr) {
		if serviceErr.Op == "" {
			serviceErr.Op = op
		}
		return serviceErr
	}
	return fmt.Errorf("%s: %w", op, converted)
}

func isPermissionError(code uint16, message string) bool {
	switch code {
	case 1044, 1045, 1142, 1143, 1227, 1370, 1419:
		return true
	}
	message = strings.ToLower(message)
	return strings.Contains(message, "access denied") ||
		strings.Contains(message, "command denied") ||
		strings.Contains(message, "execute command denied") ||
		strings.Contains(message, "permission denied")
}

func unsupported(op, detail string) error {
	return &ServiceError{Kind: ErrUnsupported, Op: op, Detail: detail}
}

func invalid(op, detail string) error {
	return &ServiceError{Kind: ErrInvalidArgument, Op: op, Detail: detail}
}

func notFound(op, detail string) error {
	return &ServiceError{Kind: ErrNotFound, Op: op, Detail: detail}
}

func policyDenied(op, detail string) error {
	return &ServiceError{Kind: ErrPolicyDenied, Op: op, Detail: detail}
}
