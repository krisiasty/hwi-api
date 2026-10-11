package helper

import (
	"sync"
)

// dmiCache keeps the helper's answers for DMI names: the SMBIOS table and
// the identity fields. The kernel copies them once at boot and never
// updates them while the host runs, and the daemon restarts with the host,
// so one answer is right for the daemon's lifetime, and asking the helper
// again would only start another instance for the same bytes. VPD is not
// kept: it can change with a hot-swap or a power state.
//
// Only an answer is kept: data, or ErrCodeNotFound, which says the host
// lacks the file, equally fixed for the boot. A client error, a timeout or
// any other error answer says nothing about the file, so it is passed on
// and the next open asks again. The zero value is ready to use.
type dmiCache struct {
	mu      sync.Mutex
	answers map[string]Response
}

// wrap returns do with the cache in front of it for OpDMI requests. The
// lock is held across the helper's answer, so concurrent opens of one name
// send a single request; the Client serialises its requests anyway.
func (c *dmiCache) wrap(do func(Request) (Response, error)) func(Request) (Response, error) {
	return func(req Request) (Response, error) {
		if req.Op != OpDMI {
			return do(req)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if resp, ok := c.answers[req.Name]; ok {
			return resp, nil
		}
		resp, err := do(req)
		if err != nil || (resp.Error != "" && resp.Error != ErrCodeNotFound) {
			return resp, err
		}
		if c.answers == nil {
			c.answers = make(map[string]Response)
		}
		c.answers[req.Name] = resp
		return resp, nil
	}
}
