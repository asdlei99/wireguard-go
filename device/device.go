/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2020 WireGuard LLC. All Rights Reserved.
 */

package device

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/wireguard-go/conn"
	"github.com/tailscale/wireguard-go/ratelimiter"
	"github.com/tailscale/wireguard-go/rwcancel"
	"github.com/tailscale/wireguard-go/tun"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"inet.af/netaddr"
)

type Device struct {
	isUp           AtomicBool // device is (going) up
	isClosed       AtomicBool // device is closed? (acting as guard)
	log            *Logger
	handshakeDone  func(peerKey NoisePublicKey, peer *Peer, allowedIPs *AllowedIPs)
	skipBindUpdate bool
	createBind     func(uport uint16, device *Device) (conn.Bind, uint16, error)
	createEndpoint func(key [32]byte, s string) (conn.Endpoint, error)

	// synchronized resources (locks acquired in order)

	state struct {
		stopping sync.WaitGroup
		sync.Mutex
		changing AtomicBool
		current  bool
	}

	net struct {
		stopping sync.WaitGroup
		sync.RWMutex
		bind          conn.Bind // bind interface
		netlinkCancel *rwcancel.RWCancel
		port          uint16 // listening port
		fwmark        uint32 // mark value (0 = disabled)
	}

	staticIdentity struct {
		sync.RWMutex
		privateKey NoisePrivateKey
		publicKey  NoisePublicKey
	}

	peers struct {
		empty        AtomicBool // empty reports whether len(keyMap) == 0
		sync.RWMutex            // protects keyMap
		keyMap       map[NoisePublicKey]*Peer
	}

	// unprotected / "self-synchronising resources"

	allowedips    AllowedIPs
	indexTable    IndexTable
	cookieChecker CookieChecker

	unexpectedip func(key *NoisePublicKey, ip netaddr.IP)

	rate struct {
		underLoadUntil atomic.Value
		limiter        ratelimiter.Ratelimiter
	}

	pool struct {
		messageBufferPool        *sync.Pool
		messageBufferReuseChan   chan *[MaxMessageSize]byte
		inboundElementPool       *sync.Pool
		inboundElementReuseChan  chan *QueueInboundElement
		outboundElementPool      *sync.Pool
		outboundElementReuseChan chan *QueueOutboundElement
	}

	queue struct {
		encryption *encryptionQueue
		decryption chan *QueueInboundElement
		handshake  chan QueueHandshakeElement
	}

	signals struct {
		stop chan struct{}
	}

	tun struct {
		device tun.Device
		mtu    int32
	}
}

// An encryptionQueue is a channel of QueueOutboundElements awaiting encryption.
// An encryptionQueue is ref-counted using its wg field.
// An encryptionQueue created with newEncryptionQueue has one reference.
// Every additional writer must call wg.Add(1).
// Every completed writer must call wg.Done().
// When no further writers will be added,
// call wg.Done to remove the initial reference.
// When the refcount hits 0, the queue's channel is closed.
type encryptionQueue struct {
	c  chan *QueueOutboundElement
	wg sync.WaitGroup
}

func newEncryptionQueue() *encryptionQueue {
	q := &encryptionQueue{
		c: make(chan *QueueOutboundElement, QueueOutboundSize),
	}
	q.wg.Add(1)
	go func() {
		q.wg.Wait()
		close(q.c)
	}()
	return q
}

/* Converts the peer into a "zombie", which remains in the peer map,
 * but processes no packets and does not exists in the routing table.
 *
 * Must hold device.peers.Mutex
 */
func unsafeRemovePeer(device *Device, peer *Peer, key NoisePublicKey) {
	// stop routing of packets
	device.allowedips.RemoveByPeer(peer)

	// remove from peer map
	delete(device.peers.keyMap, key)
	device.peers.empty.Set(len(device.peers.keyMap) == 0)
}

