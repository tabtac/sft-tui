package session

import (
	"bytes"
	"io"
	"net"
	"time"

	"sft-tui/internal/config"
	"sft-tui/internal/protocol"
)

// Conn 封装 TLS/TCP 连接上的 SFT/1 帧收发，由 Mux 单一读取协程与单一写入协程操作。
type Conn struct {
	Net      net.Conn
	Chunk    uint32
	Timeouts config.Timeouts
}

// Read 使用空闲等待超时读取首字节，再切换至帧读取超时完成完整帧解析。
func (c *Conn) Read(wait uint16) (protocol.Frame, error) {
	if wait > 0 {
		if err := c.Net.SetReadDeadline(time.Now().Add(time.Duration(wait) * time.Second)); err != nil {
			return protocol.Frame{}, err
		}
	}
	var first [1]byte
	if _, err := io.ReadFull(c.Net, first[:]); err != nil {
		return protocol.Frame{}, err
	}
	if c.Timeouts.FrameReadSeconds > 0 {
		if err := c.Net.SetReadDeadline(time.Now().Add(time.Duration(c.Timeouts.FrameReadSeconds) * time.Second)); err != nil {
			return protocol.Frame{}, err
		}
	}
	return protocol.ReadFrame(io.MultiReader(bytes.NewReader(first[:]), c.Net), c.Chunk)
}

func (c *Conn) Send(typ protocol.Type, id protocol.StreamID, v any) error {
	p, err := protocol.Marshal(v)
	if err != nil {
		return err
	}
	return c.SendPayload(typ, id, p)
}

func (c *Conn) SendPayload(typ protocol.Type, id protocol.StreamID, p []byte) error {
	if c.Timeouts.WriteSeconds > 0 {
		if err := c.Net.SetWriteDeadline(time.Now().Add(time.Duration(c.Timeouts.WriteSeconds) * time.Second)); err != nil {
			return err
		}
	}
	return protocol.WriteFrame(c.Net, typ, id, p, c.Chunk)
}
