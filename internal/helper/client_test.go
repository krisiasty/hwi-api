package helper

import (
	"bufio"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// socketPath is a socket path in a fresh directory. t.TempDir's paths can
// exceed macOS's 104-byte limit on socket paths, so this uses a shorter one.
// local answers from root, a host root, in this process, exactly as the
// helper would.
func local(root fs.FS) fs.FS {
	h := Handler{Root: root, ReadTimeout: DefaultReadTimeout}
	return NewFS(func(req Request) (Response, error) {
		resp, _ := h.Handle(req)
		return resp, nil
	})
}

func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "helper")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

// listen serves each connection on a Unix socket with h, as one helper
// instance would, and returns the socket and a count of connections.
func listen(t *testing.T, h Handler) (string, *atomic.Int32) {
	t.Helper()
	sock := socketPath(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var conns atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer c.Close()
				_ = h.Serve(c)
			}()
		}
	}()
	return sock, &conns
}

func TestClientReadsThroughTheSocket(t *testing.T) {
	sock, _ := listen(t, Handler{Root: hostRoot(), ReadTimeout: time.Second})
	fsys := (&Client{Socket: sock}).FS()
	if b, err := fs.ReadFile(fsys, "product_serial"); err != nil || string(b) != "EXSERIAL0001\n" {
		t.Errorf("product_serial = %q, %v", b, err)
	}
	if b, err := fs.ReadFile(fsys, "vpd/0000:3b:00.0"); err != nil || len(b) != 5 {
		t.Errorf("vpd = %q, %v", b, err)
	}
	if info, err := fs.Stat(fsys, "product_uuid"); err != nil || !info.Mode().IsRegular() || info.Name() != "product_uuid" {
		t.Errorf("Stat = %v, %v; want a regular file named product_uuid", info, err)
	}
}

func TestClientFSReportsAMissingFileAsNotExist(t *testing.T) {
	sock, _ := listen(t, Handler{Root: hostRoot(), ReadTimeout: time.Second})
	if _, err := fs.ReadFile((&Client{Socket: sock}).FS(), "board_serial"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("board_serial: %v, want fs.ErrNotExist", err)
	}
}

// Names outside the operations never reach the helper.
func TestClientFSRefusesOtherNames(t *testing.T) {
	fsys := NewFS(func(req Request) (Response, error) {
		t.Errorf("sent %+v", req)
		return Response{}, nil
	})
	for _, name := range []string{"etc/shadow", "vpd/../DMI", "vpd/3b:00.0", "vpd", ".", "board_asset_tag"} {
		if _, err := fs.ReadFile(fsys, name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: %v, want fs.ErrNotExist", name, err)
		}
	}
}

// Review focus 1: a missing socket is ENOENT, which must not read as a
// missing file.
func TestClientWithoutAHelperIsUnavailableNotMissing(t *testing.T) {
	_, err := fs.ReadFile((&Client{Socket: socketPath(t)}).FS(), "product_uuid")
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v; want ErrUnavailable and not fs.ErrNotExist", err)
	}
}

// Review focus 2: an idle connection whose instance has exited is redialled
// once.
func TestClientRedialsAfterTheHelperExits(t *testing.T) {
	sock := socketPath(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	h := Handler{Root: hostRoot(), ReadTimeout: time.Second}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			// Answer one request, then exit, as an instance ended by
			// RuntimeMaxSec= would.
			line, _ := bufio.NewReader(c).ReadBytes('\n')
			req, _ := decodeRequest(line)
			resp, _ := h.Handle(req)
			b, _ := jsonLine(resp)
			_, _ = c.Write(b)
			_ = c.Close()
		}
	}()
	c := &Client{Socket: sock, IdleTimeout: time.Minute}
	t.Cleanup(func() { c.Close() })
	for i := range 3 {
		if _, err := fs.ReadFile(c.FS(), "vpd/0000:3b:00.0"); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
}

