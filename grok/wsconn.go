// Copyright 2026 Marcelo Cantos
// SPDX-License-Identifier: Apache-2.0

package grok

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
)

// The WebSocket library lives in this file and nowhere else in the
// package's non-test code. [Conn] is what the rest of the package sees,
// so replacing the library changes this file only and no public
// signature (🎯T47.9; entropy-audit ENT-009).

// readLimit bounds one server message. Realtime audio deltas are
// base64 PCM, so a frame can run to megabytes.
const readLimit = 4 << 20 // 4 MB

// dialWebSocket is the default [DialArgs.Dial]: a WebSocket upgrade
// carrying header, wrapped as a [Conn].
func dialWebSocket(ctx context.Context, url string, header http.Header) (Conn, error) {
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(readLimit)
	return &wsConn{c: c}, nil
}

// wsConn adapts a WebSocket connection to [Conn]: text messages in
// both directions, a normal-closure handshake on Close.
type wsConn struct{ c *websocket.Conn }

func (w *wsConn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := w.c.Read(ctx)
	return data, err
}

func (w *wsConn) Write(ctx context.Context, msg []byte) error {
	return w.c.Write(ctx, websocket.MessageText, msg)
}

func (w *wsConn) Close() error {
	return w.c.Close(websocket.StatusNormalClosure, "bye")
}

// closeStatus names the close code behind a read error, for the log
// line; a read error that is not a close frame reports -1.
func closeStatus(err error) websocket.StatusCode {
	return websocket.CloseStatus(err)
}
