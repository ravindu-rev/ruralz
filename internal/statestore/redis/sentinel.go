// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// errNoPrimary reports a sentinel that names no primary for the master
// set.
var errNoPrimary = errors.New("redis: the sentinel names no primary")

// replyError is an error reply to one of the driver's own commands; its
// first word classifies it in logs (errType).
type replyError struct{ msg string }

func (e *replyError) Error() string { return "redis: error reply " + e.word() }

// word returns the reply's first word, such as WRONGPASS.
func (e *replyError) word() string {
	w, _, _ := strings.Cut(e.msg, " ")
	return w
}

// maxReplyDepth bounds the nesting of a reply the sentinel check passes
// over, a HELLO answer's modules among them.
const maxReplyDepth = 4

// checkSentinels starts the sentinel topology's periodic check, every
// ShardsRefresh (10 s, target) of the driver's clock and one at a time, on
// a goroutine of its own so a sentinel that does not answer never holds up
// the maintenance pass: the driver asks the sentinel seeds itself for the
// master set's primary (askSentinels), and only when they name a primary
// other than the one the State client sends to does it close the client's
// sentinel connections, so the client asks the sentinels again and moves
// there (docs/architecture/11-scalability-and-distributed-state.md "State
// client and RZ-STS error codes"). The client itself asks only at start,
// on +switch-master and when its sentinel connection closes: a periodic
// ask of its own would check the primary it holds with ROLE, without a
// deadline and under the lock its +switch-master handling needs, so a
// frozen or black-holed primary would hold up a failover the sentinels
// announce until that connection's write timeout ended it.
func (d *Driver) checkSentinels(ctx context.Context, c rueidis.Client) {
	if d.topo != statestore.TopologySentinel {
		return
	}
	now := d.clk.Now()
	if d.sentinelCheckedAt.IsZero() {
		// The client was created from the sentinels' answer just now.
		d.sentinelCheckedAt = now
		return
	}
	if now.Sub(d.sentinelCheckedAt) < d.lim.ShardsRefresh || !d.sentinelChecking.CompareAndSwap(false, true) {
		return
	}
	d.sentinelCheckedAt = now
	if !d.goStart() {
		d.sentinelChecking.Store(false)
		return
	}
	go func() {
		defer d.goEnd()
		defer d.sentinelChecking.Store(false)
		d.checkPrimary(ctx, c)
	}()
}

// checkPrimary asks the sentinels for the primary and, when it is not the
// one c sends to, closes c's sentinel connections, as a READONLY reply's
// reconnect does, so c asks the sentinels again. An answer naming the
// primary c holds changes nothing, however long that primary takes to
// answer.
func (d *Driver) checkPrimary(ctx context.Context, c rueidis.Client) {
	named, err := d.askSentinels(ctx)
	if err != nil {
		if !d.closed.Load() && d.sentinelLog.allow(d.clk.Now()) {
			d.log.WarnContext(ctx, "state store sentinel check failed", slog.String(catalog.KeyShard, d.single.id), slog.String(catalog.KeyErrorType, errType(err)))
		}
		return
	}
	if _, ok := c.Nodes()[named]; ok {
		return
	}
	d.conns.closeWhere(func(tc *trackedConn) bool { return tc.sentinel })
	d.log.WarnContext(ctx, "state store sentinels name another primary; the client asks them again", slog.String(catalog.KeyShard, d.single.id))
}

// askSentinels asks the sentinel seeds of the URL, in order, for the
// master set's primary and returns the first answer as host:port; each
// ask, dial included, lasts at most the admin timeout.
func (d *Driver) askSentinels(ctx context.Context) (string, error) {
	err := errNoPrimary
	for _, addr := range d.ep.opt.InitAddress {
		var named string
		if named, err = d.askSentinel(ctx, addr); err == nil {
			return named, nil
		}
		if ctx.Err() != nil {
			break
		}
	}
	return "", err
}

