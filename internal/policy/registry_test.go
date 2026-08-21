package policy

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestRegistryLifecycle verifies duplicate protection, atomic replacement, and
// removal. A failed replacement must leave the previous parser available so a
// bad configuration reload cannot take a healthy datasource offline.
func TestRegistryLifecycle(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()

	if err := registry.RegisterDatasource("primary", "5.7.44"); err != nil {
		t.Fatalf("RegisterDatasource() error = %v", err)
	}
	if err := registry.RegisterDatasource("primary", "8.0.36"); !errors.Is(err, ErrDatasourceExists) {
		t.Fatalf("duplicate error = %v, want ErrDatasourceExists", err)
	}
	if err := registry.SetDatasource("primary", "8.0.36"); err != nil {
		t.Fatalf("SetDatasource() error = %v", err)
	}
	configured, err := registry.PolicyFor("primary")
	if err != nil {
		t.Fatalf("PolicyFor() error = %v", err)
	}
	if configured.MySQLServerVersion() != "8.0.36" {
		t.Fatalf("version after replacement = %q, want 8.0.36", configured.MySQLServerVersion())
	}

	if err := registry.SetDatasource("primary", "invalid"); err == nil {
		t.Fatal("SetDatasource() succeeded with an invalid version")
	}
	configured, err = registry.PolicyFor("primary")
	if err != nil || configured.MySQLServerVersion() != "8.0.36" {
		t.Fatalf("failed replacement changed healthy policy: policy=%v error=%v", configured, err)
	}

	if !registry.RemoveDatasource("primary") {
		t.Fatal("RemoveDatasource() = false, want true")
	}
	if registry.RemoveDatasource("primary") {
		t.Fatal("second RemoveDatasource() = true, want false")
	}
	if _, err := registry.PolicyFor("primary"); !errors.Is(err, ErrUnknownDatasource) {
		t.Fatalf("removed datasource error = %v, want ErrUnknownDatasource", err)
	}
}

// TestRegistryValidationErrors makes name handling explicit. Whitespace around
// a configured name is normalized, while an empty name is never a usable key.
func TestRegistryValidationErrors(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	if err := registry.RegisterDatasource("  ", "8.0.36"); !errors.Is(err, ErrEmptyDatasource) {
		t.Fatalf("empty name error = %v, want ErrEmptyDatasource", err)
	}
	if err := registry.RegisterDatasource(" reporting ", "8.0.36"); err != nil {
		t.Fatalf("RegisterDatasource() error = %v", err)
	}
	if _, err := registry.PolicyFor("reporting"); err != nil {
		t.Fatalf("trimmed PolicyFor() error = %v", err)
	}
	if _, err := (*Registry)(nil).PolicyFor("reporting"); !errors.Is(err, ErrUnknownDatasource) {
		t.Fatalf("nil registry error = %v, want ErrUnknownDatasource", err)
	}
	if err := registry.RegisterDatasource("bad-version", "invalid"); err == nil {
		t.Fatal("RegisterDatasource() accepted an invalid MySQL version")
	}
	if err := registry.SetDatasource(" ", "8.0.36"); !errors.Is(err, ErrEmptyDatasource) {
		t.Fatalf("SetDatasource() empty name error = %v", err)
	}
	if registry.RemoveDatasource(" ") {
		t.Fatal("RemoveDatasource() removed an empty datasource")
	}
	if _, err := registry.PolicyFor(" "); !errors.Is(err, ErrEmptyDatasource) {
		t.Fatalf("PolicyFor() empty name error = %v", err)
	}
	if _, err := registry.Classify("missing", "SELECT 1"); !errors.Is(err, ErrUnknownDatasource) {
		t.Fatalf("Classify() missing error = %v", err)
	}
}

// TestRegistryConcurrentAccess runs readers while policies are atomically
// replaced. It is primarily a race-test scenario: every observed policy must
// be complete, and no goroutine may see a partially initialized parser.
func TestRegistryConcurrentAccess(t *testing.T) {
	registry := NewRegistry()
	if err := registry.RegisterDatasource("shared", "5.7.44"); err != nil {
		t.Fatalf("RegisterDatasource() error = %v", err)
	}

	const readers = 24
	const iterations = 50
	start := make(chan struct{})
	errorsSeen := make(chan error, readers+1)
	var wait sync.WaitGroup

	for reader := 0; reader < readers; reader++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for iteration := 0; iteration < iterations; iteration++ {
				classification, err := registry.Classify("shared", "SELECT ABS(1)")
				if err != nil {
					errorsSeen <- err
					return
				}
				if classification.Class != ClassRead {
					errorsSeen <- fmt.Errorf("class = %q, want %q", classification.Class, ClassRead)
					return
				}
			}
		}()
	}

	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		for iteration := 0; iteration < iterations; iteration++ {
			version := "5.7.44"
			if iteration%2 == 0 {
				version = "8.0.36"
			}
			if err := registry.SetDatasource("shared", version); err != nil {
				errorsSeen <- err
				return
			}
		}
	}()

	close(start)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Errorf("concurrent registry operation: %v", err)
	}
}

// TestRegistryDelegates verifies every public wrapper resolves the datasource
// first and then preserves the underlying policy result. This protects service
// code from accidentally using a parser configured for another MySQL version.
func TestRegistryDelegates(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	if err := registry.RegisterDatasource("app", "8.0.36"); err != nil {
		t.Fatalf("RegisterDatasource() error = %v", err)
	}

	if _, err := registry.ValidateReadQuery("app", "SELECT 1"); err != nil {
		t.Fatalf("ValidateReadQuery() error = %v", err)
	}
	if _, err := registry.ValidateReadQueryForSchemas("app", "SELECT * FROM app.orders", "app", []string{"app"}); err != nil {
		t.Fatalf("ValidateReadQueryForSchemas() error = %v", err)
	}
	// Pattern-only restrictions use the same wrapper. This specifically proves
	// the variadic compatibility parameter is forwarded rather than discarded.
	if _, err := registry.ValidateReadQueryForSchemas("app", "SELECT * FROM orders_dev.orders", "", nil, []string{"*_dev"}); err != nil {
		t.Fatalf("ValidateReadQueryForSchemas() pattern error = %v", err)
	}
	if _, err := registry.ValidateExplain("app", "EXPLAIN SELECT 1"); err != nil {
		t.Fatalf("ValidateExplain() error = %v", err)
	}
	if classification, err := registry.ValidateCommand("app", "UPDATE app.orders SET id=1"); err != nil {
		t.Fatalf("ValidateCommand() error = %v", err)
	} else if classification.Class != ClassWrite {
		t.Fatalf("ValidateCommand().Class = %q, want %q", classification.Class, ClassWrite)
	}

	if _, err := registry.ValidateReadQuery("missing", "SELECT 1"); !errors.Is(err, ErrUnknownDatasource) {
		t.Fatalf("ValidateReadQuery() missing error = %v", err)
	}
	if _, err := registry.ValidateReadQueryForSchemas("missing", "SELECT 1", "app", []string{"app"}); !errors.Is(err, ErrUnknownDatasource) {
		t.Fatalf("ValidateReadQueryForSchemas() missing error = %v", err)
	}
	if _, err := registry.ValidateExplain("missing", "EXPLAIN SELECT 1"); !errors.Is(err, ErrUnknownDatasource) {
		t.Fatalf("ValidateExplain() missing error = %v", err)
	}
	if _, err := registry.ValidateCommand("missing", "UPDATE app.orders SET id=1"); !errors.Is(err, ErrUnknownDatasource) {
		t.Fatalf("ValidateCommand() missing error = %v", err)
	}
}
