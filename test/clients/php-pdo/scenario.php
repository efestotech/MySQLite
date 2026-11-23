<?php
// PHP PDO smoke scenario (docs/07-testing.md section 6, M4 criterion 1): native
// prepared statements (ATTR_EMULATE_PREPARES=false) through the binary protocol.
//
// Environment: MYSQLITE_HOST, MYSQLITE_PORT, MYSQLITE_USER, MYSQLITE_PASSWORD and
// MYSQLITE_DB, an existing database where the scenario creates and drops one table.
// Prints one line per step and "PASS" at the end; exits 1 on the first failure.

declare(strict_types=1);

function check(string $step, bool $ok, $got = null): void
{
    if (!$ok) {
        fwrite(STDERR, "FAIL $step: " . var_export($got, true) . "\n");
        exit(1);
    }
    echo "ok $step\n";
}

$host = getenv('MYSQLITE_HOST') ?: '127.0.0.1';
$port = getenv('MYSQLITE_PORT') ?: '3307';
$user = getenv('MYSQLITE_USER') ?: 'test';
$password = getenv('MYSQLITE_PASSWORD') ?: 'test';
$db = getenv('MYSQLITE_DB') ?: 'test';

try {
    $pdo = new PDO("mysql:host=$host;port=$port;dbname=$db;charset=utf8mb4", $user, $password, [
        PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION,
        PDO::ATTR_EMULATE_PREPARES => false,
        PDO::ATTR_STRINGIFY_FETCHES => false,
    ]);
    check('connect', true);
    check('native prepares', $pdo->getAttribute(PDO::ATTR_EMULATE_PREPARES) === false);

    $dbs = $pdo->query('SHOW DATABASES')->fetchAll(PDO::FETCH_COLUMN);
    check('list databases', in_array($db, $dbs, true), $dbs);

    // A table name of its own per run.
    $t = 'pdo_people_' . bin2hex(random_bytes(4));
    $pdo->exec("CREATE TABLE $t (
        id INT AUTO_INCREMENT PRIMARY KEY,
        name VARCHAR(50) NOT NULL,
        age INT,
        balance DECIMAL(10,2),
        score DOUBLE,
        born DATE,
        seen DATETIME,
        photo BLOB
    )");
    $tables = $pdo->query('SHOW TABLES')->fetchAll(PDO::FETCH_COLUMN);
    check('list tables', in_array($t, $tables, true), $tables);
    $cols = $pdo->query("DESCRIBE $t")->fetchAll(PDO::FETCH_COLUMN);
    check('describe table', $cols === ['id', 'name', 'age', 'balance', 'score', 'born', 'seen', 'photo'], $cols);

    // INSERT: one prepared statement executed several times. mysqlnd re-sends the
    // parameter types only when they change (new-params-bound flag 0 otherwise).
    $photo = '';
    for ($i = 0; $i < 256; $i++) {
        $photo .= chr($i);
    }
    $ins = $pdo->prepare("INSERT INTO $t (name, age, balance, score, born, seen, photo) VALUES (?, ?, ?, ?, ?, ?, ?)");
    $rows = [
        ["O'Brien \\ \"quoted\"", 42, '1234.50', 0.25, '1980-05-17', '2024-01-02 10:20:30', $photo],
        ['città', null, '-0.75', -1.5, null, null, null],
        ['plain', 7, '0.00', 3.0, '2000-01-01', '2000-01-01 00:00:00', 'x'],
    ];
    foreach ($rows as $r) {
        $ins->bindValue(1, $r[0], PDO::PARAM_STR);
        $ins->bindValue(2, $r[1], $r[1] === null ? PDO::PARAM_NULL : PDO::PARAM_INT);
        $ins->bindValue(3, $r[2], PDO::PARAM_STR);
        $ins->bindValue(4, $r[3]);
        $ins->bindValue(5, $r[4], $r[4] === null ? PDO::PARAM_NULL : PDO::PARAM_STR);
        $ins->bindValue(6, $r[5], $r[5] === null ? PDO::PARAM_NULL : PDO::PARAM_STR);
        $ins->bindValue(7, $r[6], $r[6] === null ? PDO::PARAM_NULL : PDO::PARAM_LOB);
        $ins->execute();
    }
    check('insert', $ins->rowCount() === 1 && $pdo->lastInsertId() === '3', [$ins->rowCount(), $pdo->lastInsertId()]);
    // execute() with an array: every value bound as a string.
    $pdo->prepare("INSERT INTO $t (name, age) VALUES (?, ?)")->execute(['array', '99']);

    // SELECT with a WHERE on a string; native types come back.
    $sel = $pdo->prepare("SELECT id, name, age, balance, score, born, seen, photo FROM $t WHERE name = ?");
    $sel->execute(["O'Brien \\ \"quoted\""]);
    $row = $sel->fetch(PDO::FETCH_ASSOC);
    check('select where string', $row !== false && $row['name'] === "O'Brien \\ \"quoted\"", $row);
    check('int column is int', $row['id'] === 1 && $row['age'] === 42, $row);
    check('decimal column', $row['balance'] === '1234.50', $row['balance']);
    check('double column is float', $row['score'] === 0.25, $row['score']);
    check('date and datetime', $row['born'] === '1980-05-17' && $row['seen'] === '2024-01-02 10:20:30', $row);
    check('blob byte-identical', $row['photo'] === $photo, strlen((string)$row['photo']));

    $sel->execute(['città']);
    $row = $sel->fetch(PDO::FETCH_ASSOC);
    check('nulls', $row['age'] === null && $row['born'] === null && $row['photo'] === null && $row['balance'] === '-0.75', $row);
    $sel->execute(["x' OR '1'='1"]);
    check('injection is data', $sel->fetchAll() === []);

    // LIMIT with placeholders (docs/05-dialect.md section 10).
    $page = $pdo->prepare("SELECT name FROM $t ORDER BY id LIMIT ? OFFSET ?");
    $page->bindValue(1, 2, PDO::PARAM_INT);
    $page->bindValue(2, 1, PDO::PARAM_INT);
    $page->execute();
    $names = $page->fetchAll(PDO::FETCH_COLUMN);
    check('limit placeholders', $names === ['città', 'plain'], $names);

    // UPDATE and DELETE.
    $upd = $pdo->prepare("UPDATE $t SET age = age + ?, balance = ? WHERE name = ?");
    $upd->execute([1, '10.10', 'plain']);
    check('update', $upd->rowCount() === 1, $upd->rowCount());
    $age = $pdo->prepare("SELECT age, balance FROM $t WHERE name = ?");
    $age->execute(['plain']);
    $row = $age->fetch(PDO::FETCH_NUM);
    check('update applied', $row === [8, '10.10'], $row);

    $del = $pdo->prepare("DELETE FROM $t WHERE id > ?");
    $del->execute([2]);
    check('delete', $del->rowCount() === 2, $del->rowCount());
    $count = (int)$pdo->query("SELECT COUNT(*) FROM $t")->fetchColumn();
    check('rows left', $count === 2, $count);

    // Errors keep their MySQL code through EXECUTE.
    try {
        $pdo->prepare("INSERT INTO $t (id, name) VALUES (?, ?)")->execute([1, 'dup']);
        check('duplicate key', false);
    } catch (PDOException $e) {
        check('duplicate key', $e->errorInfo[1] === 1062 && $e->errorInfo[0] === '23000', $e->errorInfo);
    }
    try {
        $pdo->prepare("SELECT nope FROM $t WHERE id = ?");
        check('prepare error', false);
    } catch (PDOException $e) {
        check('prepare error', $e->errorInfo[1] === 1054, $e->errorInfo);
    }

    // Transaction through prepared statements.
    $pdo->beginTransaction();
    $pdo->prepare("INSERT INTO $t (name) VALUES (?)")->execute(['rolled back']);
    $pdo->rollBack();
    $count = (int)$pdo->query("SELECT COUNT(*) FROM $t")->fetchColumn();
    check('rollback', $count === 2, $count);

    $pdo->exec("DROP TABLE $t");
    check('drop table', !in_array($t, $pdo->query('SHOW TABLES')->fetchAll(PDO::FETCH_COLUMN), true));
    $pdo = null;
    check('disconnect', true);
} catch (Throwable $e) {
    fwrite(STDERR, 'FAIL ' . get_class($e) . ': ' . $e->getMessage() . "\n");
    exit(1);
}
echo "PASS\n";