// askSentinel dials the sentinel at addr through the driver's dialer, as a
// sentinel connection paced with the client's own, authenticates in HELLO
// with the URL's credentials as the client does, sends SENTINEL
// GET-MASTER-ADDR-BY-NAME for the master set and closes the connection,
// all within the admin timeout.
func (d *Driver) askSentinel(ctx context.Context, addr string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, d.opts.AdminTimeout)
	defer cancel()
	dialer := d.ep.opt.Dialer
	nc, err := d.dial(ctx, addr, &dialer, d.sentinelMark)
	if err != nil {
		return "", err
	}
	defer func() { _ = nc.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		if err := nc.SetDeadline(dl); err != nil {
			return "", err
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	hello := []string{"HELLO", "3"}
	if u, pw := d.ep.opt.Username, d.ep.opt.Password; u != "" || pw != "" {
		if u == "" {
			u = "default"
		}
		hello = append(hello, "AUTH", u, pw)
	}
	if _, err := io.WriteString(nc, command(hello)+command([]string{"SENTINEL", "GET-MASTER-ADDR-BY-NAME", d.ep.opt.Sentinel.MasterSet})); err != nil {
		return "", err
	}
	r := bufio.NewReaderSize(nc, 512)
	if err := skipReply(r, maxReplyDepth); err != nil {
		return "", err
	}
	return readPrimary(r)
}

// command encodes args as a RESP array of bulk strings.
func command(args []string) string {
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b.WriteString("$" + strconv.Itoa(len(a)) + "\r\n" + a + "\r\n")
	}
	return b.String()
}

// skipReply reads one RESP2 or RESP3 reply, nested at most depth levels,
// and discards it; an error reply is returned as a replyError.
func skipReply(r *bufio.Reader, depth int) error {
	line, err := readLine(r)
	if err != nil {
		return err
	}
	if line == "" || depth < 0 {
		return errProtocol
	}
	switch line[0] {
	case '-':
		return &replyError{msg: line[1:]}
	case '!':
		// A blob error: its length, then the message.
		s, err := readBlob(r, line)
		if err != nil {
			return err
		}
		return &replyError{msg: s}
	case '+', ':', '_', ',', '#', '(':
		return nil
	case '$', '=':
		_, err := readBlob(r, line)
		return err
	case '*', '~', '>', '%', '|':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < -1 || n > maxPreloadReply {
			return errProtocol
		}
		if line[0] == '%' || line[0] == '|' {
			n *= 2
		}
		for range max(n, 0) {
			if err := skipReply(r, depth-1); err != nil {
				return err
			}
		}
		return nil
	default:
		return errProtocol
	}
}

// readBlob reads the body of a blob reply whose header line is line;
// a null blob ($-1) is empty.
func readBlob(r *bufio.Reader, line string) (string, error) {
	n, err := strconv.Atoi(line[1:])
	if err != nil || n < -1 || n > maxPreloadReply {
		return "", errProtocol
	}
	if n == -1 {
		return "", nil
	}
	buf := make([]byte, n+2)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	if buf[n] != '\r' || buf[n+1] != '\n' {
		return "", errProtocol
	}
	return string(buf[:n]), nil
}

// readPrimary reads SENTINEL GET-MASTER-ADDR-BY-NAME's reply: the primary's
// host and port as host:port, errNoPrimary for a null reply or a master set
// the sentinel does not know, a replyError for an error reply.
func readPrimary(r *bufio.Reader) (string, error) {
	line, err := readLine(r)
	if err != nil {
		return "", err
	}
	switch {
	case line == "":
		return "", errProtocol
	case line[0] == '-':
		return "", &replyError{msg: line[1:]}
	case line == "_" || line == "*-1" || line == "$-1":
		return "", errNoPrimary
	case line != "*2":
		return "", errProtocol
	}
	var hostPort [2]string
	for i := range hostPort {
		l, err := readLine(r)
		if err != nil {
			return "", err
		}
		if l == "" || l[0] != '$' {
			return "", errProtocol
		}
		if hostPort[i], err = readBlob(r, l); err != nil {
			return "", err
		}
	}
	if hostPort[0] == "" || hostPort[1] == "" {
		return "", errNoPrimary
	}
	return net.JoinHostPort(hostPort[0], hostPort[1]), nil
}
