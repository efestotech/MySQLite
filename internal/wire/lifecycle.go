package wire

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"errors"

	"github.com/go-mysql-org/go-mysql/mysql"

	"mysqlite/internal/errs"
)

// resetSession gives the connection the session of a new one (COM_RESET_CONNECTION,
// COM_CHANGE_USER): the open transaction is rolled back and the writer released,
// variables return to their global values, prepared statements are released and
// LAST_INSERT_ID() is 0. The caller sets the current database. `oracle: pending`.
func (h *handler) resetSession() {
	h.close()
	h.fresh()
	h.stmts = nil
	h.fixer.forgetStatements()
	h.lastInsertID, h.rowCount = 0, 0
	h.sink = nil
	h.diag, h.pendingDiag = nil, nil
	h.clock = statementClock{}
}

// resetConnection answers COM_RESET_CONNECTION: a reset session that keeps the user
// and the current database.
func (h *handler) resetConnection() error {
	db, _ := h.sess.Database()
	h.resetSession()
	if db != "" {
		// A database dropped meanwhile leaves the session without one.
		_ = h.sess.Use(db)
	}
	current, _ := h.sess.Database()
	h.procs.end(h.proc, current)
	h.syncStatus()
	return nil
}

// changeUserRequest is the part of COM_CHANGE_USER mysqlite uses.
type changeUserRequest struct {
	user, database string
}

// parseChangeUser reads COM_CHANGE_USER's payload after the command byte: user,
// auth response (length-prefixed with CLIENT_SECURE_CONNECTION), database. The
// character set, plugin name and attributes that follow are not used: the handshake's
// character set is not applied either, and the plugin is always mysql_native_password.
func parseChangeUser(data []byte, secure bool) (changeUserRequest, bool) {
	user, rest, ok := cutNUL(data)
	if !ok {
		return changeUserRequest{}, false
	}
	if secure {
		if len(rest) < 1 || len(rest) < 1+int(rest[0]) {
			return changeUserRequest{}, false
		}
		rest = rest[1+int(rest[0]):]
	} else if _, rest, ok = cutNUL(rest); !ok {
		return changeUserRequest{}, false
	}
	db, _, ok := cutNUL(rest)
	if !ok {
		// The database is the last field of a pre-4.1 packet; it may lack its NUL.
		db = rest
	}
	return changeUserRequest{user: string(user), database: string(db)}, true
}

func cutNUL(b []byte) (before, after []byte, ok bool) {
	for i, c := range b {
		if c == 0 {
			return b[:i], b[i+1:], true
		}
	}
	return nil, nil, false
}

// changeUser answers COM_CHANGE_USER (docs/03-protocol.md section 4). The client's
// scramble was computed with the handshake's salt, which go-mysql keeps private, so
// mysqlite always answers with an AuthSwitchRequest carrying a new salt, as MySQL does
// when the plugin changes, and checks the reply. A known or unknown user with a wrong
// password gets the same 1045 after the switch. On success the session is reset for
// the new user and database. A failure ends the connection. `oracle: pending`.
func (h *handler) changeUser(data []byte) error {
	req, ok := parseChangeUser(data, h.conn.HasCapability(mysql.CLIENT_SECURE_CONNECTION))
	if !ok {
		return h.endConnection(toMySQL(errs.MalformedPacket()))
	}
	salt, err := newSalt()
	if err != nil {
		return err
	}
	pkt := append(make([]byte, 4, 4+1+len(mysql.AUTH_NATIVE_PASSWORD)+1+len(salt)+1), mysql.EOF_HEADER)
	pkt = append(pkt, mysql.AUTH_NATIVE_PASSWORD...)
	pkt = append(pkt, 0)
	pkt = append(pkt, salt...)
	pkt = append(pkt, 0)
	if err := h.conn.WritePacket(pkt); err != nil {
		return err
	}
	// The reply is not a command: the statement fixer must not read it as one.
	h.fixer.active.Store(false)
	reply, err := h.conn.ReadPacket()
	h.fixer.active.Store(true)
	if err != nil {
		return err
	}
	password, known := h.users[req.user]
	if !known || !nativeMatches(reply, salt, password) {
		return h.endConnection(toMySQL(errs.AccessDenied(req.user, h.conn.RemoteAddr().String(), !isEmptyAuthData(reply))))
	}

	h.resetSession()
	h.account = req.user
	h.procs.rename(h.proc, req.user)
	h.log = h.log.With("user", req.user)
	if req.database != "" {
		if err := h.use(req.database); err != nil {
			return h.endConnection(err)
		}
	}
	db, _ := h.sess.Database()
	h.procs.end(h.proc, db)
	h.syncStatus()
	return nil
}

// endConnection sends err and closes the connection. go-mysql then writes an OK
// packet, which the response filter drops.
func (h *handler) endConnection(err error) error {
	var my *mysql.MyError
	if !errors.As(err, &my) {
		return err
	}
	if werr := h.conn.WriteValue(err); werr != nil {
		return werr
	}
	h.filter.dropNext()
	h.conn.Close()
	return nil
}

// nativeMatches checks a mysql_native_password reply to salt. An empty password
// expects an empty reply.
func nativeMatches(reply, salt []byte, password string) bool {
	if password == "" {
		return isEmptyAuthData(reply)
	}
	return subtle.ConstantTimeCompare(reply, mysql.CalcNativePassword(salt, []byte(password))) == 1
}

// newSalt returns 20 random bytes without NUL, the authentication data of
// mysql_native_password.
func newSalt() ([]byte, error) {
	salt := make([]byte, 20)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	for i, b := range salt {
		salt[i] = 1 + b%127 // no NUL, 7-bit like MySQL's salt
	}
	return salt, nil
}

// MySQL's COM_SET_OPTION values.
const (
	optionMultiStatementsOn  = 0
	optionMultiStatementsOff = 1
)

// setOption answers COM_SET_OPTION: multi-statements on or off for the session.
// go-mysql answers EOF when it returns nil, as MySQL does. `oracle: pending`.
func (h *handler) setOption(data []byte) error {
	if len(data) < 2 {
		return toMySQL(errs.MalformedPacket())
	}
	switch binary.LittleEndian.Uint16(data) {
	case optionMultiStatementsOn:
		h.multiStatements = true
	case optionMultiStatementsOff:
		h.multiStatements = false
	default:
		return toMySQL(errs.UnknownCommand())
	}
	return nil
}
