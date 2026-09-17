package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestErrorClassNeverIncludesDriverText(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want string
	}{
		{nil, "none"}, {context.Canceled, "canceled"}, {context.DeadlineExceeded, "deadline"}, {sql.ErrTxDone, "transaction_closed"}, {errors.New("secret SQL Cookie path"), "unknown"},
	} {
		if got := ErrorClass(tt.err); got != tt.want {
			t.Fatalf("class=%s want=%s", got, tt.want)
		}
		if IsTransientWriteError(tt.err) {
			t.Fatal("non-contention error retried")
		}
	}
	db, err := Open(filepath.Join(t.TempDir(), "class.db"))
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	defer func() { _ = pool.Close() }()
	if err := db.Exec("CREATE TABLE private_fixture (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO private_fixture VALUES (1)").Error; err != nil {
		t.Fatal(err)
	}
	err = db.Exec("INSERT INTO private_fixture VALUES (1)").Error
	if ErrorClass(fmt.Errorf("wrapper: %w", err)) != "unique" || IsTransientWriteError(err) {
		t.Fatal("constraint classification lost")
	}
	if err := db.Exec("PRAGMA query_only=ON").Error; err != nil {
		t.Fatal(err)
	}
	err = db.Exec("INSERT INTO private_fixture VALUES (2)").Error
	if ErrorClass(err) != "readonly" {
		t.Fatalf("readonly class=%s", ErrorClass(err))
	}
}
