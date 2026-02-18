package cluster

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// mockConnector simulates PG connectivity for tests.
type mockConnector struct {
	connectErr error
	conn       *mockConn
}

func (c *mockConnector) Connect(ctx context.Context, dsn string) (PGConn, error) {
	if c.connectErr != nil {
		return nil, c.connectErr
	}
	if c.conn == nil {
		c.conn = &mockConn{isInRecovery: false}
	}
	return c.conn, nil
}

type mockConn struct {
	isInRecovery bool
	queryErr     error
	closed       bool
}

func (c *mockConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &mockRow{isInRecovery: c.isInRecovery, err: c.queryErr}
}

func (c *mockConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if c.queryErr != nil {
		return nil, c.queryErr
	}
	return &mockRows{}, nil
}

func (c *mockConn) Close(ctx context.Context) error {
	c.closed = true
	return nil
}

type mockRow struct {
	isInRecovery bool
	err          error
}

func (r *mockRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > 0 {
		switch v := dest[0].(type) {
		case *bool:
			*v = r.isInRecovery
		case *int:
			*v = 5
		case **int64:
			val := int64(0)
			*v = &val
		}
	}
	return nil
}

type mockRows struct {
	called bool
}

func (r *mockRows) Close()                                         {}
func (r *mockRows) Err() error                                     { return nil }
func (r *mockRows) CommandTag() pgconn.CommandTag                  { return pgconn.CommandTag{} }
func (r *mockRows) FieldDescriptions() []pgconn.FieldDescription   { return nil }
func (r *mockRows) Next() bool                                     { return false }
func (r *mockRows) Scan(dest ...any) error                         { return nil }
func (r *mockRows) Values() ([]any, error)                         { return nil, nil }
func (r *mockRows) RawValues() [][]byte                            { return nil }
func (r *mockRows) Conn() *pgx.Conn                                { return nil }

func TestHealthChecker_ConnectFailure(t *testing.T) {
	connector := &mockConnector{connectErr: fmt.Errorf("connection refused")}
	hc := NewHealthCheckerWithConnector("postgres://localhost/test", 2*time.Second, connector, testLogger())

	hc.check(context.Background())

	status := hc.Status()
	if status.Healthy {
		t.Error("expected unhealthy when connection fails")
	}
	if status.AcceptingConns {
		t.Error("expected not accepting connections")
	}
	if status.Error == "" {
		t.Error("expected error message")
	}
}

func TestHealthChecker_PrimaryHealth(t *testing.T) {
	conn := &mockConn{isInRecovery: false}
	connector := &mockConnector{conn: conn}
	hc := NewHealthCheckerWithConnector("postgres://localhost/test", 2*time.Second, connector, testLogger())

	hc.check(context.Background())

	status := hc.Status()
	if !status.Healthy {
		t.Error("expected healthy")
	}
	if status.IsInRecovery {
		t.Error("expected not in recovery for primary")
	}
	if !status.AcceptingConns {
		t.Error("expected accepting connections")
	}
}

func TestHealthChecker_ReplicaHealth(t *testing.T) {
	conn := &mockConn{isInRecovery: true}
	connector := &mockConnector{conn: conn}
	hc := NewHealthCheckerWithConnector("postgres://localhost/test", 2*time.Second, connector, testLogger())

	hc.check(context.Background())

	status := hc.Status()
	if !status.Healthy {
		t.Error("expected healthy")
	}
	if !status.IsInRecovery {
		t.Error("expected in recovery for replica")
	}
}

func TestHealthChecker_StatusThreadSafe(t *testing.T) {
	connector := &mockConnector{conn: &mockConn{}}
	hc := NewHealthCheckerWithConnector("postgres://localhost/test", 2*time.Second, connector, testLogger())

	// Concurrent reads/writes shouldn't panic.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			hc.check(context.Background())
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		_ = hc.Status()
	}
	<-done
}

func TestHealthChecker_Ready(t *testing.T) {
	connector := &mockConnector{conn: &mockConn{}}
	hc := NewHealthCheckerWithConnector("postgres://localhost/test", 2*time.Second, connector, testLogger())

	// Ready channel should not be closed initially.
	select {
	case <-hc.Ready():
		t.Error("ready channel should not be closed before first check")
	default:
		// Expected.
	}

	// Perform a health check.
	hc.check(context.Background())

	// Ready channel should now be closed.
	select {
	case <-hc.Ready():
		// Expected.
	case <-time.After(100 * time.Millisecond):
		t.Error("ready channel should be closed after first check")
	}
}
