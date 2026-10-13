package ethtool

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
)

var (
	// ErrNotSupported is the kernel's EOPNOTSUPP: the driver does not implement the query.
	ErrNotSupported = errors.New("not supported by the driver")
	// ErrNoDevice is the kernel's ENODEV: the interface is gone, its index and name no longer match, or it is still registered but its device
	// is not present (a driver reset, a fatal firmware error, suspend). A name no interface can have gets it too, without being sent.
	ErrNoDevice = errors.New("no such device")
	// ErrNoFamily means the generic-netlink controller does not know the ethtool family: the kernel is older than 5.6, or was built without
	// CONFIG_ETHTOOL_NETLINK.
	ErrNoFamily = errors.New("the kernel has no ethtool netlink family (Linux 5.6+ with CONFIG_ETHTOOL_NETLINK)")
	// ErrBusy means Dial opened no session because one session is open at a time, process-wide, and another still holds that place: it is
	// open, or it was closed but one of its requests timed out and is still blocked in the kernel, such as behind a stalled driver. Nothing
	// can cancel such a request, and asking again would only block another thread.
	ErrBusy = errors.New("another ethtool session is open, or a previous request is still blocked in the kernel")
)

// Feature is one offload feature as `ethtool -k` shows it.
type Feature struct {
	Active    bool // currently on
	Requested bool // wanted on by the administrator
	Fixed     bool // cannot be changed: `ethtool -k` prints [fixed]
}

// Link is an interface's link modes and port.
type Link struct {
	Port      string // "" when LINKINFO is unsupported
	Autoneg   *bool  // nil when the reply does not say
	SpeedMbps uint32 // 0 = unknown
	Duplex    string // "", "full", "half"
	Lanes     uint32 // 0 = unknown

	// Link-mode names (such as 25000baseCR/Full) in the kernel's bit order; never nil once the link modes are parsed.
	Supported, Advertised, Partner []string
}

// FEC is an interface's forward error correction setting.
type FEC struct {
	Configured []string // FEC mode names; never nil
	Active     string   // "" when no mode is active
	Auto       *bool    // nil when the reply does not say
}

// Generic netlink, from include/uapi/linux/genetlink.h and netlink.h.
const (
	nlmFRequest = 1 // NLM_F_REQUEST

	genlHdrLen  = 4 // genlmsghdr: cmd u8, version u8, reserved u16
	genlVersion = 1 // the ethtool family's version; the controller accepts any

	nlmsgMinType = 0x10 // NLMSG_MIN_TYPE: types below it are netlink control messages (NOOP, ERROR, DONE, OVERRUN)

	genlIDCtrl         = 0x10 // GENL_ID_CTRL
	ctrlCmdNewFamily   = 1    // CTRL_CMD_NEWFAMILY: the controller's reply to GETFAMILY
	ctrlCmdGetFamily   = 3    // CTRL_CMD_GETFAMILY
	ctrlAttrFamilyID   = 1    // CTRL_ATTR_FAMILY_ID (u16)
	ctrlAttrFamilyName = 2    // CTRL_ATTR_FAMILY_NAME (string)

	ethtoolFamilyName = "ethtool"
)

// The ethtool family's commands and attributes, from include/uapi/linux/ethtool_netlink_generated.h.
const (
	ethtoolMsgLinkInfoGet       = 2
	ethtoolMsgLinkInfoGetReply  = 2
	ethtoolMsgLinkModesGet      = 4
	ethtoolMsgLinkModesGetReply = 4
	ethtoolMsgFeaturesGet       = 11
	ethtoolMsgFeaturesGetReply  = 11
	ethtoolMsgFECGet            = 29
	ethtoolMsgFECGetReply       = 30

	ethtoolAHeader         = 1 // every message's ETHTOOL_A_*_HEADER nest
	ethtoolAHeaderDevIndex = 1 // ETHTOOL_A_HEADER_DEV_INDEX (u32)
	ethtoolAHeaderDevName  = 2 // ETHTOOL_A_HEADER_DEV_NAME (NUL-terminated string)
	ethtoolAHeaderFlags    = 3 // ETHTOOL_A_HEADER_FLAGS (u32): 0 asks for verbose bitsets

	ethtoolAFeaturesHW       = 2 // bitsets
	ethtoolAFeaturesWanted   = 3
	ethtoolAFeaturesActive   = 4
	ethtoolAFeaturesNochange = 5

	ethtoolALinkModesAutoneg = 2 // u8
	ethtoolALinkModesOurs    = 3 // bitset: supported modes, the advertised ones set
	ethtoolALinkModesPeer    = 4 // bitset (NOMASK): the partner's advertised modes
	ethtoolALinkModesSpeed   = 5 // u32
	ethtoolALinkModesDuplex  = 6 // u8
	ethtoolALinkModesLanes   = 9 // u32

	ethtoolALinkInfoPort = 2 // u8

	ethtoolAFECModes  = 2 // bitset
	ethtoolAFECAuto   = 3 // u8
	ethtoolAFECActive = 4 // u32: a link-mode bit index
)

