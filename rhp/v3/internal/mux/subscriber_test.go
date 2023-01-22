package mux

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	nlog "gitlab.com/NebulousLabs/log"
	"gitlab.com/NebulousLabs/siamux"
	nmux "gitlab.com/NebulousLabs/siamux/mux"
	"go.sia.tech/mux/v1"
	"lukechampine.com/frand"
)

func startEchoSubscriber(l net.Listener, priv ed25519.PrivateKey) {
	for {
		conn, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		} else if err != nil {
			panic(err)
		}

		m, err := AcceptSubscriber(conn, priv)
		if err != nil {
			panic(err)
		}

		go func() {
			defer func() {
				log.Println("closing mux")
				m.Close()
			}()

			for i := 0; ; i++ {
				stream, subscriber, err := m.AcceptSubscriberStream()
				if errors.Is(err, mux.ErrPeerClosedConn) {
					return
				} else if err != nil {
					panic(err)
				}
				if subscriber != "echo" {
					stream.Close()
					continue
				}

				go func(i int) {
					defer stream.Close()
					io.Copy(stream, stream)
				}(i)
			}
		}()
	}
}

func TestSubscriberMux(t *testing.T) {
	serverKey := ed25519.NewKeyFromSeed(frand.Bytes(ed25519.SeedSize))

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal("failed to listen:", err)
	}
	t.Cleanup(func() { listener.Close() })
	go startEchoSubscriber(listener, serverKey)

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal("failed to dial:", err)
	}
	t.Cleanup(func() { conn.Close() })

	m, err := DialSubscriber(conn, serverKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal("failed to dial subscriber:", err)
	}
	t.Cleanup(func() { m.Close() })

	t.Run("test bad subscriber", func(t *testing.T) {
		// Due to the laziness expected by siad, we cannot detect the unknown
		// subscriber error as part of the handshake. We have to first write
		// to then read from the stream.
		s, err := m.NewSubscriberStream("bad subscriber")
		if err != nil {
			t.Fatal("failed to initiate stream:", err)
		}
		defer s.Close()

		if err := writePrefixedBytes(s, []byte("hello")); err != nil {
			t.Fatal("failed to write to stream:", err)
		} else if _, err := readPrefixedBytes(s, 1024); err == nil {
			t.Fatalf("expected subscriber error")
		}
	})

	t.Run("test good subscriber", func(t *testing.T) {
		s, err := m.NewSubscriberStream("echo")
		if err != nil {
			t.Fatal("failed to create subscriber stream:", err)
		}
		defer s.Close()

		req := frand.Bytes(128)
		if err := writePrefixedBytes(s, req); err != nil {
			t.Fatal("failed to write object to stream:", err)
		} else if resp, err := readPrefixedBytes(s, 1024); err != nil {
			t.Fatal("failed to read subscriber response:", err)
		} else if !bytes.Equal(req, resp) {
			t.Fatalf("unexpected reply: got %v expected %v", resp, req)
		}
	})
}

func TestManySubscribers(t *testing.T) {
	serverKey := ed25519.NewKeyFromSeed(frand.Bytes(ed25519.SeedSize))

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal("failed to listen:", err)
	}
	t.Cleanup(func() { listener.Close() })
	go startEchoSubscriber(listener, serverKey)

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal("failed to dial:", err)
	}
	t.Cleanup(func() { conn.Close() })

	m, err := DialSubscriber(conn, serverKey.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal("failed to dial subscriber:", err)
	}
	t.Cleanup(func() { m.Close() })

	var wg sync.WaitGroup
	var errCh = make(chan error, 1000)
	wg.Add(cap(errCh))
	for i := 0; i < cap(errCh); i++ {
		go func(i int) {
			defer wg.Done()
			s, err := m.NewSubscriberStream("echo")
			if err != nil {
				errCh <- fmt.Errorf("stream %v: failed to create subscriber stream: %w", i, err)
				return
			}
			defer s.Close()

			req := []byte("hello world")
			if err := writePrefixedBytes(s, req); err != nil {
				errCh <- fmt.Errorf("stream %v: failed to write object to stream: %w", i, err)
				return
			} else if resp, err := readPrefixedBytes(s, 1024); err != nil {
				errCh <- fmt.Errorf("stream %v: failed to read subscriber reply: %w", i, err)
				return
			} else if !bytes.Equal(req, resp) {
				errCh <- fmt.Errorf("stream %v: unexpected reply: got %v expected %v", i, resp, req)
				return
			}
			log.Println(i, "done")
		}(i)
	}

	wg.Wait()
	close(errCh)
	var errored bool
	for err := range errCh {
		if err != nil {
			errored = true
			t.Log(err)
		}
	}
	if errored {
		t.FailNow()
	}
}

