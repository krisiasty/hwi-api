package helper

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/user"
	"strconv"
)

// Main runs one helper instance, as `hwi-api helper`: it answers the
// connection systemd accepted for it, if that connection comes from
// --peer-user, and returns when the peer closes it or ctx is done.
//
// ctx matters because systemd stops an instance with SIGTERM, which main
// has taken over for a graceful shutdown: without ctx the helper would sit
// in a read on an idle connection until SIGKILL.
func Main(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("hwi-api helper", flag.ContinueOnError)
	flags.SetOutput(stderr)
	peer := flags.String("peer-user", "hwi-api", "the only user whose connections are answered")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q: hwi-api helper takes options only", flags.Arg(0))
	}

	conn, err := activatedConn(os.Getenv, os.Getpid(), 3)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	return serve(ctx, conn, *peer, os.DirFS("/"))
}

// serve answers conn from root if its peer is peerUser, and closes conn when
// ctx is done. A stop is a clean exit, not an error.
func serve(ctx context.Context, conn *net.UnixConn, peerUser string, root fs.FS) error {
	want, err := user.Lookup(peerUser)
	if err != nil {
		return fmt.Errorf("looking up %s: %w", peerUser, err)
	}
	got, err := peerUID(conn)
	if err != nil {
		return fmt.Errorf("reading the peer's credentials: %w", err)
	}
	if strconv.Itoa(got) != want.Uid {
		return fmt.Errorf("refused a connection from uid %d: only %s (uid %s) is answered", got, peerUser, want.Uid)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := (Handler{Root: root, ReadTimeout: DefaultReadTimeout}).Serve(conn); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