func deviceUpdateState(device *Device) error {

	// check if state already being updated (guard)

	if device.state.changing.Swap(true) {
		return nil
	}

	// compare to current state of device

	device.state.Lock()

	newIsUp := device.isUp.Get()

	if newIsUp == device.state.current {
		device.state.changing.Set(false)
		device.state.Unlock()
		return nil
	}

	// change state of device

	switch newIsUp {
	case true:
		if err := device.BindUpdate(); err != nil {
			device.isUp.Set(false)
			device.state.Unlock()
			return fmt.Errorf("unable to update bind: %w\n", err)
		}
		device.peers.RLock()
		for _, peer := range device.peers.keyMap {
			if err := peer.Start(); err != nil {
				device.state.Unlock()
				device.peers.RUnlock()
				return err
			}
			if atomic.LoadUint32(&peer.persistentKeepaliveInterval) > 0 {
				peer.SendKeepalive()
			}
		}
		device.peers.RUnlock()

	case false:
		device.BindClose()
		device.peers.RLock()
		for _, peer := range device.peers.keyMap {
			peer.Stop()
		}
		device.peers.RUnlock()
	}

	// update state variables

	device.state.current = newIsUp
	device.state.changing.Set(false)
	device.state.Unlock()

	// check for state change in the mean time

	return deviceUpdateState(device)
}

func (device *Device) Up() error {

	// closed device cannot be brought up

	if device.isClosed.Get() {
		return errors.New("device is closed")
	}

	device.isUp.Set(true)
	return deviceUpdateState(device)
}

func (device *Device) Down() error {
	device.isUp.Set(false)
	return deviceUpdateState(device)
}

func (device *Device) IsUnderLoad() bool {

	// check if currently under load

	now := time.Now()
	underLoad := len(device.queue.handshake) >= UnderLoadQueueSize
	if underLoad {
		device.rate.underLoadUntil.Store(now.Add(UnderLoadAfterTime))
		return true
	}

	// check if recently under load

	until := device.rate.underLoadUntil.Load().(time.Time)
	return until.After(now)
}

func (device *Device) SetPrivateKey(sk NoisePrivateKey) error {
	var peersToStop []*Peer
	defer func() {
		for _, peer := range peersToStop {
			peer.Stop()
		}
	}()

	// lock required resources

	device.staticIdentity.Lock()
	defer device.staticIdentity.Unlock()

	if sk.Equals(device.staticIdentity.privateKey) {
		return nil
	}

	device.peers.Lock()
	defer device.peers.Unlock()

	lockedPeers := make([]*Peer, 0, len(device.peers.keyMap))
	for _, peer := range device.peers.keyMap {
		peer.handshake.mutex.RLock()
		lockedPeers = append(lockedPeers, peer)
	}

	// remove peers with matching public keys
	var publicKey NoisePublicKey // allow a zero key here for disabling this device
	if !sk.IsZero() {
		publicKey = sk.publicKey()
	}

	for key, peer := range device.peers.keyMap {
		if peer.handshake.remoteStatic.Equals(publicKey) {
			unsafeRemovePeer(device, peer, key)
			peersToStop = append(peersToStop, peer)
		}
	}

	// update key material

	device.staticIdentity.privateKey = sk
	device.staticIdentity.publicKey = publicKey
	device.cookieChecker.Init(publicKey)

	// do static-static DH pre-computations

	expiredPeers := make([]*Peer, 0, len(device.peers.keyMap))
	for _, peer := range device.peers.keyMap {
		handshake := &peer.handshake
		handshake.precomputedStaticStatic = device.staticIdentity.privateKey.sharedSecret(handshake.remoteStatic)
		expiredPeers = append(expiredPeers, peer)
	}

	for _, peer := range lockedPeers {
		peer.handshake.mutex.RUnlock()
	}
	for _, peer := range expiredPeers {
		peer.ExpireCurrentKeypairs()
	}

	return nil
}

type DeviceOptions struct {
	Logger *Logger

	// UnexpectedIP is called when a packet is received from a
	// validated peer with an unexpected internal IP address.
	// The packet is then dropped.
	UnexpectedIP func(key *NoisePublicKey, ip netaddr.IP)

	// HandshakeDone is called every time we complete a peer handshake.
	HandshakeDone func(peerKey NoisePublicKey, peer *Peer, allowedIPs *AllowedIPs)

	CreateEndpoint func(key [32]byte, s string) (conn.Endpoint, error)
	CreateBind     func(uport uint16) (conn.Bind, uint16, error)
	SkipBindUpdate bool // if true, CreateBind only ever called once
}

