package node

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"

	"github.com/justin06lee/henri/internal/config"
	"github.com/justin06lee/henri/internal/secure"
)

// ErrNotRunning means nothing is listening on the daemon's port locally.
//
// No "henri: " in the text: main() adds that prefix to whatever error reaches
// it, and commands that let this one through unhandled -- `henri send` -- were
// printing "henri: henri: the daemon is not running".
var ErrNotRunning = errors.New("the daemon is not running (start it with `henri daemon`)")

// Query sends a control message to the daemon on this machine and returns the
// reply. It reuses the group key, so a process that cannot read the config
// cannot drive the daemon.
func Query(cfg *config.Config, kind string) (*Message, error) {
	return query(cfg, kind, false)
}

// QueryPush asks the daemon to send the clipboard, or the highlighted text.
func QueryPush(cfg *config.Config, primary bool) (*Message, error) {
	return query(cfg, KindPush, primary)
}

func query(cfg *config.Config, kind string, primary bool) (*Message, error) {
	master, err := cfg.MasterKey()
	if err != nil {
		return nil, err
	}
	box, err := secure.NewBox(master, cfg.GroupID, secure.InfoSync)
	if err != nil {
		return nil, err
	}

	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(cfg.ListenPort))
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil, ErrNotRunning
		}
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	req := &Message{
		V:       ProtocolVersion,
		Kind:    kind,
		Device:  cfg.DeviceID,
		Name:    cfg.DeviceName,
		TS:      time.Now().UnixMilli(),
		Primary: primary,
	}
	if err := writeFrame(conn, box, req); err != nil {
		return nil, err
	}
	resp, _, err := readFrame(conn, box, frameLimit(cfg.MaxBytes))
	if err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	return resp, nil
}