func TestSubscriberRouterCompat(t *testing.T) {
	dir := t.TempDir()

	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal("failed to create listener:", err)
	}
	t.Cleanup(func() { l.Close() })

	serverKey := ed25519.NewKeyFromSeed(frand.Bytes(32))
	startEchoSubscriber(l, serverKey)

	m, err := siamux.New(":0", ":0", nlog.DiscardLogger, filepath.Join(dir, "siamux"))
	if err != nil {
		t.Fatal("failed to create sia mux:", err)
	}
	t.Cleanup(func() { m.Close() })

	var key nmux.ED25519PublicKey
	copy(key[:], serverKey.Public().(ed25519.PublicKey)[:])

	t.Run("bad subscriber", func(t *testing.T) {
		s, err := m.NewStream("bad sub", l.Addr().String(), key)
		if err != nil {
			t.Fatal("failed to create subscriber stream:", err)
		}

		if err := writePrefixedBytes(s, []byte("hello")); err != nil {
			t.Fatal("failed to write to stream:", err)
		} else if _, err := readPrefixedBytes(s, 1024); err == nil || !strings.Contains(err.Error(), "unknown subscriber") {
			t.Fatal("expected subscriber error:", err)
		}
	})

	t.Run("echo subscriber", func(t *testing.T) {
		s, err := m.NewStream("echo", l.Addr().String(), key)
		if err != nil {
			t.Fatal("failed to create subscriber stream:", err)
		}

		req := frand.Bytes(128)
		if err := writePrefixedBytes(s, req); err != nil {
			t.Fatal("failed to write object to stream:", err)
		} else if resp, err := readPrefixedBytes(s, 1024); err != nil {
			t.Fatal("failed to read subscriber reply:", err)
		} else if !bytes.Equal(req, resp) {
			t.Fatalf("unexpected reply: got %v expected %v", resp, req)
		}
	})
}

func TestSubscriberMuxCompat(t *testing.T) {
	dir := t.TempDir()

	serverMux, err := siamux.New(":0", ":0", nlog.DiscardLogger, filepath.Join(dir, "siamux"))
	if err != nil {
		t.Fatal("failed to create sia mux:", err)
	}
	t.Cleanup(func() { serverMux.Close() })

	serverMux.NewListener("echo", func(stream siamux.Stream) {
		if req, err := readPrefixedBytes(stream, 1024); err != nil {
			panic(fmt.Errorf("failed to read object from stream: %w", err))
		} else if err := writePrefixedBytes(stream, req); err != nil {
			panic(fmt.Errorf("failed to write object to stream: %w", err))
		}
	})

	conn, err := net.Dial("tcp", serverMux.Address().String())
	if err != nil {
		t.Fatal("failed to dial sia mux:", err)
	}
	t.Cleanup(func() { conn.Close() })

	serverKey := serverMux.PublicKey()
	m, err := DialSubscriber(conn, serverKey[:])
	if err != nil {
		t.Fatal("failed to dial subscriber mux:", err)
	}
	t.Cleanup(func() { m.Close() })

	t.Run("bad subscriber", func(t *testing.T) {
		s, err := m.NewSubscriberStream("bad sub")
		if err != nil {
			t.Fatal("failed to create subscriber stream:", err)
		}

		if err := writePrefixedBytes(s, []byte("hello")); err != nil {
			t.Fatal("failed to write to stream:", err)
		} else if _, err := readPrefixedBytes(s, 1024); err == nil {
			t.Fatal("expected subscriber error:", err)
		}
	})

	t.Run("echo subscriber", func(t *testing.T) {
		s, err := m.NewSubscriberStream("echo")
		if err != nil {
			t.Fatal("failed to create subscriber stream:", err)
		}

		req := frand.Bytes(128)
		if err := writePrefixedBytes(s, req); err != nil {
			t.Fatal("failed to write object to stream:", err)
		} else if resp, err := readPrefixedBytes(s, 1024); err != nil {
			t.Fatal("failed to read subscriber reply:", err)
		} else if !bytes.Equal(req, resp) {
			t.Fatalf("unexpected reply: got %v expected %v", resp, req)
		}
	})
}
