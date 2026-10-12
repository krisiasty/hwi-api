package ethtool

import (
	"errors"
	"fmt"
)

// nlmsgDone is NLMSG_DONE, which ends a dump; the GET requests here are not dumps, but a reply ending in one is still complete.
const nlmsgDone = 3

// conn sends one request and returns every message of its reply, through NLMSG_DONE, an NLMSG_ERROR, or the single reply message.
type conn interface {
	roundTrip(req []byte, seq uint32) ([]message, error)
	close() error
}

// Client is one netlink session with the ethtool family. It is not safe for concurrent use; a probe opens one, asks for every interface, and
// closes it.
//
// Every query's error, whether the socket's or the kernel's, is wrapped once with the query's name ("features", "link modes", "link info",
// "fec"), so that it reads on its own; errors.Is still finds ErrNotSupported, ErrNoDevice or os.ErrDeadlineExceeded through it.
//
// Every query names the interface by both its index and its name, as sysfs lists them; the kernel answers ErrNoDevice when they do not
// match, such as after a rename, rather than about whichever interface now has the index.
type Client struct {
	c      conn
	family uint16
	seq    uint32
}

// newClient resolves the ethtool family over c, closing c if that fails.
func newClient(c conn) (*Client, error) {
	cl := &Client{c: c}
	cl.seq++
	req, err := familyRequest(cl.seq)
	var msgs []message
	if err == nil {
		msgs, err = c.roundTrip(req, cl.seq)
	}
	if err == nil {
		cl.family, err = parseFamilyID(msgs)
	}
	if err != nil {
		return nil, errors.Join(err, c.close())
	}
	return cl, nil
}

// query sends one ethtool GET of the interface under the next sequence number and returns its reply.
func (c *Client) query(cmd uint8, ifindex uint32, name string) ([]message, error) {
	c.seq++
	req, err := request(c.family, cmd, c.seq, ifindex, name)
	if err != nil {
		return nil, err
	}
	return c.c.roundTrip(req, c.seq)
}

// Features returns the interface's offload features, keyed by the kernel's feature names.
func (c *Client) Features(ifindex uint32, name string) (map[string]Feature, error) {
	msgs, err := c.query(ethtoolMsgFeaturesGet, ifindex, name)
	if err == nil {
		var features map[string]Feature
		if features, err = parseFeatures(msgs); err == nil {
			return features, nil
		}
	}
	return nil, fmt.Errorf("features: %w", err)
}

// Link returns the interface's link modes and port. The port comes from a second query, LINKINFO, which some drivers do not implement; their
// link modes are still returned, with Port "".
func (c *Client) Link(ifindex uint32, name string) (*Link, error) {
	l := &Link{}
	msgs, err := c.query(ethtoolMsgLinkModesGet, ifindex, name)
	if err == nil {
		err = parseLinkModes(msgs, l)
	}
	if err != nil {
		return nil, fmt.Errorf("link modes: %w", err)
	}
	msgs, err = c.query(ethtoolMsgLinkInfoGet, ifindex, name)
	if err == nil {
		if err = parseLinkInfo(msgs, l); errors.Is(err, ErrNotSupported) {
			err = nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("link info: %w", err)
	}
	return l, nil
}

// FEC returns the interface's forward error correction setting.
func (c *Client) FEC(ifindex uint32, name string) (*FEC, error) {
	msgs, err := c.query(ethtoolMsgFECGet, ifindex, name)
	if err == nil {
		var fec *FEC
		if fec, err = parseFEC(msgs); err == nil {
			return fec, nil
		}
	}
	return nil, fmt.Errorf("fec: %w", err)
}

// Close closes the netlink socket.
func (c *Client) Close() error {
	return c.c.close()
}

// replyMessages returns the messages of one receive that answer request seq, and whether they complete its reply. Messages with another seq are
// dropped. That is defensive: a request that timed out poisons its connection, so no later request shares the socket with its late reply, but
// were one to arrive, the parse functions, which take the first message carrying their command, would read a stale reply. A GET that is not a
// dump is answered by a single message, an NLMSG_ERROR, or (for completeness) NLMSG_DONE.
func replyMessages(msgs []message, seq uint32) (reply []message, done bool) {
	for _, m := range msgs {
		if m.seq != seq {
			continue
		}
		reply = append(reply, m)
		if m.typ == nlmsgError || m.typ == nlmsgDone || m.typ >= nlmsgMinType {
			done = true
		}
	}
	return reply, done
}
