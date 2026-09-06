package services

import (
	"errors"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func isSQLiteBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && (sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY || sqliteErr.Code()&0xff == sqlite3.SQLITE_LOCKED)
}
