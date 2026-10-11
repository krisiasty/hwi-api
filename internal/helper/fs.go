package helper

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"
)

// NewFS presents the operations do answers as a read-only file system:
// each DMI name by its name, and vpd/<PCI address> for a device's
// VPD. Opening a name sends its request, so every open reads the hardware
// again. A file the host lacks, and any other name, is fs.ErrNotExist. An
// error from do is kept, so a helper that does not answer never reads as a
// missing file. A refused or failed read is an *AnswerError.
func NewFS(do func(Request) (Response, error)) fs.FS { return helperFS(do) }

type helperFS func(Request) (Response, error)

func (do helperFS) Open(name string) (fs.File, error) {
	req, ok := requestFor(name)
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	resp, err := do(req)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	switch resp.Error {
	case "":
		return &file{Reader: bytes.NewReader(resp.Data), info: fileInfo{path.Base(name), int64(len(resp.Data))}}, nil
	case ErrCodeNotFound:
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	default:
		return nil, &fs.PathError{Op: "open", Path: name, Err: &AnswerError{Code: resp.Error, Message: resp.Message}}
	}
}

// AnswerError is the helper's answer refusing or failing a request, other
// than a file the host lacks: Code is its error code, such as
// ErrCodeTimeout, so a caller can tell a hung read from a faulty one.
type AnswerError struct {
	Code    string
	Message string
}

func (e *AnswerError) Error() string {
	return fmt.Sprintf("the helper answered %s: %s", e.Code, e.Message)
}

// requestFor maps a file name to its request.
func requestFor(name string) (Request, bool) {
	if _, ok := DMIFile(name); ok {
		return Request{Op: OpDMI, Name: name}, true
	}
	if addr, ok := strings.CutPrefix(name, "vpd/"); ok && pciAddress.MatchString(addr) {
		return Request{Op: OpVPD, Address: addr}, true
	}
	return Request{}, false
}

type file struct {
	*bytes.Reader
	info fileInfo
}

func (f *file) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *file) Close() error               { return nil }

type fileInfo struct {
	name string
	size int64
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return 0o444 }
func (i fileInfo) ModTime() time.Time { return time.Time{} }
func (i fileInfo) IsDir() bool        { return false }
func (i fileInfo) Sys() any           { return nil }