// Values from include/uapi/linux/ethtool.h.
const (
	speedUnknown  = 0xFFFFFFFF // SPEED_UNKNOWN
	duplexHalf    = 0x00
	duplexFull    = 0x01
	duplexUnknown = 0xFF
)

// portNames maps ethtool.h's PORT_* values to the names the payload reports.
var portNames = map[uint8]string{
	0x00: "TP",
	0x01: "AUI",
	0x02: "MII",
	0x03: "FIBRE",
	0x04: "BNC",
	0x05: "DA",
	0xEF: "NONE",
	0xFF: "OTHER",
}

// fecModeNames maps ethtool.h's ETHTOOL_LINK_MODE_FEC_*_BIT indices to their link-mode names, for an active mode the configured bitset does not
// list.
var fecModeNames = map[uint32]string{
	49: "None",
	50: "RS",
	51: "BASER",
	74: "LLRS",
}

// Linux errno values. The kernel's codes arrive in the reply whatever the build's GOOS, and syscall's constants differ between platforms
// (EOPNOTSUPP is 102 on Darwin), so the replies are matched against Linux's numbers.
const (
	linuxENOENT     = 2
	linuxENODEV     = 19
	linuxEOPNOTSUPP = 95
)

// ifNameSize is the kernel's IFNAMSIZ: an interface name is at most ifNameSize-1 bytes, and the kernel compares the name in a request with
// the interface's own over at most ifNameSize bytes.
const ifNameSize = 16

// The family request's lengths: all of it but seq is fixed. They are constants, and converting a constant is checked by the compiler, so
// they are known to fit their header fields.
const (
	familyNameAttrLen = nlaHdrLen + len(ethtoolFamilyName) + 1 // the name and its NUL
	familyRequestLen  = nlmsgHdrLen + genlHdrLen + (familyNameAttrLen+3)&^3
)

// familyRequest asks the generic-netlink controller for the ethtool family's id. TestFamilyRequest checks it against the tests' general
// encoders.
func familyRequest(seq uint32) []byte {
	b := appendMessageHeader(make([]byte, 0, familyRequestLen), genlIDCtrl, nlmFRequest, seq, uint32(familyRequestLen))
	b = append(b, ctrlCmdGetFamily, genlVersion, 0, 0)
	b = appendAttrHeader(b, ctrlAttrFamilyName, uint16(familyNameAttrLen))
	b = append(b, ethtoolFamilyName...)
	return b[:familyRequestLen] // the NUL and padding are already zero
}

// parseFamilyID returns the family id from the controller's reply to familyRequest; the controller answers ENOENT for a family it does not know.
func parseFamilyID(msgs []message) (uint16, error) {
	if err := replyError(msgs); err != nil {
		if errors.Is(err, syscall.Errno(linuxENOENT)) {
			return 0, ErrNoFamily
		}
		return 0, fmt.Errorf("family lookup: %w", err)
	}
	attrs, err := replyAttributes(msgs, ctrlCmdNewFamily)
	if err != nil {
		return 0, fmt.Errorf("family lookup: %w", err)
	}
	for _, a := range attrs {
		if a.typ == ctrlAttrFamilyID {
			id, err := a.u16()
			if err != nil {
				return 0, fmt.Errorf("family id: %w", err)
			}
			return id, nil
		}
	}
	return 0, errors.New("family lookup: the reply has no family id")
}