func TestClientTimesOutASilentHelper(t *testing.T) {
	sock := socketPath(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	held := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			held <- c // never answered
		}
	}()
	t.Cleanup(func() {
		for {
			select {
			case c := <-held:
				c.Close()
			default:
				return
			}
		}
	})
	start := time.Now()
	_, err = fs.ReadFile((&Client{Socket: sock, Timeout: 100 * time.Millisecond}).FS(), "DMI")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s; a timed-out request must not be retried", d)
	}
}

// Review focus 3: after a timeout answer the helper exits, so the client
// starts the next request on a new connection.
func TestClientDropsTheConnectionAfterATimeoutAnswer(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	sock, conns := listen(t, Handler{Root: hangingFS{hostRoot(), devVPD, release}, ReadTimeout: 50 * time.Millisecond})
	c := &Client{Socket: sock, IdleTimeout: time.Minute}
	t.Cleanup(func() { c.Close() })
	_, err := fs.ReadFile(c.FS(), "vpd/0000:3b:00.0")
	if err == nil || errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), ErrCodeTimeout) {
		t.Errorf("hung read: %v, want the helper's %s answer", err, ErrCodeTimeout)
	}
	if _, err := fs.ReadFile(c.FS(), "product_serial"); err != nil {
		t.Fatalf("next read: %v", err)
	}
	if n := conns.Load(); n != 2 {
		t.Errorf("%d connections, want 2", n)
	}
}

func TestClientReusesThenClosesIdleConnections(t *testing.T) {
	sock, conns := listen(t, Handler{Root: hostRoot(), ReadTimeout: time.Second})
	c := &Client{Socket: sock, IdleTimeout: time.Second}
	t.Cleanup(func() { c.Close() })
	for range 3 {
		if _, err := fs.ReadFile(c.FS(), "vpd/0000:3b:00.0"); err != nil {
			t.Fatal(err)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Errorf("a burst of 3 requests used %d connections, want 1", n)
	}
	time.Sleep(2500 * time.Millisecond)
	if _, err := fs.ReadFile(c.FS(), "vpd/0000:3b:00.0"); err != nil {
		t.Fatal(err)
	}
	if n := conns.Load(); n != 2 {
		t.Errorf("%d connections after an idle pause, want 2", n)
	}
}

func TestLocalAnswersAsTheHelperDoes(t *testing.T) {
	fsys := local(hostRoot())
	if b, err := fs.ReadFile(fsys, "DMI"); err != nil || len(b) != 6 {
		t.Errorf("DMI = %v, %v", b, err)
	}
	if _, err := fs.ReadFile(fsys, "board_serial"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("board_serial: %v, want fs.ErrNotExist", err)
	}
}

// A refused or failed read is an AnswerError carrying the helper's code,
// so callers can tell a hung read from a missing or faulty one.
func TestFSReportsTheHelpersAnswer(t *testing.T) {
	fsys := NewFS(func(Request) (Response, error) {
		return Response{Error: ErrCodeTimeout, Message: "the read did not finish in time"}, nil
	})
	_, err := fs.ReadFile(fsys, "vpd/0000:3b:00.0")
	var answer *AnswerError
	if !errors.As(err, &answer) || answer.Code != ErrCodeTimeout {
		t.Fatalf("err = %v; want an AnswerError with code %s", err, ErrCodeTimeout)
	}
	if want := "the helper answered timeout: the read did not finish in time"; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to contain %q", err, want)
	}
}

// countingHandler counts the requests reaching a helper, not connections.
func countingHandler(n *atomic.Int32, h Handler) Handler {
	h.Root = countingFS{FS: h.Root, n: n}
	return h
}

type countingFS struct {
	fs.FS
	n *atomic.Int32
}

func (c countingFS) Open(name string) (fs.File, error) {
	c.n.Add(1)
	return c.FS.Open(name)
}

func TestClientFSAsksForADMIFileOnce(t *testing.T) {
	var n atomic.Int32
	sock, _ := listen(t, countingHandler(&n, Handler{Root: hostRoot(), ReadTimeout: time.Second}))
	fsys := (&Client{Socket: sock}).FS()
	a, err1 := fs.ReadFile(fsys, "product_serial")
	b, err2 := fs.ReadFile(fsys, "product_serial")
	if err1 != nil || err2 != nil || string(a) != "EXSERIAL0001\n" || string(a) != string(b) {
		t.Fatalf("reads = %q, %v; %q, %v", a, err1, b, err2)
	}
	if got := n.Load(); got != 1 {
		t.Errorf("helper reads = %d, want 1", got)
	}
}

