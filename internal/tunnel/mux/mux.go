// Adapted from github.com/bwsalmon/grain pkg/tunnel/mux/mux.go at d6cd9d71f9b46df18d47e5305f84b5471ce93047, Apache License 2.0.

// Package mux implements a lightweight full-duplex stream multiplexer over a single connection.
//
// Multiple logical streams are multiplexed over an underlying io.ReadWriteCloser (or net.Conn),
// each presenting a standard net.Conn interface with support for half-closing (CloseWrite),
// stream resets, keepalive heartbeats, and concurrent bidirectional data transfer.
package mux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Frame type identifiers.
const (
	FrameData       byte = 0x01 // Carries payload bytes for a specific stream.
	FrameCloseWrite byte = 0x02 // Signals half-close (EOF) from the sender's write half.
	FrameReset      byte = 0x03 // Abruptly terminates a stream with an error.
	FramePing       byte = 0x04 // Session-level heartbeat ping.
	FramePong       byte = 0x05 // Session-level heartbeat pong (echo).

	// FrameWindowUpdate grants the peer more send credit. On a stream it carries a
	// 4-byte big-endian increment; on stream 0 it is the flow-control capability
	// announcement each side sends as its very first frame. Older peers ignore it.
	FrameWindowUpdate byte = 0x06
)

const (
	headerSize    = 9     // 4 bytes payload length + 1 byte type + 4 bytes streamID
	maxPayloadLen = 65536 // 64 KB max payload per frame to bound allocations
	defaultChunk  = 32768 // 32 KB chunk size when fragmenting writes

	// initialWindow is the per-stream receive window both sides of a
	// flow-controlled session start with: a sender never has more than this many
	// bytes outstanding that the receiver has not yet read.
	initialWindow = 1 << 20
	// legacyMaxBuffer bounds the per-stream receive buffer when the peer does not
	// speak flow control and so cannot be told to stop sending. A stream that
	// exceeds it is reset rather than buffered without limit.
	legacyMaxBuffer = 16 << 20

	// maxUnseenPeerIDs bounds how many peer stream ids below the highest one
	// seen are remembered as not yet seen; see Mux.claimPeerID.
	maxUnseenPeerIDs = 1 << 14
)

var (
	ErrSessionClosed = errors.New("tunnel mux: session closed")
	ErrStreamClosed  = errors.New("tunnel mux: stream closed")
	ErrStreamReset   = errors.New("tunnel mux: stream reset by peer")
	ErrFrameTooLarge = errors.New("tunnel mux: frame payload exceeds max size")
	ErrTimeout       = errors.New("tunnel mux: i/o timeout")
	ErrBufferFull    = errors.New("tunnel mux: stream receive buffer full")
)

// Config configures a Mux session.
type Config struct {
	// IsServer determines the stream ID parity.
	// Server/Gateway initiates odd stream IDs (1, 3, 5, ...).
	// Client/Connector initiates even stream IDs (2, 4, 6, ...).
	IsServer bool

	// PingInterval is how often keep-alive pings are sent. Defaults to 15s if <= 0.
	PingInterval time.Duration

	// PingTimeout is how long to wait for a pong before closing the session. Defaults to 10s if <= 0.
	PingTimeout time.Duration
}

// Mux manages multiplexed bidirectional streams over a single transport connection.
type Mux struct {
	rwc      io.ReadWriteCloser
	isServer bool

	writeMu sync.Mutex

	streamsMu sync.RWMutex
	streams   map[uint32]*Stream
	nextID    uint32

	// Peer-initiated stream ids that have been seen, so that a frame
	// arriving after its stream has closed is not taken for a new stream.
	// Rather than remember every closed id for the life of the session, this
	// keeps the next id beyond the highest seen (peerNextID) and the ids
	// below it the peer has skipped so far (peerUnseen). Ids can arrive out
	// of order -- OpenStream allocates one before its first frame is sent --
	// so a skipped id may still turn up; peerUnseen is bounded by
	// maxUnseenPeerIDs, the oldest dropped first, which peerUnseenOrder
	// (ascending, possibly holding ids already claimed) finds without a
	// scan. Guarded by streamsMu.
	peerNextID      uint32
	peerUnseen      map[uint32]struct{}
	peerUnseenOrder []uint32

	acceptCh chan *Stream

	closed     atomic.Bool
	closeErr   error
	closeErrMu sync.RWMutex
	doneCh     chan struct{}

	pingInterval time.Duration
	pingTimeout  time.Duration
	lastPongTime atomic.Int64 // Unix nano

	// negotiated is closed once the peer's first frame has been read, at which
	// point peerFlowControl says whether the peer announced flow control.
	negotiated      chan struct{}
	peerFlowControl atomic.Bool
}

