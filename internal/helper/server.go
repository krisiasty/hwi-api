package helper

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/krisiasty/hwi-api/internal/pciaddr"
)

// Handler answers requests from Root, the host root, bounding each read by
// ReadTimeout.
type Handler struct {
	Root fs.FS
	// ReadTimeout bounds one read; zero or less means DefaultReadTimeout.
	ReadTimeout time.Duration
}

var (
	errTimeout  = errors.New("the read did not finish in time")
	errTooLarge = errors.New("the file is larger than the helper answers")
)

// Handle answers req. fatal reports a read that did not finish: its
// goroutine is still blocked in the kernel, so the process must exit after
// answering.
func (h Handler) Handle(req Request) (resp Response, fatal bool) {
	name, limit, err := resolve(req)
	if err != nil {
		return Response{Error: ErrCodeInvalid, Message: err.Error()}, false
	}
	data, err := h.read(name, limit)
	switch {
	case err == nil:
		return Response{Data: data}, false
	case errors.Is(err, errTimeout):
		return Response{Error: ErrCodeTimeout, Message: err.Error()}, true
	case errors.Is(err, errTooLarge):
		return Response{Error: ErrCodeTooLarge, Message: err.Error()}, false
	case errors.Is(err, fs.ErrNotExist):
		return Response{Error: ErrCodeNotFound}, false
	default:
		return Response{Error: ErrCodeRead, Message: err.Error()}, false
	}
}

// resolve maps req to the file it reads, relative to the host root, and
// that file's size cap.
func resolve(req Request) (string, int64, error) {
	switch req.Op {
	case OpDMI:
		name, ok := DMIFile(req.Name)
		if !ok {
			return "", 0, fmt.Errorf("unknown DMI name %q", req.Name)
		}
		if req.Name == SMBIOSTable {
			return name, maxSMBIOSTable, nil
		}
		return name, maxDMIField, nil
	case OpVPD:
		if !pciaddr.Valid(req.Address) {
			return "", 0, fmt.Errorf("%q is not a PCI address", req.Address)
		}
		return "sys/bus/pci/devices/" + req.Address + "/vpd", maxVPD, nil
	default:
		return "", 0, fmt.Errorf("unknown operation %q", req.Op)
	}
}

// read reads name, up to limit bytes, giving up after the handler's read
// timeout (see Handler.ReadTimeout). A read it gives up on keeps its
// goroutine, which is why Handle reports it fatal.
func (h Handler) read(name string, limit int64) ([]byte, error) {
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := readLimited(h.Root, name, limit)
		done <- result{data, err}
	}()
	limitTime := h.ReadTimeout
	if limitTime <= 0 {
		limitTime = DefaultReadTimeout
	}
	timer := time.NewTimer(limitTime)
	defer timer.Stop()
	select {
	case r := <-done:
		return r.data, r.err
	case <-timer.C:
		return nil, fmt.Errorf("%w: %s, after %s", errTimeout, name, limitTime)
	}
}

func readLimited(root fs.FS, name string, limit int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error loses nothing
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s holds more than %d bytes", errTooLarge, name, limit)
	}
	return data, nil
}

// Serve answers the requests on conn, one per line, until the peer closes
// it, which returns nil. A malformed request is refused and ends the
// connection, as does a read that did not finish; both return an error.
func (h Handler) Serve(conn io.ReadWriter) error {
	r := bufio.NewReaderSize(conn, maxRequestBytes)
	enc := json.NewEncoder(conn)
	refuse := func(why string, err error) error {
		_ = enc.Encode(Response{Error: ErrCodeInvalid, Message: why})
		return fmt.Errorf("%s: %w", why, err)
	}
	for {
		line, err := r.ReadSlice('\n')
		switch {
		case errors.Is(err, io.EOF) && len(line) == 0:
			return nil
		case errors.Is(err, bufio.ErrBufferFull):
			return refuse("request too long", err)
		case errors.Is(err, io.EOF):
			return refuse("request cut off", io.ErrUnexpectedEOF)
		case err != nil:
			return err
		}
		req, err := decodeRequest(line)
		if err != nil {
			return refuse("malformed request", err)
		}
		resp, fatal := h.Handle(req)
		if err := enc.Encode(resp); err != nil {
			return err
		}
		if fatal {
			return errors.New(resp.Message)
		}
	}
}

// decodeRequest decodes one request line: a single JSON object with no
// field the protocol does not define and nothing after it.
func decodeRequest(line []byte) (Request, error) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	var req Request
	if err := dec.Decode(&req); err != nil {
		return Request{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Request{}, errors.New("data after the request")
	}
	return req, nil
}
