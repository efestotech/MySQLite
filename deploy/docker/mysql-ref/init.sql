-- The harness creates one database per corpus case on mysql-ref, as on mysqlite.
GRANT ALL PRIVILEGES ON *.* TO 'test'@'%';
FLUSH PRIVILEGES;
