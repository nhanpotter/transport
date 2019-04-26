package vnet

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/pion/logging"
)

const (
	defaultRouterQueueSize = 0 // unlimited
)

// RouterConfig ...
type RouterConfig struct {
	// CIDR notation, like "192.0.2.0/24"
	CIDR string
	// StaticIP is a static IP address to be assigned for this external network.
	// This will be ignored if this router is the root.
	StaticIP string
	// Internal queue size
	QueueSize int
	// Effective only when this router has a parent router
	NATType *NATType
	// Logger factory
	LoggerFactory logging.LoggerFactory
}

// NIC is a nework inerface controller that interfaces Router
type NIC interface {
	getInterface(ifName string) (*Interface, error)
	onInboundChunk(c Chunk)
	getStaticIP() net.IP
	setRouter(r *Router) error
}

// Router ...
type Router struct {
	interfaces    []*Interface
	ipv4Net       *net.IPNet
	staticIP      net.IP
	lastID        byte // used to assign the last digit of IPv4 address
	queue         *chunkQueue
	parent        *Router
	children      []*Router
	natType       *NATType
	nat           *networkAddressTranslator
	nics          map[string]NIC // https://stackoverflow.com/questions/50426955/net-ip-as-map-key-type-in-golang
	stopFunc      func()
	resolver      *resolver
	mutex         sync.RWMutex
	pushCh        chan struct{}
	loggerFactory logging.LoggerFactory
	log           logging.LeveledLogger
}

// NewRouter ...
func NewRouter(config *RouterConfig) (*Router, error) {
	_, ipNet, err := net.ParseCIDR(config.CIDR)
	if err != nil {
		return nil, err
	}

	queueSize := defaultRouterQueueSize
	if config.QueueSize > 0 {
		queueSize = config.QueueSize
	}

	// set up network interface, lo0
	lo0 := NewInterface(net.Interface{
		Index:        1,
		MTU:          16384,
		Name:         "lo0",
		HardwareAddr: nil,
		Flags:        net.FlagUp | net.FlagLoopback | net.FlagMulticast,
	})
	lo0.AddAddr(&net.IPAddr{IP: net.ParseIP("127.0.0.1"), Zone: ""})

	// set up network interface, eth0
	eth0 := NewInterface(net.Interface{
		Index:        2,
		MTU:          1500,
		Name:         "eth0",
		HardwareAddr: newMACAddress(),
		Flags:        net.FlagUp | net.FlagMulticast,
	})

	// local host name resolver
	resolver := newResolver(&resolverConfig{
		LoggerFactory: config.LoggerFactory,
	})

	return &Router{
		interfaces:    []*Interface{lo0, eth0},
		ipv4Net:       ipNet,
		staticIP:      net.ParseIP(config.StaticIP),
		queue:         newChunkQueue(queueSize),
		natType:       config.NATType,
		nics:          map[string]NIC{},
		resolver:      resolver,
		pushCh:        make(chan struct{}, 1),
		loggerFactory: config.LoggerFactory,
		log:           config.LoggerFactory.NewLogger("vnet"),
	}, nil
}

// caller must hold the mutex
func (r *Router) getInterfaces() ([]*Interface, error) {
	if len(r.interfaces) == 0 {
		return nil, fmt.Errorf("no interface is available")
	}

	return r.interfaces, nil
}

func (r *Router) getInterface(ifName string) (*Interface, error) {
	r.mutex.RLock()
	defer r.mutex.RUnlock()

	ifs, err := r.getInterfaces()
	if err != nil {
		return nil, err
	}
	for _, ifc := range ifs {
		if ifc.Name == ifName {
			return ifc, nil
		}
	}

	return nil, fmt.Errorf("interface %s not found", ifName)
}

// Start ...
func (r *Router) Start() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.stopFunc != nil {
		return fmt.Errorf("router already staretd")
	}

	cancelCh := make(chan struct{})

	go func() {
	loop:
		for {
			err := r.onProcessChunks()
			if err != nil {
				r.log.Warn(err.Error())
			}
			select {
			case <-r.pushCh:
			case <-cancelCh:
				break loop
			}
		}
	}()

	r.stopFunc = func() {
		close(cancelCh)
	}

	for _, child := range r.children {
		if err := child.Start(); err != nil {
			return err
		}
	}

	return nil
}

// Stop ...
func (r *Router) Stop() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.stopFunc == nil {
		return fmt.Errorf("router already stopped")
	}

	for _, router := range r.children {
		if err := router.Stop(); err != nil {
			return err
		}
	}

	r.stopFunc()
	r.stopFunc = nil
	return nil
}