// NewMux starts a new multiplexer over rwc.
func NewMux(rwc io.ReadWriteCloser, cfg Config) *Mux {
	pingInterval := cfg.PingInterval
	if pingInterval <= 0 {
		pingInterval = 15 * time.Second
	}
	pingTimeout := cfg.PingTimeout
	if pingTimeout <= 0 {
		pingTimeout = 10 * time.Second
	}

	startID, peerStartID := uint32(2), uint32(1)
	if cfg.IsServer {
		startID, peerStartID = 1, 2
	}

	m := &Mux{
		rwc:          rwc,
		isServer:     cfg.IsServer,
		streams:      make(map[uint32]*Stream),
		nextID:       startID,
		peerNextID:   peerStartID,
		peerUnseen:   make(map[uint32]struct{}),
		acceptCh:     make(chan *Stream, 256),
		doneCh:       make(chan struct{}),
		pingInterval: pingInterval,
		pingTimeout:  pingTimeout,
		negotiated:   make(chan struct{}),
	}
	m.lastPongTime.Store(time.Now().UnixNano())

	// Announce flow control ahead of every other frame, then ping so that a
	// legacy peer (which ignores the announcement) answers with a pong and
	// negotiation settles within one round trip. writeMu is taken here, before
	// NewMux returns, so no stream frame can overtake the announcement; the
	// write itself happens in the background because transports like net.Pipe
	// block until the peer reads.
	m.writeMu.Lock()
	go func() {
		defer m.writeMu.Unlock()
		var ping [8]byte
		binary.BigEndian.PutUint64(ping[:], uint64(time.Now().UnixNano()))
		buf := append(encodeFrame(FrameWindowUpdate, 0, nil), encodeFrame(FramePing, 0, ping[:])...)
		if _, err := m.rwc.Write(buf); err != nil {
			go m.CloseWithError(fmt.Errorf("write error: %w", err))
		}
	}()

	go m.readLoop()
	go m.pingLoop()

	return m
}

// OpenStream opens a new stream to the peer.
func (m *Mux) OpenStream(ctx context.Context) (*Stream, error) {
	if m.closed.Load() {
		return nil, m.getCloseErr()
	}

	m.streamsMu.Lock()
	if m.closed.Load() {
		m.streamsMu.Unlock()
		return nil, m.getCloseErr()
	}

	id := m.nextID
	m.nextID += 2 // Maintain parity (odd for server, even for client)

	stream := newStream(m, id, true)
	m.streams[id] = stream
	m.streamsMu.Unlock()

	return stream, nil
}

// AcceptStream waits for and returns the next stream opened by the peer.
func (m *Mux) AcceptStream() (*Stream, error) {
	select {
	case s, ok := <-m.acceptCh:
		if !ok {
			return nil, m.getCloseErr()
		}
		return s, nil
	case <-m.doneCh:
		return nil, m.getCloseErr()
	}
}

// Close gracefully closes the multiplexer session and all active streams.
func (m *Mux) Close() error {
	return m.CloseWithError(ErrSessionClosed)
}

