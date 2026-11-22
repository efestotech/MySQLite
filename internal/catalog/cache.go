package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	"mysqlite/internal/meta"
)

// Cache keeps table descriptions per database (docs/01-architecture.md rule 6: the one
// shared mutable state besides the session manager).
//
// Invalidation uses SQLite's schema_version, which every DDL statement increments, from
// any connection or process. It is read on the connection the statement runs on, so a
// description always matches the schema that connection sees: a transaction that
// started before another connection's DDL keeps its own version, and the cache follows
// whichever version was read last.
//
// Cached tables are shared: callers must not modify them.
type Cache struct {
	mu  sync.Mutex
	dbs map[string]*dbCache
}

type dbCache struct {
	version int64
	tables  map[string]*meta.Table // by lower-case table name
}

// NewCache returns an empty cache.
func NewCache() *Cache {
	return &Cache{dbs: map[string]*dbCache{}}
}

// SchemaVersion reads the schema version of the database conn is connected to.
func SchemaVersion(ctx context.Context, conn *sql.Conn) (int64, error) {
	var v int64
	if err := conn.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("catalog: schema version: %w", err)
	}
	return v, nil
}

// Table is ReadTable through the cache.
func (c *Cache) Table(ctx context.Context, conn *sql.Conn, database, table string) (*meta.Table, error) {
	version, err := SchemaVersion(ctx, conn)
	if err != nil {
		return nil, err
	}
	key := strings.ToLower(table)
	c.mu.Lock()
	db := c.dbs[database]
	if db != nil && db.version == version {
		if t, ok := db.tables[key]; ok {
			c.mu.Unlock()
			return t, nil
		}
	}
	c.mu.Unlock()

	t, err := ReadTable(ctx, conn, database, table)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	db = c.dbs[database]
	if db == nil || db.version != version {
		db = &dbCache{version: version, tables: map[string]*meta.Table{}}
		c.dbs[database] = db
	}
	db.tables[key] = t
	return t, nil
}

// Forget drops what is cached for database. A database file that is dropped and
// created again may start from the same schema version, so DROP DATABASE must call it.
func (c *Cache) Forget(database string) {
	c.mu.Lock()
	delete(c.dbs, database)
	c.mu.Unlock()
}
