package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	pionstun "github.com/pion/stun/v3"
)

func TestRunSTUNWithBindingOnlyServer(t *testing.T) {
	server := startBindingOnlySTUNServer(t)
	var output bytes.Buffer
	err := runSTUN(context.Background(), &output, stunConfig{
		server:  server,
		timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"[stun] public UDP mapping=",
		"this STUN server answers mapping queries",
		"OTHER-ADDRESS",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("output missing %q:\n%s", expected, output.String())
		}
	}
}

func startBindingOnlySTUNServer(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, peer, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			request := &pionstun.Message{Raw: append([]byte(nil), buffer[:n]...)}
			if err := request.Decode(); err != nil || request.Type != pionstun.BindingRequest {
				continue
			}
			udpPeer, ok := peer.(*net.UDPAddr)
			if !ok {
				continue
			}
			response := pionstun.MustBuild(
				pionstun.NewTransactionIDSetter(request.TransactionID),
				pionstun.BindingSuccess,
				&pionstun.XORMappedAddress{IP: udpPeer.IP, Port: udpPeer.Port},
			)
			_, _ = conn.WriteTo(response.Raw, peer)
		}
	}()
	return conn.LocalAddr().String()
}
