package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-mysql-org/go-mysql/client"
)

func TestRunServesSelectOneAndStopsOnCancel(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "mysqlite.yaml")
	body := "port: 0\n" +
		"data_dir: " + filepath.ToSlash(filepath.Join(dir, "data")) + "\n" +
		"log:\n  dir: " + filepath.ToSlash(filepath.Join(dir, "logs")) + "\n" +
		"users:\n  - name: test\n    password: s3cret\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrCh := make(chan net.Addr, 1)
	var console bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--config", cfgPath}, &console, func(a net.Addr) { addrCh <- a })
	}()

	var addr net.Addr
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("run exited early: %v\n%s", err, console.String())
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start")
	}

	conn, err := client.Connect(addr.String(), "test", "s3cret", "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	res, err := conn.Execute("SELECT 1")
	if err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if v, _ := res.GetInt(0, 0); v != 1 {
		t.Fatalf("want 1, got %d", v)
	}
	_ = conn.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop after cancel")
	}

	logBytes, err := os.ReadFile(filepath.Join(dir, "logs", "server.log"))
	if err != nil {
		t.Fatalf("read server log: %v", err)
	}
	logText := string(logBytes)
	if !strings.Contains(logText, "effective configuration") {
		t.Error("server log must contain the effective configuration")
	}
	if strings.Contains(logText, "s3cret") {
		t.Error("server log must never contain a password")
	}
}

func TestRunFailsFastOnInvalidConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "mysqlite.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 3307\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	err := run(context.Background(), []string{"--config", cfgPath}, &bytes.Buffer{}, nil)
	if err == nil || !strings.Contains(err.Error(), "users") {
		t.Fatalf("want config error about users, got %v", err)
	}
}
