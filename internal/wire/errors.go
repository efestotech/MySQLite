package wire

import (
	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/errs"
)

// toMySQL converts an errs.Error to the go-mysql type. The library writes the ERR packet
// only when the returned error is exactly *mysql.MyError (type assertion in
// server/resp.go, not errors.As); anything else is sent as 1105 with its raw text.
func toMySQL(e *errs.Error) error {
	if e == nil {
		return nil
	}
	return &mysql.MyError{Code: e.Code, State: e.State, Message: e.Message}
}
