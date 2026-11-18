package main

import (
	"hash/fnv"
	"sync"
	"time"

	"github.com/getkiko/burrow/internal/signalproto"
)

const (
	listenerTTL        = 45 * time.Second
	sessionTTL         = 60 * time.Second
	shardCount         = 256
	maxSessionsPerRoom = 4

	// defaultMaxRoomsPerConn bounds how many rooms one signaling connection
	// may take part in, as listener or dialer. The SDK uses one connection
	// per room, so legitimate clients stay at one or two.
	defaultMaxRoomsPerConn = 32

	// codeTooManyRooms is the error code for a connection over its room limit.
	codeTooManyRooms = "too_many_rooms"
)

type signalPeer struct {
	id uint64
	// remoteIP is the full source address, used for token source checks.
	// limitKey is the per-source rate-limit key derived from it (see
	// limitKey).
	remoteIP string
	limitKey string
	send     func([]byte) error

	// rooms holds the keys of the rooms this peer registered in as listener
	// or dialer, so Disconnect only visits those rooms. Entries can go stale
	// when Sweep expires a listener or session; trackRoom prunes them when
	// the set is full. mu is always taken after a shard lock, never before.
	mu    sync.Mutex
	rooms map[roomKey]struct{}
}

// trackRoom records key for the peer. It reports false when the key is new
// and the peer already tracks limit rooms. A limit of zero or less disables
// the check. The caller holds the lock of key's shard.
func (p *signalPeer) trackRoom(key roomKey, limit int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.rooms[key]; exists {
		return true
	}
	if limit > 0 && len(p.rooms) >= limit {
		return false
	}
	if p.rooms == nil {
		p.rooms = make(map[roomKey]struct{})
	}
	p.rooms[key] = struct{}{}
	return true
}

func (p *signalPeer) untrackRoom(key roomKey) {
	p.mu.Lock()
	delete(p.rooms, key)
	p.mu.Unlock()
}

// roomKeys returns a snapshot of the tracked room keys.
func (p *signalPeer) roomKeys() []roomKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]roomKey, 0, len(p.rooms))
	for key := range p.rooms {
		keys = append(keys, key)
	}
	return keys
}

func (p *signalPeer) roomCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.rooms)
}

type registry struct {
	shards [shardCount]*shard
	// maxRoomsPerPeer is the per-connection room limit; zero or less
	// disables it.
	maxRoomsPerPeer int
}

// roomKey is the registry key: (transport, room). transport must be the value
// normalized by Decode (tcp or udp), so that an omitted transport and an
// explicit "tcp" do not split one logical room in two.
type roomKey struct {
	transport string
	room      string
}

type shard struct {
	mu    sync.Mutex
	rooms map[roomKey]*roomState
}

type roomState struct {
	listener     *signalPeer
	listenerSeen time.Time
	capacity     int
	sessions     map[string]*sessionState
}

type sessionState struct {
	dialer    *signalPeer
	dial      signalproto.Message
	ready     *signalproto.Message
	rejected  *signalproto.Message
	createdAt time.Time
}

func newRegistry() *registry {
	r := &registry{maxRoomsPerPeer: defaultMaxRoomsPerConn}
	for index := range r.shards {
		r.shards[index] = &shard{rooms: make(map[roomKey]*roomState)}
	}
	return r
}

func (r *registry) shardFor(key roomKey) *shard {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(key.transport))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(key.room))
	return r.shards[hash.Sum32()&(shardCount-1)]
}

func (s *shard) getOrCreate(key roomKey) *roomState {
	state, exists := s.rooms[key]
	if !exists {
		state = &roomState{sessions: make(map[string]*sessionState)}
		s.rooms[key] = state
	}
	return state
}

// roomCapacity returns the effective room capacity: the capacity announced by
// LISTEN if positive, otherwise maxSessionsPerRoom.
func roomCapacity(capacity int) int {
	if capacity <= 0 {
		return maxSessionsPerRoom
	}
	return capacity
}