// caller must hold the mutex
func (r *Router) addNIC(nic NIC) error {
	ifc, err := nic.getInterface("eth0")
	if err != nil {
		return err
	}

	var ip net.IP
	if ip = nic.getStaticIP(); ip != nil {
		if !r.ipv4Net.Contains(ip) {
			return fmt.Errorf("static IP is beyond subnet: %s", r.ipv4Net.String())
		}
	} else {
		// assign an IP address
		ip, err = r.assignIPAddress()
		if err != nil {
			return err
		}
	}

	ifc.AddAddr(&net.IPNet{
		IP:   ip,
		Mask: r.ipv4Net.Mask,
	})

	if err = nic.setRouter(r); err != nil {
		return err
	}

	r.nics[ip.String()] = nic
	return nil
}

// AddRouter adds a chile Router.
func (r *Router) AddRouter(router *Router) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	// Router is a NIC. Add it as a NIC so that packets are routed to this child
	// router.
	err := r.addNIC(router)
	if err != nil {
		return err
	}

	if err = router.setRouter(r); err != nil {
		return err
	}

	r.children = append(r.children, router)
	return nil
}

// AddNet ...
func (r *Router) AddNet(nic NIC) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return r.addNIC(nic)
}

// AddHost adds a mapping of hostname and an IP address to the local resolver.
func (r *Router) AddHost(hostName string, ip net.IP) {
	r.resolver.addHost(hostName, ip)
}

// caller should hold the mutex
func (r *Router) assignIPAddress() (net.IP, error) {
	// See: https://stackoverflow.com/questions/14915188/ip-address-ending-with-zero

	if r.lastID == 0xfe {
		return nil, fmt.Errorf("address space exhausted")
	}

	ip := make(net.IP, 4)
	copy(ip, r.ipv4Net.IP[:3])
	r.lastID++
	ip[3] = r.lastID
	return ip, nil
}

func (r *Router) push(c Chunk) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.log.Debugf("Router.push: %s", c.String())
	if r.stopFunc != nil {
		c.setTimestamp()
		if r.queue.push(c) {
			select {
			case r.pushCh <- struct{}{}:
			default:
			}
		} else {
			r.log.Warn("queue was full. dropped a chunk")
		}
	}
}

func (r *Router) onProcessChunks() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	for {
		c := r.queue.peek()
		if c == nil {
			break // no more chunk in the queue
		}

		// TODO: check timestamp to decide whether to delay the chunks

		var ok bool
		if c, ok = r.queue.pop(); !ok {
			break // no more chunk in the queue
		}

		dstIP := c.getDestinationIP()

		// check if the desination is in our subnet
		if r.ipv4Net.Contains(dstIP) {
			// search for the destination NIC
			var nic NIC
			if nic, ok = r.nics[dstIP.String()]; !ok {
				// NIC not found. drop it.
				continue
			}

			// found the NIC, forward the chunk to the NIC.
			r.mutex.Unlock()
			nic.onInboundChunk(c)
			r.mutex.Lock()
			continue
		}

		// the destination is outside of this subnet
		// is this WAN?
		if r.parent == nil {
			// this WAN. No route for this chunk
			continue
		}

		// Pass it to the parent via NAT
		toParent, err := r.nat.translateOutbound(c)
		if err != nil {
			return err
		}

		if r.nat.natType.Hairpining {
			hairpinned, err := r.nat.translateInbound(toParent)
			if err == nil {
				go func() {
					r.push(hairpinned)
				}()
			}
		}

		r.parent.push(toParent)
	}

	return nil
}

// caller must hold the mutex
func (r *Router) setRouter(parent *Router) error {
	r.parent = parent
	r.resolver.setParent(parent.resolver)

	// when this method is called, an IP address has already been assigned by
	// the parent router.
	ifc, err := r.getInterface("eth0")
	if err != nil {
		return err
	}

	if len(ifc.addrs) == 0 {
		return fmt.Errorf("no IP address is assigned for eth0")
	}

	var ip net.IP
	switch addr := ifc.addrs[0].(type) {
	case *net.IPNet:
		ip = addr.IP
	case *net.IPAddr:
		ip = addr.IP
	default:
		return fmt.Errorf("unexpected address type for eth0")
	}

	mappedIP := ip.String()

	// Set up NAT here
	if r.natType == nil {
		r.natType = &NATType{
			MappingBehavior:   EndpointIndependent,
			FilteringBehavior: EndpointAddrPortDependent,
			Hairpining:        false,
			PortPreservation:  false,
			MappingLifeTime:   30 * time.Second,
		}
	}
	r.nat = newNAT(&natConfig{
		natType:       *r.natType,
		mappedIP:      mappedIP,
		loggerFactory: r.loggerFactory,
	})

	return nil
}

func (r *Router) onInboundChunk(c Chunk) {
	fromParent, err := r.nat.translateInbound(c)
	if err != nil {
		r.log.Warn(err.Error())
		return
	}

	r.push(fromParent)
}

func (r *Router) getStaticIP() net.IP {
	return r.staticIP
}