func NewDevice(tunDevice tun.Device, opts *DeviceOptions) *Device {
	device := new(Device)

	device.isUp.Set(false)
	device.isClosed.Set(false)

	if opts != nil {
		if opts.Logger != nil {
			device.log = opts.Logger
		}
		if opts.UnexpectedIP != nil {
			device.unexpectedip = opts.UnexpectedIP
		} else {
			device.unexpectedip = func(key *NoisePublicKey, ip netaddr.IP) {
				device.log.Info.Printf("IPv4 packet with disallowed source address %s from %v", ip, key)
			}
		}
		device.handshakeDone = opts.HandshakeDone
		if opts.CreateEndpoint != nil {
			device.createEndpoint = opts.CreateEndpoint
		} else {
			device.createEndpoint = func(_ [32]byte, s string) (conn.Endpoint, error) {
				return conn.CreateEndpoint(s)
			}
		}
		if opts.CreateBind != nil {
			device.createBind = func(uport uint16, device *Device) (conn.Bind, uint16, error) {
				return opts.CreateBind(uport)
			}
		} else {
			device.createBind = func(uport uint16, device *Device) (conn.Bind, uint16, error) {
				return conn.CreateBind(uport)
			}
		}
		device.skipBindUpdate = opts.SkipBindUpdate
	}

	device.tun.device = tunDevice
	mtu, err := device.tun.device.MTU()
	if err != nil {
		device.log.Error.Println("Trouble determining MTU, assuming default:", err)
		mtu = DefaultMTU
	}
	device.tun.mtu = int32(mtu)

	device.peers.keyMap = make(map[NoisePublicKey]*Peer)

	device.rate.limiter.Init()
	device.rate.underLoadUntil.Store(time.Time{})

	device.indexTable.Init()
	device.allowedips.Reset()

	device.PopulatePools()

	// create queues

	device.queue.handshake = make(chan QueueHandshakeElement, QueueHandshakeSize)
	device.queue.encryption = newEncryptionQueue()
	device.queue.decryption = make(chan *QueueInboundElement, QueueInboundSize)

	// prepare signals

	device.signals.stop = make(chan struct{})

	// prepare net

	device.net.port = 0
	device.net.bind = nil

	// start workers

	cpus := runtime.NumCPU()
	device.state.stopping.Wait()
	for i := 0; i < cpus; i++ {
		device.state.stopping.Add(2) // decryption and handshake
		go device.RoutineEncryption()
		go device.RoutineDecryption()
		go device.RoutineHandshake()
	}

	device.state.stopping.Add(2)
	go device.RoutineReadFromTUN()
	go device.RoutineTUNEventReader()

	return device
}

func (device *Device) LookupPeer(pk NoisePublicKey) *Peer {
	device.peers.RLock()
	defer device.peers.RUnlock()

	return device.peers.keyMap[pk]
}

// RemovePeer stops the Peer and removes it from routing.
func (device *Device) RemovePeer(key NoisePublicKey) {
	device.peers.Lock()
	peer := device.peers.keyMap[key]
	if peer != nil {
		unsafeRemovePeer(device, peer, key)
	}
	device.peers.Unlock()

	if peer != nil {
		peer.Stop()
	}
}

func (device *Device) RemoveAllPeers() {
	var peersToStop []*Peer
	defer func() {
		for _, peer := range peersToStop {
			peer.Stop()
		}
	}()

	device.peers.Lock()
	defer device.peers.Unlock()
	for key, peer := range device.peers.keyMap {
		peersToStop = append(peersToStop, peer)
		unsafeRemovePeer(device, peer, key)
	}
	device.peers.keyMap = make(map[NoisePublicKey]*Peer)
}

func (device *Device) FlushPacketQueues() {
	for {
		select {
		case elem, ok := <-device.queue.decryption:
			if ok {
				elem.Drop()
				elem.Unlock()
			}
		case <-device.queue.handshake:
		default:
			return
		}
	}

}

