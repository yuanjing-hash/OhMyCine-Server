package database

import (
	"context"
	"database/sql"
	"errors"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrorClass is deliberately finite: never expose SQL, paths or driver text.
func ErrorClass(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	if errors.Is(err, sql.ErrTxDone) {
		return "transaction_closed"
	}
	var e *sqlite.Error
	if !errors.As(err, &e) {
		return "unknown"
	}
	switch e.Code() {
	case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
		return "foreign_key"
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return "unique"
	}
	switch e.Code() & 0xff {
	case sqlite3.SQLITE_BUSY, sqlite3.SQLITE_LOCKED:
		return "busy"
	case sqlite3.SQLITE_CONSTRAINT:
		return "constraint"
	case sqlite3.SQLITE_INTERRUPT:
		return "interrupted"
	case sqlite3.SQLITE_IOERR:
		return "io"
	case sqlite3.SQLITE_FULL:
		return "full"
	case sqlite3.SQLITE_READONLY:
		return "readonly"
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return "corrupt"
	default:
		return "unknown"
	}
}

func IsTransientWriteError(err error) bool { return ErrorClass(err) == "busy" }