// CloseWithError terminates the session with a specific error.
func (m *Mux) CloseWithError(err error) error {
	if err == nil {
		err = ErrSessionClosed
	}

	if !m.closed.CompareAndSwap(false, true) {
		return nil
	}

	m.closeErrMu.Lock()
	m.closeErr = err
	m.closeErrMu.Unlock()

	close(m.doneCh)
	closeErr := m.rwc.Close()

	// Terminate all active streams
	m.streamsMu.Lock()
	active := make([]*Stream, 0, len(m.streams))
	for _, s := range m.streams {
		active = append(active, s)
	}
	m.streams = make(map[uint32]*Stream)
	m.streamsMu.Unlock()

	for _, s := range active {
		s.closeWithError(err)
	}

	return closeErr
}

// Done returns a channel that is closed when the multiplexer session is closed.
func (m *Mux) Done() <-chan struct{} {
	return m.doneCh
}

func (m *Mux) getCloseErr() error {
	m.closeErrMu.RLock()
	defer m.closeErrMu.RUnlock()
	if m.closeErr != nil {
		return m.closeErr
	}
	return ErrSessionClosed
}

// writeFrame writes a framed packet to the underlying connection.
func (m *Mux) writeFrame(frameType byte, streamID uint32, payload []byte) error {
	if m.closed.Load() {
		return m.getCloseErr()
	}

	if len(payload) > maxPayloadLen {
		return ErrFrameTooLarge
	}

	buf := encodeFrame(frameType, streamID, payload)

	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	if m.closed.Load() {
		return m.getCloseErr()
	}

	_, err := m.rwc.Write(buf)
	if err != nil {
		go m.CloseWithError(fmt.Errorf("write error: %w", err))
		return err
	}
	return nil
}

func encodeFrame(frameType byte, streamID uint32, payload []byte) []byte {
	buf := make([]byte, headerSize+len(payload))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(payload)))
	buf[4] = frameType
	binary.BigEndian.PutUint32(buf[5:9], streamID)
	copy(buf[9:], payload)
	return buf
}

// recvLimit is the most a stream may buffer before it is reset.
func (m *Mux) recvLimit() int {
	if m.peerFlowControl.Load() {
		return initialWindow
	}
	return legacyMaxBuffer
}

// negotiatedChan returns the channel to wait on for negotiation, or nil once
// it has completed so that callers selecting on it do not spin.
func (m *Mux) negotiatedChan() <-chan struct{} {
	select {
	case <-m.negotiated:
		return nil
	default:
		return m.negotiated
	}
}

func (m *Mux) sendWindowUpdate(streamID uint32, increment uint32) {
	var payload [4]byte
	binary.BigEndian.PutUint32(payload[:], increment)
	_ = m.writeFrame(FrameWindowUpdate, streamID, payload[:])
}

func (m *Mux) removeStream(id uint32) {
	m.streamsMu.Lock()
	delete(m.streams, id)
	m.streamsMu.Unlock()
}

// claimPeerID records that a frame for the peer-initiated stream id has
// arrived and reports whether it is the first: false means the stream has
// been seen before and so is closed if it is not in m.streams. Callers hold
// streamsMu.
func (m *Mux) claimPeerID(id uint32) bool {
	if id < m.peerNextID {
		if _, ok := m.peerUnseen[id]; !ok {
			return false
		}
		delete(m.peerUnseen, id)
		if len(m.peerUnseenOrder) > 2*len(m.peerUnseen)+64 {
			live := make([]uint32, 0, len(m.peerUnseen))
			for _, u := range m.peerUnseenOrder {
				if _, ok := m.peerUnseen[u]; ok {
					live = append(live, u)
				}
			}
			m.peerUnseenOrder = live
		}
		return true
	}

	// The ids between the last seen and this one have been allocated by
	// the peer but not used yet. A gap wider than the bound is only
	// remembered at its top end.
	from := m.peerNextID
	if span := uint32(2 * maxUnseenPeerIDs); id-from > span {
		from = id - span
	}
	for skipped := from; skipped < id; skipped += 2 {
		m.peerUnseen[skipped] = struct{}{}
		m.peerUnseenOrder = append(m.peerUnseenOrder, skipped)
	}
	m.peerNextID = id + 2

	for len(m.peerUnseen) > maxUnseenPeerIDs {
		delete(m.peerUnseen, m.peerUnseenOrder[0])
		m.peerUnseenOrder = m.peerUnseenOrder[1:]
	}
	return true
}

