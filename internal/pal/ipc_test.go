package pal

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewIPCTransfersCompletePayload(t *testing.T) {
	endpoint := GetIPCPath(t.TempDir())
	listener, err := ListenIPC(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	payload := bytes.Repeat([]byte("payload"), 1024)
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			serverErr <- err
			return
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, got); err != nil {
			serverErr <- err
			return
		}
		if !bytes.Equal(got, payload) {
			serverErr <- fmt.Errorf("server received corrupted payload")
			return
		}
		_, err = io.Copy(conn, bytes.NewReader(got))
		serverErr <- err
	}()
	conn, err := DialIPC(endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(conn, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("IPC response corrupted")
	}
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("IPC server did not finish")
	}
}

func TestReviewIPCAbsentServiceClassification(t *testing.T) {
	endpoint := GetIPCPath(t.TempDir())
	conn, err := DialIPC(endpoint, 100*time.Millisecond)
	if conn != nil {
		conn.Close()
	}
	if err == nil || !IsIPCNotFound(err) {
		t.Errorf("absent endpoint err=%v classified=%v", err, IsIPCNotFound(err))
	}
	for _, err := range []error{nil, os.ErrPermission, &net.OpError{Op: "dial", Err: os.ErrPermission}} {
		if IsIPCNotFound(err) {
			t.Errorf("non-absence error misclassified: %v", err)
		}
	}
}

func TestReviewIPCEndpointIdentity(t *testing.T) {
	base := t.TempDir()
	first := GetIPCPath(filepath.Join(base, "a"))
	if first == GetIPCPath(filepath.Join(base, "b")) {
		t.Error("different instances share endpoint")
	}
	if first != GetIPCPath(filepath.Join(base, "a", ".")) {
		t.Error("equivalent instance path changed endpoint")
	}
}