func TestClientFSAsksForAMissingDMIFileOnce(t *testing.T) {
	var n atomic.Int32
	sock, _ := listen(t, countingHandler(&n, Handler{Root: hostRoot(), ReadTimeout: time.Second}))
	fsys := (&Client{Socket: sock}).FS()
	for range 2 {
		if _, err := fs.ReadFile(fsys, "board_serial"); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("board_serial: %v, want fs.ErrNotExist", err)
		}
	}
	if got := n.Load(); got != 1 {
		t.Errorf("helper reads = %d, want 1", got)
	}
}

func TestClientFSDoesNotCacheAnUnavailableHelper(t *testing.T) {
	sock := socketPath(t)
	fsys := (&Client{Socket: sock}).FS()
	if _, err := fs.ReadFile(fsys, "product_serial"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("without a helper: %v, want ErrUnavailable", err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = Handler{Root: hostRoot(), ReadTimeout: time.Second}.Serve(c)
			}()
		}
	}()
	if b, err := fs.ReadFile(fsys, "product_serial"); err != nil || string(b) != "EXSERIAL0001\n" {
		t.Errorf("with a helper: %q, %v", b, err)
	}
}

func TestClientFSDoesNotCacheAnErrorAnswer(t *testing.T) {
	var calls int
	c := &Client{}
	fsys := c.fsOver(func(Request) (Response, error) {
		calls++
		if calls == 1 {
			return Response{Error: ErrCodeTimeout, Message: "hung"}, nil
		}
		return Response{Data: []byte("ok\n")}, nil
	})
	var ae *AnswerError
	if _, err := fs.ReadFile(fsys, "product_serial"); !errors.As(err, &ae) {
		t.Fatalf("first read: %v, want an AnswerError", err)
	}
	if b, err := fs.ReadFile(fsys, "product_serial"); err != nil || string(b) != "ok\n" {
		t.Errorf("second read: %q, %v", b, err)
	}
}

func TestClientFSAlwaysAsksForVPD(t *testing.T) {
	var n atomic.Int32
	sock, _ := listen(t, countingHandler(&n, Handler{Root: hostRoot(), ReadTimeout: time.Second}))
	fsys := (&Client{Socket: sock}).FS()
	for range 2 {
		if _, err := fs.ReadFile(fsys, "vpd/0000:3b:00.0"); err != nil {
			t.Fatal(err)
		}
	}
	if got := n.Load(); got != 2 {
		t.Errorf("helper reads = %d, want 2", got)
	}
}

func TestClientFSCacheIsSafeForConcurrentUse(t *testing.T) {
	var n atomic.Int32
	sock, _ := listen(t, countingHandler(&n, Handler{Root: hostRoot(), ReadTimeout: time.Second}))
	fsys := (&Client{Socket: sock}).FS()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b, err := fs.ReadFile(fsys, "product_serial"); err != nil || string(b) != "EXSERIAL0001\n" {
				t.Errorf("read: %q, %v", b, err)
			}
		}()
	}
	wg.Wait()
	if got := n.Load(); got != 1 {
		t.Errorf("helper reads = %d, want 1", got)
	}
}

// Close drops the connection, so the helper's instance sees it end, and the
// Client dials again for a later request.
func TestClientCloseEndsTheConnectionAndAllowsAnotherRequest(t *testing.T) {
	sock := socketPath(t)
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	served := make(chan error, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				served <- Handler{Root: hostRoot(), ReadTimeout: time.Second}.Serve(c)
			}()
		}
	}()
	c := &Client{Socket: sock, IdleTimeout: time.Minute}
	t.Cleanup(func() { c.Close() })
	if _, err := fs.ReadFile(c.FS(), "vpd/0000:3b:00.0"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the connection close")
	}
	if _, err := fs.ReadFile(c.FS(), "vpd/0000:3b:00.0"); err != nil {
		t.Fatalf("read after Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
