package wire

import (
	"fmt"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
)

// nativeOnlyProvider accepts only mysql_native_password (docs/03-protocol.md section 3).
// Any other plugin proposed by the client is answered by go-mysql with an
// AuthSwitchRequest to mysql_native_password, because every credential we return
// names that plugin.
type nativeOnlyProvider struct {
	users *authHandler
}

func (nativeOnlyProvider) Validate(authPluginName string) bool {
	return authPluginName == mysql.AUTH_NATIVE_PASSWORD
}

func (p nativeOnlyProvider) Authenticate(c *server.Conn, authPluginName string, clientAuthData []byte) error {
	if authPluginName != mysql.AUTH_NATIVE_PASSWORD {
		return fmt.Errorf("%w: plugin %q refused", server.ErrAccessDenied, authPluginName)
	}
	// go-mysql v1.16.0 panics in DecodePasswordHex when a client sends a non-empty
	// scramble for a user whose password is empty, which would crash the process.
	// Reject that case before delegating (docs/agent-notes/api-go-mysql.md, W5).
	if password, ok := p.users.users[c.GetUser()]; ok && password == "" && !isEmptyAuthData(clientAuthData) {
		return server.ErrAccessDenied
	}
	var def server.DefaultAuthenticationProvider
	return def.Authenticate(c, authPluginName, clientAuthData)
}

// isEmptyAuthData mirrors go-mysql's isEmptyPassword: some clients send no bytes,
// libmysql sends a single NUL.
func isEmptyAuthData(data []byte) bool {
	return len(data) == 0 || (len(data) == 1 && data[0] == 0)
}

// authHandler serves the users from the configuration file. It is read-only after
// construction, so it is safe for concurrent use without locking.
type authHandler struct {
	users map[string]string
}

func newAuthHandler(users map[string]string) *authHandler {
	copied := make(map[string]string, len(users))
	for name, password := range users {
		copied[name] = password
	}
	return &authHandler{users: copied}
}

func (a *authHandler) GetCredential(username string) (server.Credential, bool, error) {
	password, ok := a.users[username]
	if !ok {
		// go-mysql reports a missing credential as error 1449 ("definer does not exist").
		// Wrapping ErrAccessDenied makes the handshake send 1045, as MySQL does
		// (docs/agent-notes/api-go-mysql.md, W1).
		return server.Credential{}, false, fmt.Errorf("%w: unknown user", server.ErrAccessDenied)
	}
	// An empty password must be passed as [""], an empty slice means "no user" (W4).
	return server.Credential{
		Passwords:      []string{password},
		AuthPluginName: mysql.AUTH_NATIVE_PASSWORD,
	}, true, nil
}

func (a *authHandler) OnAuthSuccess(conn *server.Conn) error {
	// go-mysql starts every connection with status 0; clients read AUTOCOMMIT from
	// OK/EOF packets to decide whether to send COMMIT (W3, docs/03-protocol.md section 7).
	conn.SetStatus(mysql.SERVER_STATUS_AUTOCOMMIT)
	return nil
}

func (a *authHandler) OnAuthFailure(*server.Conn, error) {}