// request returns an ethtool query of one interface. The header names it by both index and name: the kernel answers ENODEV when they do
// not match, so an index reused by another interface since the caller read it is not answered for. Header flags of 0 ask for verbose
// bitsets, which list each bit with its name.
//
// The kernel compares the name with the interface's over at most ifNameSize bytes, and stops at a NUL. So a name no interface can have,
// empty, longer than ifNameSize-1 bytes or holding a NUL, could never match, or would match on its part before the NUL: it gets the kernel's
// own answer, ErrNoDevice, without being sent.
//
// A name that can be an interface's keeps the whole request under 64 bytes, so its lengths are computed rather than checked;
// TestRequestEncodesAsTheFixtures checks it against the tests' general encoders.
func request(family uint16, cmd uint8, seq uint32, ifindex uint32, name string) ([]byte, error) {
	if len(name) == 0 || len(name) >= ifNameSize || strings.IndexByte(name, 0) >= 0 {
		return nil, fmt.Errorf("interface name of %d bytes cannot be an interface's (1 to %d bytes, no NUL): %w", len(name), ifNameSize-1, ErrNoDevice)
	}
	nameLen := nlaHdrLen + len(name) + 1                                         // DEV_NAME: the name and its NUL
	headerLen := nlaHdrLen + (nlaHdrLen + 4) + align4(nameLen) + (nlaHdrLen + 4) // the nest: DEV_INDEX, DEV_NAME, FLAGS
	msgLen := nlmsgHdrLen + genlHdrLen + headerLen

	b := appendMessageHeader(make([]byte, 0, msgLen), family, nlmFRequest, seq, uint32(msgLen))
	b = append(b, cmd, genlVersion, 0, 0)
	b = appendAttrHeader(b, ethtoolAHeader|nlaFNested, uint16(headerLen))
	b = appendU32Attr(b, ethtoolAHeaderDevIndex, ifindex)
	b = appendAttrHeader(b, ethtoolAHeaderDevName, uint16(nameLen))
	b = append(b, name...)
	b = b[:len(b)+align4(nameLen)-nameLen+1] // the NUL and padding, already zero in the capacity
	b = appendU32Attr(b, ethtoolAHeaderFlags, 0)
	return b, nil
}

// replyError returns the first NLMSG_ERROR with a nonzero code as an error: ErrNotSupported, ErrNoDevice, or the syscall.Errno. An ACK (code 0)
// is not an error.
func replyError(msgs []message) error {
	for _, m := range msgs {
		if m.typ != nlmsgError {
			continue
		}
		code, err := errorCode(m)
		if err != nil {
			return err
		}
		switch -code {
		case 0:
			continue
		case linuxEOPNOTSUPP:
			return ErrNotSupported
		case linuxENODEV:
			return ErrNoDevice
		default:
			return syscall.Errno(-code)
		}
	}
	return nil
}

// replyAttributes returns the attributes of the reply's first generic-netlink message carrying cmd, after turning an NLMSG_ERROR in the reply into
// an error. Netlink control messages (such as NLMSG_DONE) and messages carrying other commands are skipped.
func replyAttributes(msgs []message, cmd uint8) ([]attr, error) {
	if err := replyError(msgs); err != nil {
		return nil, err
	}
	for _, m := range msgs {
		if m.typ < nlmsgMinType {
			continue
		}
		if len(m.data) < genlHdrLen {
			return nil, fmt.Errorf("generic netlink message of %d bytes, shorter than its header", len(m.data))
		}
		if m.data[0] != cmd {
			continue
		}
		return parseAttributes(m.data[genlHdrLen:])
	}
	return nil, fmt.Errorf("no reply with generic netlink command %d", cmd)
}

// parseFeatures decodes a FEATURES_GET reply into every feature named in any of its bitsets. Every bitset in the reply is NOMASK, so a feature is
// in a bitset exactly when its bit is set there; a feature that is off and unchangeable is in none and so is absent from the result.
func parseFeatures(msgs []message) (map[string]Feature, error) {
	attrs, err := replyAttributes(msgs, ethtoolMsgFeaturesGetReply)
	if err != nil {
		return nil, err
	}
	var hw, wanted, active, nochange map[string]bool
	for _, a := range attrs {
		var set *map[string]bool
		switch a.typ {
		case ethtoolAFeaturesHW:
			set = &hw
		case ethtoolAFeaturesWanted:
			set = &wanted
		case ethtoolAFeaturesActive:
			set = &active
		case ethtoolAFeaturesNochange:
			set = &nochange
		default:
			continue
		}
		bits, err := parseBitset(a)
		if err != nil {
			return nil, err
		}
		if *set == nil {
			*set = make(map[string]bool, len(bits))
		}
		for _, b := range bits {
			if b.set && b.name != "" {
				(*set)[b.name] = true
			}
		}
	}

	features := make(map[string]Feature)
	for _, set := range []map[string]bool{hw, wanted, active, nochange} {
		for name := range set {
			features[name] = Feature{
				Active:    active[name],
				Requested: wanted[name],
				Fixed:     !hw[name] || nochange[name],
			}
		}
	}
	return features, nil
}