func (m *Mux) readLoop() {
	var hdr [headerSize]byte
	first := true

	for {
		if _, err := io.ReadFull(m.rwc, hdr[:]); err != nil {
			m.CloseWithError(fmt.Errorf("reading frame header: %w", err))
			return
		}

		length := binary.BigEndian.Uint32(hdr[0:4])
		frameType := hdr[4]
		streamID := binary.BigEndian.Uint32(hdr[5:9])

		if length > maxPayloadLen {
			m.CloseWithError(ErrFrameTooLarge)
			return
		}

		var payload []byte
		if length > 0 {
			payload = make([]byte, length)
			if _, err := io.ReadFull(m.rwc, payload); err != nil {
				m.CloseWithError(fmt.Errorf("reading frame payload: %w", err))
				return
			}
		}

		if first {
			// A flow-controlled peer always sends its announcement first, so
			// the first frame decides the mode before any of the peer's data.
			first = false
			m.peerFlowControl.Store(frameType == FrameWindowUpdate && streamID == 0)
			close(m.negotiated)
		}

		m.handleFrame(frameType, streamID, payload)
	}
}

func (m *Mux) handleFrame(frameType byte, streamID uint32, payload []byte) {
	// Control frames for session keepalive
	if streamID == 0 {
		switch frameType {
		case FramePing:
			// Reply off the read loop so a peer that is itself blocked writing
			// to us cannot deadlock the session.
			go func() { _ = m.writeFrame(FramePong, 0, payload) }()
		case FramePong:
			m.lastPongTime.Store(time.Now().UnixNano())
		}
		return
	}

	m.streamsMu.Lock()
	stream, exists := m.streams[streamID]
	if !exists {
		isPeerInitiated := (m.isServer && streamID%2 == 0) || (!m.isServer && streamID%2 == 1)
		// Any frame marks a peer id as used, so a stream the peer reset
		// before sending anything is not left among the unseen ones.
		isNew := isPeerInitiated && m.claimPeerID(streamID)
		if isNew && (frameType == FrameData || frameType == FrameCloseWrite) {
			if m.closed.Load() {
				m.streamsMu.Unlock()
				return
			}
			stream = newStream(m, streamID, false)
			m.streams[streamID] = stream
			m.streamsMu.Unlock()

			select {
			case m.acceptCh <- stream:
			default:
				// Accept queue full; reset stream
				stream.closeWithError(errors.New("accept queue full"))
				m.removeStream(streamID)
				go func() { _ = m.writeFrame(FrameReset, streamID, nil) }()
				return
			}
		} else {
			m.streamsMu.Unlock()
			// Frame for unknown, already closed, or invalid stream; ignore
			return
		}
	} else {
		m.streamsMu.Unlock()
	}

	switch frameType {
	case FrameData:
		stream.receiveData(payload)
	case FrameCloseWrite:
		stream.receiveCloseWrite()
	case FrameReset:
		stream.receiveReset()
	case FrameWindowUpdate:
		if len(payload) == 4 {
			stream.receiveWindowUpdate(binary.BigEndian.Uint32(payload))
		}
	}
}

func (m *Mux) pingLoop() {
	ticker := time.NewTicker(m.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.doneCh:
			return
		case <-ticker.C:
			lastPong := time.Unix(0, m.lastPongTime.Load())
			if time.Since(lastPong) > m.pingInterval+m.pingTimeout {
				m.CloseWithError(errors.New("ping timeout: peer unresponsive"))
				return
			}

			var payload [8]byte
			binary.BigEndian.PutUint64(payload[:], uint64(time.Now().UnixNano()))
			if err := m.writeFrame(FramePing, 0, payload[:]); err != nil {
				return
			}
		}
	}
}
