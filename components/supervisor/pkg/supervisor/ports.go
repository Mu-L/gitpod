package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gitpod-io/gitpod/common-go/log"
	"github.com/gitpod-io/gitpod/supervisor/api"
	"github.com/google/tcpproxy"
	"golang.org/x/xerrors"
)

const (
	portRefreshInterval = 2 * time.Second
	maxSubscriptions    = 10

	fnNetTCP  = "/proc/net/tcp"
	fnNetTCP6 = "/proc/net/tcp6"
)

var (
	// proxyPortRange is the port range in which we'll try to find
	// ports for proxying localhost-only services.
	proxyPortRange = struct {
		Low  uint32
		High uint32
	}{
		50000,
		60000,
	}
)

func newPortsManager(internalPorts ...uint32) *portsManager {
	state := make(map[uint32]*managedPort)
	for _, p := range internalPorts {
		state[p] = &managedPort{Internal: true}
	}

	return &portsManager{
		state:         state,
		subscriptions: make(map[*portsSubscription]struct{}),
	}
}

type portsManager struct {
	state         map[uint32]*managedPort
	subscriptions map[*portsSubscription]struct{}
	mu            sync.RWMutex
}

type managedPort struct {
	Internal      bool
	LocalhostPort uint32
	GloalPort     uint32
	Proxy         io.Closer
}

// Run runs the port manager - this function does not return
func (pm *portsManager) Run(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()

	t := time.NewTicker(portRefreshInterval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		var ports []netPort
		for _, fn := range []string{fnNetTCP, fnNetTCP6} {
			fc, err := os.Open(fn)
			if err != nil {
				log.WithError(err).WithField("fn", fn).Warn("cannot update used ports")
				continue
			}
			ps, err := readNetTCPFile(fc, true)
			fc.Close()

			if err != nil {
				log.WithError(err).WithField("fn", fn).Warn("cannot update used ports")
				continue
			}
			ports = append(ports, ps...)
		}

		pm.updateState(ports)
	}
}

func (pm *portsManager) Subscribe() *portsSubscription {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if len(pm.subscriptions) > maxSubscriptions {
		return nil
	}

	sub := &portsSubscription{updates: make(chan []*api.PortsStatus, 5)}
	sub.Close = func() error {
		pm.mu.Lock()
		defer pm.mu.Unlock()

		// We can safely close the channel here even though we're not the
		// producer writing to it, because we're holding mu.
		close(sub.updates)
		delete(pm.subscriptions, sub)

		return nil
	}
	pm.subscriptions[sub] = struct{}{}

	return sub
}

type portsSubscription struct {
	updates chan []*api.PortsStatus
	Close   func() error
}

func (sub *portsSubscription) Updates() <-chan []*api.PortsStatus {
	return sub.updates
}

func (pm *portsManager) ServedPorts() []*api.PortsStatus {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	return pm.getStatus()
}

func (pm *portsManager) getStatus() []*api.PortsStatus {
	res := make([]*api.PortsStatus, 0, len(pm.state))
	for _, p := range pm.state {
		if p.Internal {
			continue
		}

		res = append(res, &api.PortsStatus{
			GlobalPort: p.GloalPort,
			LocalPort:  p.LocalhostPort,
		})
	}
	return res
}

func (pm *portsManager) updateState(listeningPorts []netPort) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	openPorts := make(map[uint32]struct{}, len(listeningPorts))
	for _, p := range listeningPorts {
		openPorts[p.Port] = struct{}{}
	}

	// add new ports
	var changes bool
	for _, p := range listeningPorts {
		if _, exists := pm.state[p.Port]; exists {
			continue
		}

		mp := &managedPort{
			LocalhostPort: p.Port,
		}
		if p.Localhost {
			err := startTCPProxy(mp, openPorts)
			if err != nil {
				log.WithError(err).WithField("port", p.Port).Warn("cannot start localhost proxy")
			}
		} else {
			// we don't need a proxy - the port is globally bound
			mp.GloalPort = p.Port
		}

		pm.state[p.Port] = mp
		changes = true
	}

	// remove closed ports
	for p, mp := range pm.state {
		if mp.Internal {
			continue
		}
		if _, ok := openPorts[p]; ok {
			continue
		}

		if mp.Proxy != nil {
			err := mp.Proxy.Close()
			if err != nil {
				log.WithError(err).WithField("port", p).Warn("cannot stop localhost proxy")
			}
		}

		delete(pm.state, p)
		changes = true
	}

	// TODO(cw): trigger listener updates once we support port status update listener
	if !changes {
		return
	}
	status := pm.getStatus()
	for sub := range pm.subscriptions {
		select {
		case sub.updates <- status:
		default:
			log.Warn("cannot to push ports update to a subscriber")
		}
	}
}

func startTCPProxy(dst *managedPort, openPorts map[uint32]struct{}) (err error) {
	var proxyPort uint32
	for prt := proxyPortRange.High; prt >= proxyPortRange.Low; prt-- {
		if _, used := openPorts[prt]; used {
			continue
		}

		proxyPort = prt
		break
	}
	if proxyPort == 0 {
		return xerrors.Errorf("cannot find a free proxy port")
	}

	var p tcpproxy.Proxy
	p.AddRoute(fmt.Sprintf(":%d", proxyPort), tcpproxy.To(fmt.Sprintf("localhost:%d", dst.LocalhostPort)))

	err = p.Start()
	if err != nil {
		return xerrors.Errorf("cannot start proxy: %w", err)
	}

	dst.Proxy = &p
	dst.GloalPort = proxyPort
	return nil
}

type netPort struct {
	Port      uint32
	Localhost bool
}

func readNetTCPFile(fc io.Reader, listeningOnly bool) (ports []netPort, err error) {
	scanner := bufio.NewScanner(fc)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		if listeningOnly && fields[3] != "0A" {
			continue
		}

		segs := strings.Split(fields[1], ":")
		if len(segs) < 2 {
			continue
		}
		addr, prt := segs[0], segs[1]

		globallyBound := addr == "00000000" || addr == "00000000000000000000000000000000"
		port, err := strconv.ParseUint(prt, 16, 32)
		if err != nil {
			log.WithError(err).WithField("port", prt).Warn("cannot parse port entry from /proc/net/tcp* file")
			continue
		}

		ports = append(ports, netPort{
			Localhost: !globallyBound,
			Port:      uint32(port),
		})
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}

	return
}
