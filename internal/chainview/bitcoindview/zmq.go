package bitcoindview

import (
	"bytes"
	"context"
	"log/slog"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/gozmq"
)

// ZMQ topics a Bitcoin node publishes.
const (
	topicRawBlock = "rawblock"
	topicRawTx    = "rawtx"
)

// runMempoolZMQ streams unconfirmed transactions.
//
// Here the payload is the point rather than a doorbell: the transaction itself is
// what has to be examined, and there is no way to ask the node for "whatever just
// arrived".
func (v *View) runMempoolZMQ(ctx context.Context, out chan<- *wire.MsgTx) {
	defer close(out)

	v.runZMQLoop(ctx, v.c.opts.ZMQRawTx, topicRawTx, func(msg [][]byte) {
		// A notification is topic, payload, and a sequence number.
		if len(msg) < 2 {
			return
		}
		var tx wire.MsgTx
		if err := tx.Deserialize(bytes.NewReader(msg[1])); err != nil {
			v.log().Warn("could not read a published transaction",
				slog.String("error", firstLine(err.Error())))
			return
		}
		v.emitTx(out, &tx)
	})
}

// runZMQLoop keeps a subscription alive, calling onMessage for each notification.
//
// It returns only when the context ends. Every other failure is retried with
// backoff, because the channel closing is how a consumer learns to stop — and a
// node restart, which is routine, must not be mistaken for a shutdown.
func (v *View) runZMQLoop(ctx context.Context, endpoint, topic string, onMessage func([][]byte)) {
	backoff := reconnectMin

	for ctx.Err() == nil {
		conn, err := gozmq.Subscribe(endpoint, []string{topic}, zmqReceiveTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			v.log().Warn("could not subscribe to node notifications; will retry",
				slog.String("endpoint", endpoint),
				slog.String("topic", topic),
				slog.Duration("retry_in", backoff),
				slog.String("error", firstLine(err.Error())))
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		v.log().Info("subscribed to node notifications",
			slog.String("endpoint", endpoint), slog.String("topic", topic))

		// Connected: a later failure is a fresh problem, not a continuation.
		backoff = reconnectMin
		v.readZMQ(ctx, conn, endpoint, topic, onMessage)

		if err := conn.Close(); err != nil {
			v.log().Debug("closing notification socket", slog.String("error", firstLine(err.Error())))
		}
		if ctx.Err() != nil {
			return
		}
		// Dropped connection. Pause before reconnecting so a node that is down does
		// not get hammered.
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// readZMQ consumes notifications until the connection fails or the context ends.
func (v *View) readZMQ(ctx context.Context, conn *gozmq.Conn, endpoint, topic string, onMessage func([][]byte)) {
	// Reused across reads to avoid reallocating for every block.
	bufs := make([][]byte, 0, 3)

	for {
		if ctx.Err() != nil {
			return
		}
		msg, err := conn.Receive(bufs)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// A read timeout is expected and is how cancellation gets noticed
			// promptly; anything else means the connection is gone.
			if isTimeout(err) {
				continue
			}
			v.log().Warn("node notification stream failed; will reconnect",
				slog.String("endpoint", endpoint),
				slog.String("topic", topic),
				slog.String("error", firstLine(err.Error())))
			return
		}
		onMessage(msg)
	}
}

// isTimeout reports whether err is a read deadline rather than a real failure.
func isTimeout(err error) bool {
	type timeouter interface{ Timeout() bool }
	var t timeouter
	if ok := asType(err, &t); ok {
		return t.Timeout()
	}
	return false
}

// nextBackoff doubles the delay up to the ceiling.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > reconnectMax {
		return reconnectMax
	}
	return d
}

// sleepCtx waits for d, reporting false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
