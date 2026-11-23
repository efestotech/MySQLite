// Command adminer drives Adminer over HTTP through the client scenario of
// docs/07-testing.md section 6: log in, create a database and a table, list databases
// and tables, show a table's structure, browse it, run a SELECT with a WHERE on a
// string, insert a row through the edit form, update and delete through the SQL page,
// log out.
//
//	go run ./test/clients/adminer -url http://127.0.0.1:8080/ -server mysqlite:3307
//
// -url is Adminer's address (the docker adminer service, or adminer.php under
// `php -S`); -server is the mysqlite address as Adminer reaches it. Prints one line per
// step and PASS at the end; exits 1 on the first failure.
package main

import (
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

var (
	reToken = regexp.MustCompile(`name=["']token["'] value=["']([^"']+)["']`)
	reError = regexp.MustCompile(`(?s)class=['"]error['"][^>]*>(.*?)</div>`)
	reTag   = regexp.MustCompile(`<[^>]+>`)
)

type session struct {
	client *http.Client
	base   string
	params url.Values // server, username, db
	token  string
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8080/", "Adminer URL")
	server := flag.String("server", "127.0.0.1:3307", "mysqlite address as Adminer reaches it")
	user := flag.String("user", "test", "user")
	password := flag.String("password", "test", "password")
	db := flag.String("db", "clients_adminer", "database the scenario creates and uses")
	flag.Parse()
	if err := run(*base, *server, *user, *password, *db); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}

func run(base, server, user, password, db string) error {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	s := &session{client: &http.Client{Jar: jar, Timeout: 30 * time.Second}, base: base,
		params: url.Values{"server": {server}, "username": {user}}}

	// Adminer may still be starting. The bare URL: with server parameters and no session
	// Adminer answers 403.
	var page string
	for i := 0; ; i++ {
		page, err = s.read(s.client.Get(base))
		if err == nil || i == 30 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("login page: %w", err)
	}
	if err := step("login page", strings.Contains(page, "auth[username]"), page); err != nil {
		return err
	}
	page, err = s.post(url.Values{}, url.Values{
		"auth[driver]": {"server"}, "auth[server]": {server}, "auth[username]": {user},
		"auth[password]": {password}, "auth[db]": {""},
	}, false)
	if err := check("login", page, err, `name=["']logout["']`); err != nil {
		return err
	}

	page, err = s.sql("CREATE DATABASE IF NOT EXISTS " + db)
	if err := check("create database", page, err, `Query executed OK`); err != nil {
		return err
	}
	page, err = s.get(url.Values{})
	if err := check("list databases", page, err, `>`+regexp.QuoteMeta(db)+`<`); err != nil {
		return err
	}

	s.params.Set("db", db)
	page, err = s.sql("DROP TABLE IF EXISTS people;\nCREATE TABLE people (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50) NOT NULL, city VARCHAR(50));\n" +
		"INSERT INTO people (name, city) VALUES ('Ada', 'Roma'), ('Bea', 'Milano'), ('Cesare', 'Roma');")
	if err := check("create table", page, err, `Query executed OK`); err != nil {
		return err
	}
	page, err = s.get(url.Values{})
	if err := check("list tables", page, err, `>people<`); err != nil {
		return err
	}
	page, err = s.get(url.Values{"table": {"people"}})
	if err := check("describe table", page, err, `>id<`, `>name<`, `>city<`, `varchar\(50\)`); err != nil {
		return err
	}
	page, err = s.get(url.Values{"select": {"people"}})
	if err := check("browse", page, err, `>Ada<`, `>Bea<`, `>Cesare<`); err != nil {
		return err
	}
	page, err = s.sql("SELECT name FROM people WHERE city = 'Roma' ORDER BY id")
	if err := check("select where string", page, err, `>Ada<`, `>Cesare<`); err != nil {
		return err
	}
	if err := step("select where string excludes", !strings.Contains(page, ">Bea<"), page); err != nil {
		return err
	}

	// Insert through the edit form, as a user would.
	page, err = s.get(url.Values{"edit": {"people"}})
	if err := check("insert form", page, err, `fields\[name\]`); err != nil {
		return err
	}
	page, err = s.post(url.Values{"edit": {"people"}}, url.Values{
		"fields[name]": {"Dora"}, "fields[city]": {"Napoli"}, "save": {"Save"},
	}, true)
	if err := check("insert", page, err, `>Dora<`); err != nil {
		return err
	}

	page, err = s.sql("UPDATE people SET city = 'Torino' WHERE name = 'Bea'")
	if err := check("update", page, err, `Query executed OK, 1 row affected`); err != nil {
		return err
	}
	page, err = s.sql("DELETE FROM people WHERE city = 'Roma'")
	if err := check("delete", page, err, `Query executed OK, 2 rows affected`); err != nil {
		return err
	}
	page, err = s.sql("SELECT name, city FROM people ORDER BY id")
	if err := check("rows left", page, err, `>Bea<`, `>Torino<`, `>Dora<`); err != nil {
		return err
	}
	page, err = s.sql("DROP TABLE people")
	if err := check("drop table", page, err, `Query executed OK`); err != nil {
		return err
	}
	page, err = s.post(url.Values{}, url.Values{"logout": {"Logout"}}, true)
	return check("logout", page, err, `auth\[username\]`)
}

// get loads Adminer with the session's parameters plus extra.
func (s *session) get(extra url.Values) (string, error) {
	resp, err := s.client.Get(s.url(extra))
	return s.read(resp, err)
}

// post submits a form; withToken adds the CSRF token of the last page.
func (s *session) post(extra, form url.Values, withToken bool) (string, error) {
	if withToken {
		form.Set("token", s.token)
	}
	resp, err := s.client.PostForm(s.url(extra), form)
	return s.read(resp, err)
}

// sql runs statements through Adminer's SQL command page.
func (s *session) sql(query string) (string, error) {
	if _, err := s.get(url.Values{"sql": {""}}); err != nil {
		return "", err
	}
	return s.post(url.Values{"sql": {""}}, url.Values{"query": {query}, "limit": {""}, "error_stops": {"1"}}, true)
}

func (s *session) url(extra url.Values) string {
	q := url.Values{}
	for k, v := range s.params {
		q[k] = v
	}
	for k, v := range extra {
		q[k] = v
	}
	if len(q) == 0 {
		return s.base
	}
	// Adminer reads "sql=" and "edit=people" as present keys; Encode keeps them.
	return s.base + "?" + q.Encode()
}

func (s *session) read(resp *http.Response, err error) (string, error) {
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		msg := ""
		if m := reError.FindStringSubmatch(string(b)); m != nil {
			msg = ": " + text(m[1])
		}
		return string(b), fmt.Errorf("HTTP %d%s", resp.StatusCode, msg)
	}
	page := string(b)
	if m := reToken.FindStringSubmatch(page); m != nil {
		s.token = m[1]
	}
	return page, nil
}

// check fails the step on a request error, an Adminer error message, or a pattern the
// page lacks.
func check(name, page string, err error, patterns ...string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if m := reError.FindStringSubmatch(page); m != nil {
		return fmt.Errorf("%s: Adminer error: %s", name, text(m[1]))
	}
	for _, p := range patterns {
		if !regexp.MustCompile(p).MatchString(page) {
			return fmt.Errorf("%s: page lacks %s", name, p)
		}
	}
	fmt.Println("ok", name)
	return nil
}

func step(name string, ok bool, page string) error {
	if !ok {
		return fmt.Errorf("%s: unexpected page: %.300s", name, text(page))
	}
	fmt.Println("ok", name)
	return nil
}

func text(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(reTag.ReplaceAllString(s, " "))), " ")
}