// parseLinkModes decodes a LINKMODES_GET reply into l. OURS lists every supported mode and sets the advertised ones; PEER (NOMASK) lists the
// partner's advertised modes. Only real link modes are kept (see isLinkMode).
func parseLinkModes(msgs []message, l *Link) error {
	attrs, err := replyAttributes(msgs, ethtoolMsgLinkModesGetReply)
	if err != nil {
		return err
	}
	l.Supported, l.Advertised, l.Partner = []string{}, []string{}, []string{}
	for _, a := range attrs {
		switch a.typ {
		case ethtoolALinkModesAutoneg:
			v, err := a.u8()
			if err != nil {
				return fmt.Errorf("autoneg: %w", err)
			}
			on := v != 0
			l.Autoneg = &on
		case ethtoolALinkModesOurs, ethtoolALinkModesPeer:
			bits, err := parseBitset(a)
			if err != nil {
				return err
			}
			for _, b := range bits {
				if !isLinkMode(b.name) {
					continue
				}
				if a.typ == ethtoolALinkModesPeer {
					if b.set {
						l.Partner = append(l.Partner, b.name)
					}
					continue
				}
				l.Supported = append(l.Supported, b.name)
				if b.set {
					l.Advertised = append(l.Advertised, b.name)
				}
			}
		case ethtoolALinkModesSpeed:
			v, err := a.u32()
			if err != nil {
				return fmt.Errorf("speed: %w", err)
			}
			if v == speedUnknown {
				v = 0
			}
			l.SpeedMbps = v
		case ethtoolALinkModesDuplex:
			v, err := a.u8()
			if err != nil {
				return fmt.Errorf("duplex: %w", err)
			}
			switch v {
			case duplexHalf:
				l.Duplex = "half"
			case duplexFull:
				l.Duplex = "full"
			default:
				l.Duplex = ""
			}
		case ethtoolALinkModesLanes:
			if l.Lanes, err = a.u32(); err != nil {
				return fmt.Errorf("lanes: %w", err)
			}
		}
	}
	return nil
}

// isLinkMode reports whether a link-mode bitset name is a real link mode (such as 25000baseCR/Full) rather than one of the bitset's other bits
// (TP, FIBRE, Autoneg, Pause, Asym_Pause, the FEC names).
func isLinkMode(name string) bool {
	return name != "" && name[0] >= '0' && name[0] <= '9' && strings.Contains(name, "base")
}

// parseLinkInfo decodes a LINKINFO_GET reply's port into l.Port: a PORT_* name, or "unknown" for a value ethtool.h does not define.
func parseLinkInfo(msgs []message, l *Link) error {
	attrs, err := replyAttributes(msgs, ethtoolMsgLinkInfoGetReply)
	if err != nil {
		return err
	}
	for _, a := range attrs {
		if a.typ != ethtoolALinkInfoPort {
			continue
		}
		v, err := a.u8()
		if err != nil {
			return fmt.Errorf("port: %w", err)
		}
		name, ok := portNames[v]
		if !ok {
			name = "unknown"
		}
		l.Port = name
	}
	return nil
}

// parseFEC decodes a FEC_GET reply. The active mode is a link-mode bit index, named from the configured bitset's names, else from the FEC table,
// else "unknown".
func parseFEC(msgs []message) (*FEC, error) {
	attrs, err := replyAttributes(msgs, ethtoolMsgFECGetReply)
	if err != nil {
		return nil, err
	}
	fec := &FEC{Configured: []string{}}
	names := map[uint32]string{}
	var active uint32
	hasActive := false
	for _, a := range attrs {
		switch a.typ {
		case ethtoolAFECModes:
			bits, err := parseBitset(a)
			if err != nil {
				return nil, err
			}
			for _, b := range bits {
				if b.name == "" {
					continue
				}
				names[b.index] = b.name
				if b.set {
					fec.Configured = append(fec.Configured, b.name)
				}
			}
		case ethtoolAFECAuto:
			v, err := a.u8()
			if err != nil {
				return nil, fmt.Errorf("auto: %w", err)
			}
			on := v != 0
			fec.Auto = &on
		case ethtoolAFECActive:
			if active, err = a.u32(); err != nil {
				return nil, fmt.Errorf("active: %w", err)
			}
			hasActive = true
		}
	}
	if hasActive {
		fec.Active = "unknown"
		if name, ok := names[active]; ok {
			fec.Active = name
		} else if name, ok := fecModeNames[active]; ok {
			fec.Active = name
		}
	}
	return fec, nil
}
