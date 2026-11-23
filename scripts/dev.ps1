# Developer entrypoint (docs/09-conventions.md section 6): wraps `docker compose` with
# the same verbs as scripts/dev.sh. Needs only Docker; no environment variables.
param([string]$Verb = "")

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$composeFile = Join-Path $root "deploy/docker/compose.yaml"

function Invoke-Compose {
    & docker compose -f $composeFile @args
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

$usage = @"
usage: scripts/dev.ps1 VERB

  up       build and start mysqlite (port 3307) and mysql-ref (port 3306)
  down     stop and remove every container
  build    build the images
  test     go vet and go test -race in the toolchain container
  harness  run the differential harness (mysql-ref against mysqlite)
  clients  run the client matrix scenarios (mysql 8.0, 8.4, MariaDB 11, PHP PDO,
           Adminer, Connector/J)
  lint     golangci-lint
  shell    a shell in the toolchain container
  logs     follow the mysqlite logs
"@

switch ($Verb) {
    "up" { Invoke-Compose up -d --build mysqlite mysql-ref }
    "down" { Invoke-Compose --profile tools --profile clients down }
    "build" { Invoke-Compose --profile tools --profile clients build }
    "test" { Invoke-Compose run --rm dev sh -c "go vet ./... && go test -race -count=1 ./..." }
    "harness" {
        Invoke-Compose up -d --build mysqlite mysql-ref
        Invoke-Compose run --rm runner
    }
    "clients" {
        Invoke-Compose up -d --build mysqlite adminer
        foreach ($s in @("cli80", "cli84", "cli-maria", "php", "java", "adminer-smoke")) {
            Write-Host "== $s"
            Invoke-Compose run --rm --build $s
        }
    }
    "lint" { Invoke-Compose run --rm lint }
    "shell" { Invoke-Compose run --rm dev bash }
    "logs" { Invoke-Compose logs -f mysqlite }
    default {
        [Console]::Error.WriteLine($usage)
        exit 2
    }
}
