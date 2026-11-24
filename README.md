# mysqlite

mysqlite è un server che parla il protocollo di rete di MySQL e salva i dati in file
SQLite. Un client MySQL (la CLI `mysql`, PHP PDO, Adminer, HeidiSQL, Connector/J) si
connette come a un MySQL 8.0; ogni database è un file `<nome>.db` nella cartella dati.

È pensato per sviluppo, test e piccole installazioni con un solo scrittore. **Non** è un
sostituto di MySQL in produzione: leggi le [limitazioni](#limitazioni) prima di usarlo.

Licenza: Apache-2.0 (`LICENSE`); le licenze delle dipendenze sono in `NOTICE`.

## Avvio rapido (meno di 5 minuti)

Serve Go 1.27.4 o successivo (`go version`). Nessun compilatore C: il binario è statico.

1. Clona e compila:

   ```sh
   git clone <url-del-repository> mysqlite
   cd mysqlite
   go build -o bin/mysqlite ./cmd/mysqlite
   ```

   Su Windows il binario è `bin/mysqlite.exe` (`go build -o bin/mysqlite.exe ./cmd/mysqlite`).

2. Crea `mysqlite.yaml` nella cartella del repository:

   ```yaml
   listen: 127.0.0.1
   port: 3307
   data_dir: data
   users:
     - name: test
       password: test
   log:
     level: info
     dir: logs
   ```

3. Avvia il server:

   ```sh
   ./bin/mysqlite -config mysqlite.yaml
   ```

   All'avvio il log riporta un WARN sui DECIMAL (vedi sotto): è atteso.

4. Connettiti da un altro terminale e crea un database:

   ```sh
   mysql -h 127.0.0.1 -P 3307 -u test -ptest
   ```

   ```sql
   CREATE DATABASE app;
   USE app;
   CREATE TABLE t (id INT PRIMARY KEY AUTO_INCREMENT, nome VARCHAR(50));
   INSERT INTO t (nome) VALUES ('ciao');
   SELECT * FROM t;
   ```

   Il file `data/app.db` è un normale database SQLite.

Con Docker (non serve Go sull'host): `scripts/dev.sh up` (Linux, macOS) o
`scripts/dev.ps1 up` (Windows) avvia mysqlite sulla porta 3307 con utente `test` /
`test`, più un MySQL 8.0 di riferimento sulla porta 3306.

## Configurazione

Ordine di precedenza: flag della riga di comando > file YAML > valori predefiniti. Non si
usano variabili d'ambiente. Una chiave sconosciuta nel file è un errore.

| Chiave | Predefinito | Significato |
|---|---|---|
| `listen` | `127.0.0.1` | Indirizzo di ascolto |
| `port` | `3307` | Porta |
| `data_dir` | `data` | Cartella dei file `<database>.db` (creata se manca) |
| `users` | — | Elenco di `name`, `password`, `default_db` opzionale |
| `allow_remote` | `false` | Necessario per ascoltare su un indirizzo non locale (non c'è TLS) |
| `server_version` | `8.0.36-mysqlite` | Versione annunciata ai client |
| `max_allowed_packet` | `67108864` | Dimensione massima di un pacchetto |
| `max_read_conns` | `4` | Connessioni SQLite in lettura per database |
| `busy_timeout_ms` | `5000` | Attesa di SQLite su un file occupato |
| `synchronous` | `NORMAL` | `PRAGMA synchronous` |
| `write_queue_depth` | `256` | Sessioni in coda per lo scrittore di un database; oltre, errore 1205 |
| `idle_tx_timeout_s` | `60` | Rollback di una transazione inattiva; `0` disattiva |
| `readonly` | `false` | Ogni scrittura risponde 1290 |
| `alter_backup` | `true` | Copia del database prima di un `ALTER TABLE` che ricostruisce la tabella |
| `log.level` | `info` | `debug`, `info`, `warn`, `error` |
| `log.dir` | `logs` | Cartella dei log |
| `log.max_size_mb`, `log.max_files` | `100`, `10` | Rotazione dei log e del diario |
| `log.journal` | `true` | Diario delle query ricevute (JSONL) |

Flag: `-config` (predefinito `mysqlite.yaml`), `-listen`, `-port`, `-data-dir`,
`-log-level`, `-readonly`, `-write-queue-depth`, `-idle-tx-timeout`.

## Limitazioni

mysqlite traduce l'SQL di MySQL in SQLite. Quando non sa tradurre una costruzione in modo
fedele risponde con l'errore **1235** (`ER_NOT_SUPPORTED_YET`) invece di indovinare.
Le differenze note sono queste.

### DECIMAL non è esatto

Una colonna `DECIMAL(p,s)` è salvata come `REAL` di SQLite (virgola mobile a 64 bit, ADR
0006). I client vedono il tipo DECIMAL e la scala dichiarata (`1.50`), ma i calcoli non
sono esatti: `0.1 + 0.2` può differire nell'ultima cifra e somme lunghe accumulano
errori. Le cifre significative oltre circa 15 si perdono. **Non usare mysqlite per
importi monetari che richiedono precisione esatta.** A ogni avvio il log lo ricorda; con
il diario attivo le istruzioni con aritmetica su DECIMAL sono marcate
`"decimal_as_real": true`. `ROUND` e `TRUNCATE` arrotondano come MySQL.

### Collation approssimate

La collation predefinita di mysqlite è `utf8mb4_general_ci`, non `utf8mb4_0900_ai_ci`
come in MySQL 8: una tabella creata senza `COLLATE` confronta senza distinguere
maiuscole e accenti e ignora gli spazi finali (`'Bar' = 'Bar  '` è vero; in MySQL 8 con
la collation predefinita è falso). Per avere il comportamento di MySQL 8 dichiarare
`COLLATE utf8mb4_0900_ai_ci` sulla tabella o sulla colonna.

Le collation sono un'approssimazione di quelle di MySQL (ADR 0004), calcolate dai dati
Unicode e non dalle tabelle di MySQL:

- `utf8mb4_general_ci`: confronto senza maiuscole e accenti per singolo carattere; gli
  spazi finali sono ignorati (PAD SPACE).
- `utf8mb4_0900_ai_ci` (predefinita di MySQL 8): stessa idea con `ß` = `ss`, NO PAD.
- `utf8mb4_bin`, `utf8mb4_0900_bin`: ordine dei code point.

Ordinamenti e confronti con caratteri rari, contrazioni o regole specifiche di una lingua
possono differire da MySQL. Altre collation e altri charset rispondono 1235.

### Un solo scrittore per database

SQLite ammette un solo scrittore per file. mysqlite mette in coda le sessioni che
scrivono sullo stesso database: una transazione che scrive tiene lo scrittore fino al
`COMMIT` o al `ROLLBACK`. Le letture procedono in parallelo.

- Oltre `write_queue_depth` sessioni in attesa, o dopo `innodb_lock_wait_timeout`,
  l'errore è **1205** (lock wait timeout).
- Una transazione inattiva per `idle_tx_timeout_s` secondi viene annullata.
- Non c'è lock per riga: due transazioni non scrivono mai in parallelo sullo stesso
  database, e `SELECT ... FOR UPDATE` / `FOR SHARE` rispondono 1235.
- `LOCK TABLES` / `UNLOCK TABLES` fanno un commit e un warning, senza prendere lock.

### Altre differenze

- **Autenticazione**: solo `mysql_native_password`. Niente TLS: per i client usare
  `--ssl-mode=DISABLED` (MySQL) o `--skip-ssl` (MariaDB) se la richiedono. Niente
  `caching_sha2_password`; un client che supporta solo quello non si connette.
- **Database**: `CREATE DATABASE` accetta solo `utf8mb4` / `utf8mb4_general_ci` e
  `ENCRYPTION 'N'`; altre opzioni rispondono 1235. `DROP DATABASE` risponde 1235: per
  eliminare un database si cancella il file `<nome>.db` a server fermo.
- **Interi**: `BIGINT UNSIGNED` oltre 9223372036854775807 risponde 1235. `SUM` di interi
  oltre 2^63 risponde 1235 (MySQL restituisce un DECIMAL). Un overflow dentro una `WHERE`
  non è rilevato.
- **Letterali binari**: `X'..'`, `0x..`, `_binary'..'` sono accettati solo come valori
  scritti in colonne BINARY, VARBINARY o BLOB; altrove 1235.
- **Fuso orario**: `time_zone` accetta `SYSTEM`, `UTC` e scostamenti (`+02:00`); i fusi
  con nome (`Europe/Rome`) rispondono 1298 (ADR 0007). `TIMESTAMP` è salvato in UTC;
  `SYSDATE()` vale l'ora di inizio dell'istruzione.
- **ALTER TABLE** che ricostruisce una tabella copia prima l'intero database in
  `<nome>.pre-alter-<data>.sqlite` (ne tiene 3; `alter_backup: false` lo disattiva).
- **Messaggio info** (`Records: 3  Duplicates: 0  Warnings: 0` dopo un INSERT multiplo):
  arriva solo ai client che dichiarano `CLIENT_SESSION_TRACK` (ADR 0003).
- **information_schema** e `SHOW` coprono ciò che i client della matrice usano (ADR
  0005); alcune tabelle sono vuote (per esempio `TRIGGERS`, dato che i trigger non
  esistono) e le costruzioni non tradotte rispondono 1235.
- Le differenze puntuali sono elencate in `docs/agent-notes/discrepancies.md`.

## Cosa mysqlite NON fa

Per scelta, nella versione 0.1. Il client riceve un errore MySQL chiaro.

- Stored procedure, funzioni, eventi, trigger in sintassi MySQL.
- Utenti, `GRANT`, `REVOKE`, privilegi: un utente configurato ha accesso completo.
- Replica, binlog, GTID.
- Compressione del protocollo, TLS, `caching_sha2_password`.
- `LOAD DATA INFILE`, `LOAD LOCAL INFILE`, `SELECT ... INTO OUTFILE`.
- Indici FULLTEXT e SPATIAL, `MATCH ... AGAINST`.
- Query su più database (join tra database).
- `HANDLER`, `GET_LOCK`, transazioni XA, lock reali di tabella.
- `SQL_CALC_FOUND_ROWS` / `FOUND_ROWS()`.
- Piani `EXPLAIN` fedeli.
- Prestazioni pari a MySQL: la correttezza viene prima.
- Lock per riga.

## Client provati

Stato in `docs/agent-notes/client-matrix.md`. In sintesi:

| Priorità | Client | Stato |
|---|---|---|
| P0 | CLI `mysql` 8.0, 8.4 | Scenario scritto (`test/clients/mysql-cli`), da eseguire in Docker |
| P1 | CLI MariaDB 11 | Scenario scritto, da eseguire in Docker |
| P1 | PHP PDO (prepare native ed emulate) | Supera lo scenario (PHP 8.4) |
| P1 | Adminer 5.4.1 | Supera lo scenario HTTP (`test/clients/adminer`) |
| P2 | HeidiSQL | Checklist manuale (`docs/agent-notes/heidisql-checklist.md`) |
| P2 | Connector/J 9.1.0 | Scenario scritto, da eseguire in Docker |

`scripts/dev.sh clients` esegue tutta la matrice in Docker.

## Sviluppo

- Test: `go test ./...` (sull'host) o `scripts/dev.sh test` (con `-race`, in Docker).
- Confronto con MySQL vero: `scripts/dev.sh harness` esegue il corpus `test/corpus` su
  MySQL 8.0 e su mysqlite e confronta righe, tipi, errori; il report va in
  `logs/harness/report.json`.
- Lint: `scripts/dev.sh lint`. Licenze: `go run ./scripts/notice -check`.
- Binari statici per Linux: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath
  -ldflags="-s -w" ./cmd/mysqlite` (anche `GOARCH=arm64`).
- Documentazione di progetto: `AGENTS.md` e `docs/`.