// Listen registers peer as the listener of the room. It returns
// codeTooManyRooms when the peer is already in maxRoomsPerPeer other rooms.
func (r *registry) Listen(transport, room string, capacity int, peer *signalPeer, now time.Time) string {
	key := roomKey{transport: transport, room: room}
	if !r.reserveRoom(key, peer) {
		return codeTooManyRooms
	}
	shard := r.shardFor(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	if !peer.trackRoom(key, r.maxRoomsPerPeer) {
		return codeTooManyRooms
	}
	state := shard.getOrCreate(key)
	state.listener = peer
	state.listenerSeen = now
	state.capacity = roomCapacity(capacity)
	return ""
}

// reserveRoom reports whether peer may take part in the room key without
// exceeding the per-connection room limit. When the peer's set is full it
// first drops rooms the peer no longer takes part in. It must be called
// without any shard lock held, because pruning visits other shards; the
// caller rechecks the limit under the shard lock with trackRoom. A peer's
// messages are handled one at a time, so the room set cannot grow in
// between.
func (r *registry) reserveRoom(key roomKey, peer *signalPeer) bool {
	if r.maxRoomsPerPeer <= 0 || peer.roomCount() < r.maxRoomsPerPeer {
		return true
	}
	for _, known := range peer.roomKeys() {
		if known == key {
			return true
		}
	}
	r.pruneRooms(peer)
	return peer.roomCount() < r.maxRoomsPerPeer
}

// pruneRooms removes the tracked rooms in which peer is neither the
// listener nor the dialer of any session.
func (r *registry) pruneRooms(peer *signalPeer) {
	for _, key := range peer.roomKeys() {
		shard := r.shardFor(key)
		shard.mu.Lock()
		if room, exists := shard.rooms[key]; !exists || !room.references(peer) {
			peer.untrackRoom(key)
		}
		shard.mu.Unlock()
	}
}

// references reports whether peer is the room's listener or the dialer of
// one of its sessions. The caller holds the shard lock.
func (room *roomState) references(peer *signalPeer) bool {
	if room.listener == peer {
		return true
	}
	for _, session := range room.sessions {
		if session.dialer == peer {
			return true
		}
	}
	return false
}

type dialResult struct {
	Code           string
	NotifyListener *signalPeer
	Incoming       signalproto.Message
	Peer           *signalproto.Message
	Error          *signalproto.Message
}

func (r *registry) Dial(message signalproto.Message, peer *signalPeer, now time.Time) dialResult {
	key := roomKey{transport: message.Transport, room: message.Room}
	if !r.reserveRoom(key, peer) {
		return dialResult{Code: codeTooManyRooms}
	}
	shard := r.shardFor(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()

	room, exists := shard.rooms[key]
	if !exists || room.listener == nil || now.Sub(room.listenerSeen) > listenerTTL {
		return dialResult{Code: "no_listener"}
	}
	session, exists := room.sessions[message.Session]
	if !exists {
		activeSessions := 0
		for _, known := range room.sessions {
			if known.ready == nil && known.rejected == nil {
				activeSessions++
			}
		}
		if activeSessions >= room.capacity {
			return dialResult{Code: "room_busy"}
		}
		if !peer.trackRoom(key, r.maxRoomsPerPeer) {
			return dialResult{Code: codeTooManyRooms}
		}
		session = &sessionState{createdAt: now}
		room.sessions[message.Session] = session
	} else if session.dial.Fingerprint != "" && session.dial.Fingerprint != message.Fingerprint {
		return dialResult{Code: "strategy_mismatch"}
	} else if !peer.trackRoom(key, r.maxRoomsPerPeer) {
		return dialResult{Code: codeTooManyRooms}
	}
	session.dialer = peer
	session.dial = message
	if session.rejected != nil {
		response := *session.rejected
		return dialResult{Error: &response}
	}
	if session.ready != nil {
		response := *session.ready
		response.Type = signalproto.TypePeer
		return dialResult{Peer: &response}
	}
	incoming := message
	incoming.Type = signalproto.TypeIncoming
	incoming.Token = ""
	return dialResult{NotifyListener: room.listener, Incoming: incoming}
}

type readyResult struct {
	Code   string
	Dialer *signalPeer
	Peer   signalproto.Message
}

type rejectResult struct {
	Code   string
	Dialer *signalPeer
}

func (r *registry) Ready(message signalproto.Message, listener *signalPeer) readyResult {
	key := roomKey{transport: message.Transport, room: message.Room}
	shard := r.shardFor(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	room, exists := shard.rooms[key]
	if !exists || room.listener != listener {
		return readyResult{Code: "listener_mismatch"}
	}
	session, exists := room.sessions[message.Session]
	if !exists || session.dialer == nil {
		return readyResult{Code: "session_missing"}
	}
	if session.dial.Fingerprint != message.Fingerprint {
		return readyResult{Code: "strategy_mismatch"}
	}
	stored := message
	session.ready = &stored
	peer := message
	peer.Type = signalproto.TypePeer
	return readyResult{Dialer: session.dialer, Peer: peer}
}

func (r *registry) Reject(message signalproto.Message, listener *signalPeer) rejectResult {
	key := roomKey{transport: message.Transport, room: message.Room}
	shard := r.shardFor(key)
	shard.mu.Lock()
	defer shard.mu.Unlock()
	room, exists := shard.rooms[key]
	if !exists || room.listener != listener {
		return rejectResult{Code: "listener_mismatch"}
	}
	session, exists := room.sessions[message.Session]
	if !exists || session.dialer == nil {
		return rejectResult{Code: "session_missing"}
	}
	stored := message
	session.rejected = &stored
	return rejectResult{Dialer: session.dialer}
}

// Disconnect removes peer from the rooms it registered in. It only visits
// those rooms, not the whole registry; anything it misses (for example a
// session whose dialer left earlier) expires in Sweep.
func (r *registry) Disconnect(peer *signalPeer) {
	for _, key := range peer.roomKeys() {
		shard := r.shardFor(key)
		shard.mu.Lock()
		peer.untrackRoom(key)
		if room, exists := shard.rooms[key]; exists {
			if room.listener == peer {
				room.listener = nil
			}
			for sessionID, session := range room.sessions {
				if session.dialer == peer {
					session.dialer = nil
				}
				if session.dialer == nil && room.listener == nil {
					delete(room.sessions, sessionID)
				}
			}
			if room.listener == nil && len(room.sessions) == 0 {
				delete(shard.rooms, key)
			}
		}
		shard.mu.Unlock()
	}
}

func (r *registry) Sweep(now time.Time) {
	for _, shard := range r.shards {
		shard.mu.Lock()
		for key, room := range shard.rooms {
			for sessionID, session := range room.sessions {
				if now.Sub(session.createdAt) > sessionTTL {
					delete(room.sessions, sessionID)
				}
			}
			if room.listener == nil || now.Sub(room.listenerSeen) > listenerTTL {
				room.listener = nil
			}
			if room.listener == nil && len(room.sessions) == 0 {
				delete(shard.rooms, key)
			}
		}
		shard.mu.Unlock()
	}
}

func (r *registry) Stats() (rooms, sessions int) {
	for _, shard := range r.shards {
		shard.mu.Lock()
		rooms += len(shard.rooms)
		for _, room := range shard.rooms {
			sessions += len(room.sessions)
		}
		shard.mu.Unlock()
	}
	return rooms, sessions
}