func (device *Device) Close() {
	if device.isClosed.Swap(true) {
		return
	}

	device.log.Info.Println("Device closing")
	device.state.changing.Set(true)
	device.state.Lock()
	defer device.state.Unlock()

	device.tun.device.Close()
	device.BindClose()

	device.isUp.Set(false)

	// We kept a reference to the encryption queue,
	// in case we started any new peers that might write to it.
	// No new peers are coming; we are done with the encryption queue.
	device.queue.encryption.wg.Done()
	close(device.signals.stop)
	device.state.stopping.Wait()

	device.RemoveAllPeers()

	device.FlushPacketQueues()

	device.rate.limiter.Close()

	device.state.changing.Set(false)
	device.log.Info.Println("Interface closed")
}

func (device *Device) Wait() chan struct{} {
	return device.signals.stop
}

func (device *Device) SendKeepalivesToPeersWithCurrentKeypair() {
	if device.isClosed.Get() {
		return
	}

	device.peers.RLock()
	for _, peer := range device.peers.keyMap {
		peer.keypairs.RLock()
		sendKeepalive := peer.keypairs.current != nil && !peer.keypairs.current.created.Add(RejectAfterTime).Before(time.Now())
		peer.keypairs.RUnlock()
		if sendKeepalive {
			peer.SendKeepalive()
		}
	}
	device.peers.RUnlock()
}

func unsafeCloseBind(device *Device) error {
	var err error
	netc := &device.net
	if netc.netlinkCancel != nil {
		netc.netlinkCancel.Cancel()
	}
	if netc.bind != nil {
		err = netc.bind.Close()
		netc.bind = nil
	}
	netc.stopping.Wait()
	return err
}

func (device *Device) Bind() conn.Bind {
	device.net.Lock()
	defer device.net.Unlock()
	return device.net.bind
}

func (device *Device) BindSetMark(mark uint32) error {

	device.net.Lock()
	defer device.net.Unlock()

	// check if modified

	if device.net.fwmark == mark {
		return nil
	}

	// update fwmark on existing bind

	device.net.fwmark = mark
	if device.isUp.Get() && device.net.bind != nil {
		if err := device.net.bind.SetMark(mark); err != nil {
			return err
		}
	}

	// clear cached source addresses

	device.peers.RLock()
	for _, peer := range device.peers.keyMap {
		peer.Lock()
		defer peer.Unlock()
		if peer.endpoint != nil {
			peer.endpoint.ClearSrc()
		}
	}
	device.peers.RUnlock()

	return nil
}

func (device *Device) BindUpdate() error {

	device.net.Lock()
	defer device.net.Unlock()

	if device.skipBindUpdate && device.net.bind != nil {
		device.log.Debug.Println("UDP bind update skipped")
		return nil
	}

	// close existing sockets

	if err := unsafeCloseBind(device); err != nil {
		return err
	}

	// open new sockets

	if device.isUp.Get() {

		// bind to new port

		var err error
		netc := &device.net
		netc.bind, netc.port, err = device.createBind(netc.port, device)
		if err != nil {
			netc.bind = nil
			netc.port = 0
			return err
		}
		netc.netlinkCancel, err = device.startRouteListener(netc.bind)
		if err != nil {
			netc.bind.Close()
			netc.bind = nil
			netc.port = 0
			return err
		}

		// set fwmark

		if netc.fwmark != 0 {
			err = netc.bind.SetMark(netc.fwmark)
			if err != nil {
				return err
			}
		}

		// clear cached source addresses

		device.peers.RLock()
		for _, peer := range device.peers.keyMap {
			peer.Lock()
			defer peer.Unlock()
			if peer.endpoint != nil {
				peer.endpoint.ClearSrc()
			}
		}
		device.peers.RUnlock()

		// start receiving routines

		device.net.stopping.Add(2)
		go device.RoutineReceiveIncoming(ipv4.Version, netc.bind)
		go device.RoutineReceiveIncoming(ipv6.Version, netc.bind)

		device.log.Debug.Println("UDP bind has been updated")
	}

	return nil
}

func (device *Device) BindClose() error {
	device.net.Lock()
	err := unsafeCloseBind(device)
	device.net.Unlock()
	return err
}
