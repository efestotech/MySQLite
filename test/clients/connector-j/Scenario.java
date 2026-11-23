// Connector/J smoke scenario (docs/07-testing.md section 6, M4 criterion 2): server-side
// prepared statements (useServerPrepStmts=true) through the binary protocol, and the Java
// types of getInt, getBigDecimal and getTimestamp.
//
// Build and run with the Connector/J jar on the class path (Java 8+):
//   javac Scenario.java
//   java -cp .:mysql-connector-j.jar Scenario
// Environment: MYSQLITE_HOST, MYSQLITE_PORT, MYSQLITE_USER, MYSQLITE_PASSWORD and
// MYSQLITE_DB, an existing database where the scenario creates and drops one table.
// Prints one line per step and "PASS" at the end; exits 1 on the first failure.

import java.math.BigDecimal;
import java.sql.Connection;
import java.sql.DatabaseMetaData;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.sql.Statement;
import java.sql.Timestamp;
import java.sql.Types;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Random;

public class Scenario {
    static void check(String step, boolean ok, Object got) {
        if (!ok) {
            System.err.println("FAIL " + step + ": " + got);
            System.exit(1);
        }
        System.out.println("ok " + step);
    }

    static String env(String name, String def) {
        String v = System.getenv(name);
        return v == null || v.isEmpty() ? def : v;
    }

    public static void main(String[] args) throws Exception {
        String url = "jdbc:mysql://" + env("MYSQLITE_HOST", "127.0.0.1") + ":" + env("MYSQLITE_PORT", "3307")
                + "/" + env("MYSQLITE_DB", "test")
                + "?useServerPrepStmts=true&cachePrepStmts=false&useSSL=false&sslMode=DISABLED"
                + "&allowPublicKeyRetrieval=true&characterEncoding=UTF-8&connectionTimeZone=SERVER";
        String table = "cj_people_" + Integer.toHexString(new Random().nextInt() & 0x7fffffff);
        byte[] photo = new byte[256];
        for (int i = 0; i < photo.length; i++) {
            photo[i] = (byte) i;
        }
        try (Connection c = DriverManager.getConnection(url, env("MYSQLITE_USER", "test"), env("MYSQLITE_PASSWORD", "test"))) {
            check("connect", true, null);
            DatabaseMetaData md = c.getMetaData();
            check("driver", md.getDriverName() != null, md.getDriverName() + " " + md.getDriverVersion());
            System.out.println("driver " + md.getDriverName() + " " + md.getDriverVersion());

            try (Statement s = c.createStatement()) {
                List<String> dbs = new ArrayList<>();
                try (ResultSet rs = s.executeQuery("SHOW DATABASES")) {
                    while (rs.next()) {
                        dbs.add(rs.getString(1));
                    }
                }
                check("list databases", dbs.contains(env("MYSQLITE_DB", "test")), dbs);
                s.execute("CREATE TABLE " + table + " (id INT AUTO_INCREMENT PRIMARY KEY, name VARCHAR(50) NOT NULL,"
                        + " age INT, balance DECIMAL(10,2), seen DATETIME, photo BLOB)");
                List<String> tables = new ArrayList<>();
                try (ResultSet rs = s.executeQuery("SHOW TABLES")) {
                    while (rs.next()) {
                        tables.add(rs.getString(1));
                    }
                }
                check("list tables", tables.contains(table), tables);
                List<String> cols = new ArrayList<>();
                try (ResultSet rs = s.executeQuery("DESCRIBE " + table)) {
                    while (rs.next()) {
                        cols.add(rs.getString(1));
                    }
                }
                check("describe table", cols.equals(Arrays.asList("id", "name", "age", "balance", "seen", "photo")), cols);
            }

            try (PreparedStatement ins = c.prepareStatement(
                    "INSERT INTO " + table + " (name, age, balance, seen, photo) VALUES (?, ?, ?, ?, ?)",
                    Statement.RETURN_GENERATED_KEYS)) {
                ins.setString(1, "O'Brien \\ \"quoted\"");
                ins.setInt(2, 42);
                ins.setBigDecimal(3, new BigDecimal("1234.50"));
                ins.setTimestamp(4, Timestamp.valueOf("2024-01-02 10:20:30"));
                ins.setBytes(5, photo);
                check("insert", ins.executeUpdate() == 1, null);
                try (ResultSet keys = ins.getGeneratedKeys()) {
                    check("generated key", keys.next() && keys.getLong(1) == 1L, null);
                }
                ins.setString(1, "città");
                ins.setNull(2, Types.INTEGER);
                ins.setBigDecimal(3, new BigDecimal("-0.75"));
                ins.setNull(4, Types.TIMESTAMP);
                ins.setNull(5, Types.BLOB);
                check("insert nulls", ins.executeUpdate() == 1, null);
            }

            try (PreparedStatement sel = c.prepareStatement(
                    "SELECT id, name, age, balance, seen, photo FROM " + table + " WHERE name = ?")) {
                sel.setString(1, "O'Brien \\ \"quoted\"");
                try (ResultSet rs = sel.executeQuery()) {
                    check("select where string", rs.next(), null);
                    check("getInt", rs.getInt("age") == 42 && rs.getObject("age") instanceof Integer, rs.getObject("age"));
                    BigDecimal b = rs.getBigDecimal("balance");
                    check("getBigDecimal", b != null && b.equals(new BigDecimal("1234.50")), b);
                    Timestamp ts = rs.getTimestamp("seen");
                    check("getTimestamp", ts != null && ts.equals(Timestamp.valueOf("2024-01-02 10:20:30")), ts);
                    check("blob byte-identical", Arrays.equals(rs.getBytes("photo"), photo), null);
                }
                sel.setString(1, "città");
                try (ResultSet rs = sel.executeQuery()) {
                    check("nulls", rs.next() && rs.getObject("age") == null && rs.getTimestamp("seen") == null
                            && rs.getBytes("photo") == null, null);
                }
                sel.setString(1, "x' OR '1'='1");
                try (ResultSet rs = sel.executeQuery()) {
                    check("injection is data", !rs.next(), null);
                }
            }

            try (PreparedStatement page = c.prepareStatement("SELECT name FROM " + table + " ORDER BY id LIMIT ? OFFSET ?")) {
                page.setInt(1, 1);
                page.setInt(2, 1);
                try (ResultSet rs = page.executeQuery()) {
                    check("limit placeholders", rs.next() && rs.getString(1).equals("città") && !rs.next(), null);
                }
            }

            try (PreparedStatement upd = c.prepareStatement("UPDATE " + table + " SET age = ? WHERE name = ?")) {
                upd.setInt(1, 7);
                upd.setString(2, "città");
                check("update", upd.executeUpdate() == 1, null);
            }
            try (PreparedStatement del = c.prepareStatement("DELETE FROM " + table + " WHERE age = ?")) {
                del.setInt(1, 7);
                check("delete", del.executeUpdate() == 1, null);
            }
            try (PreparedStatement dup = c.prepareStatement("INSERT INTO " + table + " (id, name) VALUES (?, ?)")) {
                dup.setInt(1, 1);
                dup.setString(2, "dup");
                dup.executeUpdate();
                check("duplicate key", false, null);
            } catch (SQLException e) {
                check("duplicate key", e.getErrorCode() == 1062 && "23000".equals(e.getSQLState()), e.getErrorCode());
            }
            try (Statement s = c.createStatement()) {
                s.execute("DROP TABLE " + table);
            }
            check("drop table", true, null);
        }
        check("disconnect", true, null);
        System.out.println("PASS");
    }
}
