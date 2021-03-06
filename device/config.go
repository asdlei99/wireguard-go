// SPDX-License-Identifier: MIT

package device

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tailscale/wireguard-go/ipc"
	"github.com/tailscale/wireguard-go/wgcfg"
	"inet.af/netaddr"
)

func (device *Device) Config() *wgcfg.Config {
	cfg, err := device.config()
	if err != nil {
		device.log.Error.Println("Config failed:", err.Error())
	}
	return cfg
}

func (device *Device) config() (*wgcfg.Config, error) {
	r, w := io.Pipe()
	errc := make(chan error, 1)
	go func() {
		errc <- device.IpcGetOperation(w)
		w.Close()
	}()
	cfg, err := wgcfg.FromUAPI(r)
	if err != nil {
		return nil, err
	}
	if err := <-errc; err != nil {
		return nil, err
	}

	sort.Slice(cfg.Peers, func(i, j int) bool {
		return cfg.Peers[i].PublicKey.LessThan(&cfg.Peers[j].PublicKey)
	})
	return cfg, nil
}

// Reconfig replaces the existing device configuration with cfg.
func (device *Device) Reconfig(cfg *wgcfg.Config) (err error) {
	defer func() {
		if err != nil {
			device.log.Debug.Printf("device.Reconfig: failed: %v", err)
			device.RemoveAllPeers()
		}
	}()

	// Remove any current peers not in the new configuration.
	device.peers.RLock()
	oldPeers := make(map[NoisePublicKey]bool)
	for k := range device.peers.keyMap {
		oldPeers[k] = true
	}
	device.peers.RUnlock()
	for _, p := range cfg.Peers {
		delete(oldPeers, NoisePublicKey(p.PublicKey))
	}
	for k := range oldPeers {
		wk := wgcfg.Key(k)
		device.log.Debug.Printf("device.Reconfig: removing old peer %s", wk.ShortString())
		device.RemovePeer(k)
	}

	device.staticIdentity.Lock()
	curPrivKey := device.staticIdentity.privateKey
	device.staticIdentity.Unlock()

	if !curPrivKey.Equals(NoisePrivateKey(cfg.PrivateKey)) {
		device.log.Debug.Println("device.Reconfig: resetting private key")
		if err := device.SetPrivateKey(NoisePrivateKey(cfg.PrivateKey)); err != nil {
			return err
		}
	}

	device.net.Lock()
	device.net.port = cfg.ListenPort
	device.net.Unlock()

	if err := device.BindUpdate(); err != nil {
		return ErrPortInUse
	}

	// TODO(crawshaw): UAPI supports an fwmark field

	newKeepalivePeers := make(map[wgcfg.Key]*Peer)
	for _, p := range cfg.Peers {
		peer := device.LookupPeer(NoisePublicKey(p.PublicKey))
		if peer == nil {
			device.log.Debug.Printf("device.Reconfig: new peer %s", p.PublicKey.ShortString())
			peer, err = device.NewPeer(NoisePublicKey(p.PublicKey))
			if err != nil {
				return err
			}
			if p.PersistentKeepalive != 0 && device.isUp.Get() {
				newKeepalivePeers[p.PublicKey] = peer
			}
		}

		peer.Lock()
		atomic.StoreUint32(&peer.persistentKeepaliveInterval, uint32(p.PersistentKeepalive))
		if p.Endpoints != "" && (peer.endpoint == nil || !endpointsEqual(p.Endpoints, peer.endpoint.Addrs())) {
			ep, err := device.createEndpoint(p.PublicKey, p.Endpoints)
			if err != nil {
				peer.Unlock()
				return err
			}
			peer.endpoint = ep

			// TODO(crawshaw): whether or not a new keepalive is necessary
			// on changing the endpoint depends on the semantics of the
			// CreateEndpoint func, which is not properly defined. Define it.
			if p.PersistentKeepalive != 0 && device.isUp.Get() {
				newKeepalivePeers[p.PublicKey] = peer

				// Make sure the new handshake will get fired.
				peer.handshake.mutex.Lock()
				peer.handshake.lastSentHandshake = time.Now().Add(-RekeyTimeout)
				peer.handshake.mutex.Unlock()
			}
		}
		allowedIPsChanged := !cidrsEqual(peer.allowedIPs, p.AllowedIPs)
		if allowedIPsChanged {
			peer.allowedIPs = append([]netaddr.IPPrefix(nil), p.AllowedIPs...)
		}
		peer.Unlock()

		if allowedIPsChanged {
			// RemoveByPeer is currently (2020-07-24) very
			// expensive on large networks, so we avoid
			// calling it when possible.
			device.allowedips.RemoveByPeer(peer)
		}
		// DANGER: allowedIP is a value type. Its contents (the IP and
		// Mask) are overwritten on every iteration through the
		// loop. The loop owns its memory; don't retain references into it.
		for _, allowedIP := range p.AllowedIPs {
			ones := uint(allowedIP.Bits)
			ip := allowedIP.IP.IPAddr().IP
			if allowedIP.IP.Is4() {
				ip = ip.To4()
			}
			device.allowedips.Insert(ip, ones, peer)
		}
	}

	// Send immediate keepalive if we're turning it on and before it wasn't on.
	for k, peer := range newKeepalivePeers {
		device.log.Debug.Printf("device.Reconfig: sending keepalive to peer %s", k.ShortString())
		peer.SendKeepalive()
	}

	return nil
}

func endpointsEqual(x, y string) bool {
	// Cheap comparisons.
	if x == y {
		return true
	}
	xs := strings.Split(x, ",")
	ys := strings.Split(y, ",")
	if len(xs) != len(ys) {
		return false
	}
	// Otherwise, see if they're the same, but out of order.
	sort.Strings(xs)
	sort.Strings(ys)
	x = strings.Join(xs, ",")
	y = strings.Join(ys, ",")
	return x == y
}

func cidrsEqual(x, y []netaddr.IPPrefix) bool {
	if len(x) != len(y) {
		return false
	}
	// First see if they're equal in order, without allocating.
	exact := true
	for i := range x {
		if x[i] != y[i] {
			exact = false
			break
		}
	}
	if exact {
		return true
	}

	// Otherwise, see if they're the same, but out of order.
	m := make(map[netaddr.IPPrefix]bool)
	for _, v := range x {
		m[v] = true
	}
	for _, v := range y {
		if !m[v] {
			return false
		}
	}
	return true
}

var ErrPortInUse = fmt.Errorf("wireguard: local port in use: %w", &IPCError{ipc.IpcErrorPortInUse})
