package wire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

func header(length int, seq byte) []byte {
	return []byte{byte(length), byte(length >> 8), byte(length >> 16), seq}
}

// pipeGuard returns the client end of a pipe and a guard over the server end.
func pipeGuard(t *testing.T, limit int) (net.Conn, *packetGuard) {
	t.Helper()
	clientEnd, serverEnd := net.Pipe()
	t.Cleanup(func() {
		_ = clientEnd.Close()
		_ = serverEnd.Close()
	})
	deadline := time.Now().Add(10 * time.Second)
	_ = clientEnd.SetDeadline(deadline)
	_ = serverEnd.SetDeadline(deadline)
	return clientEnd, newPacketGuard(serverEnd, limit)
}

// sendThenReadErr writes chunks from the client end, then reads one packet and checks it
// is ERR 1153 with the given sequence id. net.Pipe is synchronous, so this must run
// concurrently with the guard's Read.
func sendThenReadErr(c net.Conn, wantSeq byte, chunks ...[]byte) error {
	for _, chunk := range chunks {
		if _, err := c.Write(chunk); err != nil {
			return fmt.Errorf("client write: %w", err)
		}
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return fmt.Errorf("read err header: %w", err)
	}
	if hdr[3] != wantSeq {
		return fmt.Errorf("want sequence %d, got %d", wantSeq, hdr[3])
	}
	payload := make([]byte, int(hdr[0])|int(hdr[1])<<8|int(hdr[2])<<16)
	if _, err := io.ReadFull(c, payload); err != nil {
		return fmt.Errorf("read err payload: %w", err)
	}
	if !bytes.Equal(payload[:3], []byte{0xff, 0x81, 0x04}) { // ERR header, 1153 little endian
		return fmt.Errorf("want ERR 1153, got % x", payload[:3])
	}
	if !bytes.Equal(payload[3:9], []byte("#08S01")) {
		return fmt.Errorf("want SQLSTATE 08S01, got %q", payload[3:9])
	}
	return nil
}

// readUntilError drains the guard and returns the first error and the bytes passed.
func readUntilError(g *packetGuard) (int, error) {
	buf := make([]byte, 1<<16)
	total := 0
	for {
		n, err := g.Read(buf)
		total += n
		if err != nil {
			return total, err
		}
	}
}

func TestPacketGuardRejectsOversizedPacketBeforePayload(t *testing.T) {
	clientEnd, guard := pipeGuard(t, 100)

	clientErr := make(chan error, 1)
	go func() { clientErr <- sendThenReadErr(clientEnd, 1, header(101, 0)) }()

	n, err := readUntilError(guard)
	if !errors.Is(err, errPacketTooLarge) || n != 0 {
		t.Fatalf("want errPacketTooLarge with 0 bytes passed, got n=%d err=%v", n, err)
	}
	if err := <-clientErr; err != nil {
		t.Fatal(err)
	}
	// The guard stays failed.
	if _, err := guard.Read(make([]byte, 8)); !errors.Is(err, errPacketTooLarge) {
		t.Fatalf("want sticky errPacketTooLarge, got %v", err)
	}
}

func TestPacketGuardSumsMultiPacketPayload(t *testing.T) {
	clientEnd, guard := pipeGuard(t, maxChunkPayload+10)

	clientErr := make(chan error, 1)
	go func() {
		clientErr <- sendThenReadErr(clientEnd, 2,
			header(maxChunkPayload, 0), make([]byte, maxChunkPayload), header(11, 1))
	}()

	if _, err := readUntilError(guard); !errors.Is(err, errPacketTooLarge) {
		t.Fatalf("want errPacketTooLarge, got %v", err)
	}
	if err := <-clientErr; err != nil {
		t.Fatal(err)
	}
}

func TestPacketGuardPassesPacketsWithinLimit(t *testing.T) {
	clientEnd, guard := pipeGuard(t, 100)

	// Two packets of 100 bytes each: the limit applies per logical packet, not per stream.
	var sent []byte
	for seq := byte(0); seq < 2; seq++ {
		sent = append(sent, header(100, seq)...)
		sent = append(sent, bytes.Repeat([]byte{'a' + seq}, 100)...)
	}
	go func() { _, _ = clientEnd.Write(sent) }()

	got := make([]byte, len(sent))
	if _, err := io.ReadFull(guard, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, sent) {
		t.Fatal("guard altered the byte stream")
	}
}
