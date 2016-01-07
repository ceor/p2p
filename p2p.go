package main

import (
	"crypto/rand"
	"flag"
	"fmt"
	"github.com/danderson/tuntap"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"p2p/commons"
	"p2p/dht"
	//"p2p/enc"
	"p2p/udpcs"
	"sort"
	"strings"
	"time"
)

type MSG_TYPE uint16

// Main structure
type PTPCloud struct {

	// IP Address assigned to device at startup
	IP string

	// MAC Address assigned to device or generated by the application (TODO: Implement random generation and MAC assignment)
	Mac string

	HardwareAddr net.HardwareAddr

	// Netmask for device
	Mask string

	// Name of the device
	DeviceName string

	// Path to tool that is used to configure network device (only "ip" tools is supported at this moment)
	IPTool string `yaml:"iptool"`

	// TUN/TAP Interface
	Interface *os.File

	// Representation of TUN/TAP Device
	Device *tuntap.Interface

	NetworkPeers []NetworkPeer

	UDPSocket *udpcs.UDPClient

	LocalIPs []net.IP
}

type NetworkPeer struct {
	Unknown     bool
	Handshaked  bool
	CleanAddr   string
	ProxyID     int
	Forwarder   *net.UDPAddr
	PeerAddr    *net.UDPAddr
	PeerLocalIP net.IP
	PeerHW      net.HardwareAddr
}

// Creates TUN/TAP Interface and configures it with provided IP tool
func (ptp *PTPCloud) CreateDevice(ip, mac, mask, device string) error {
	var err error

	ptp.IP = ip
	ptp.Mac = mac
	ptp.Mask = mask
	ptp.DeviceName = device

	// Extract necessary information from config file
	// TODO: Remove hard-coded path
	yamlFile, err := ioutil.ReadFile("config.yaml")
	if err != nil {
		log.Printf("[ERROR] Failed to load config: %v", err)
		return err
	}
	err = yaml.Unmarshal(yamlFile, ptp)
	if err != nil {
		log.Printf("[ERROR] Failed to parse config: %v", err)
		return err
	}

	ptp.Device, err = tuntap.Open(ptp.DeviceName, tuntap.DevTap)
	if ptp.Device == nil {
		log.Fatalf("[FATAL] Failed to open TAP device: %v", err)
		return err
	} else {
		log.Printf("[INFO] %v TAP Device created", ptp.DeviceName)
	}

	linkup := exec.Command(ptp.IPTool, "link", "set", "dev", ptp.DeviceName, "up")
	err = linkup.Run()
	if err != nil {
		log.Fatalf("[ERROR] Failed to up link: %v", err)
		return err
	}

	// Configure new device
	log.Printf("[INFO] Setting %s IP on device %s\n", ptp.IP, ptp.DeviceName)
	setip := exec.Command(ptp.IPTool, "addr", "add", ptp.IP+"/24", "dev", ptp.DeviceName)
	err = setip.Run()
	if err != nil {
		log.Fatalf("[FATAL] Failed to set IP: %v", err)
		return err
	}

	// Set MAC to device
	log.Printf("[INFO] Setting %s MAC on device %s\n", mac, ptp.DeviceName)
	setmac := exec.Command(ptp.IPTool, "link", "set", "dev", ptp.DeviceName, "address", mac)
	err = setmac.Run()
	if err != nil {
		log.Fatalf("[FATAL] Failed to set MAC: %v", err)
		return err
	}
	return nil
}

// Handles a packet that was received by TUN/TAP device
// Receiving a packet by device means that some application sent a network
// packet within a subnet in which our application works.
// This method calls appropriate gorouting for extracted packet protocol
func handlePacket(ptp *PTPCloud, contents []byte, proto int) {
	/*
		512   (PUP)
		2048  (IP)
		2054  (ARP)
		32821 (RARP)
		33024 (802.1q)
		34525 (IPv6)
		34915 (PPPOE discovery)
		34916 (PPPOE session)
	*/
	switch proto {
	case 512:
		log.Printf("[DEBUG] Received PARC Universal Packet")
	case 2048:
		ptp.handlePacketIPv4(contents, proto)
	case 2054:
		log.Printf("[DEBUG] Received ARP Packet")
		ptp.handlePacketARP(contents)
	case 32821:
		log.Printf("[DEBUG] Received RARP Packet")
	case 33024:
		log.Printf("[DEBUG] Received 802.1q Packet")
	case 34525:
		ptp.handlePacketIPv6(contents)
	case 34915:
		log.Printf("[DEBUG] Received PPPoE Discovery Packet")
	case 34916:
		log.Printf("[DEBUG] Received PPPoE Session Packet")
	default:
		log.Printf("[DEBUG] Received Undefined Packet")
	}
}

// Listen TAP interface for incoming packets
func (ptp *PTPCloud) ListenInterface() {
	// Read packets received by TUN/TAP device and send them to a handlePacket goroutine
	// This goroutine will decide what to do with this packet
	for {
		packet, err := ptp.Device.ReadPacket()
		if err != nil {
			log.Printf("Error reading packet: %s", err)
		}
		if packet.Truncated {
			log.Printf("[DEBUG] Truncated packet")
		}
		// TODO: Make handlePacket as a part of PTPCloud
		go handlePacket(ptp, packet.Packet, packet.Protocol)
	}
}

func (ptp *PTPCloud) FindNetworkAddresses() {
	log.Printf("[INFO] Looking for available network interfaces")
	inf, err := net.Interfaces()
	if err != nil {
		log.Printf("[ERROR] Failed to retrieve list of network interfaces")
		return
	}
	for _, i := range inf {
		addresses, err := i.Addrs()

		if err != nil {
			log.Printf("[ERROR] Failed to retrieve address for interface %v", err)
			continue
		}
		for _, addr := range addresses {
			var decision string = "Ignoring"
			var ipType string = "Unknown"
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				log.Printf("[ERROR] Failed to parse CIDR notation: %v", err)
			}
			if ip.IsLoopback() {
				ipType = "Loopback"
			} else if ip.IsMulticast() {
				ipType = "Multicast"
			} else if ip.IsGlobalUnicast() {
				decision = "Saving"
				ipType = "Global Unicast"
			} else if ip.IsLinkLocalUnicast() {
				ipType = "Link Local Unicast"
			} else if ip.IsLinkLocalMulticast() {
				ipType = "Link Local Multicast"
			} else if ip.IsInterfaceLocalMulticast() {
				ipType = "Interface Local Multicast"
			}
			log.Printf("[INFO] Interface %s: %s. Type: %s. %s", i.Name, addr.String(), ipType, decision)
			if decision == "Saving" {
				ptp.LocalIPs = append(ptp.LocalIPs, ip)
			}
		}
	}
	log.Printf("[INFO] %d interfaces were saved", len(ptp.LocalIPs))
}

func main() {
	// TODO: Move this to init() function
	var (
		argIp     string
		argMask   string
		argMac    string
		argDev    string
		argDirect string
		argHash   string
	)

	// TODO: Improve this
	flag.StringVar(&argIp, "ip", "none", "IP Address to be used")
	// TODO: Parse this properly
	flag.StringVar(&argMask, "mask", "none", "Network mask")
	// TODO: Implement this
	flag.StringVar(&argMac, "mac", "none", "MAC Address for a TUN/TAP interface")
	flag.StringVar(&argDev, "dev", "none", "TUN/TAP interface name")
	// TODO: Direct connection is not implemented yet
	flag.StringVar(&argDirect, "direct", "none", "IP to connect to directly")
	flag.StringVar(&argHash, "hash", "none", "Infohash for environment")

	flag.Parse()
	if argIp == "none" || argMask == "none" || argDev == "none" {
		fmt.Println("USAGE: p2p [OPTIONS]")
		fmt.Printf("\nOPTIONS:\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	var hw net.HardwareAddr

	if argMac != "none" {
		var err2 error
		hw, err2 = net.ParseMAC(argMac)
		if err2 != nil {
			log.Printf("[ERROR] Invalid MAC address provided: %v", err2)
			return
		}
	} else {
		argMac, hw = GenerateMAC()
		log.Printf("[INFO] Generate MAC for TAP device: %s", argMac)
	}

	// Create new DHT Client, configured it and initialize
	// During initialization procedure, DHT Client will send
	// a introduction packet along with a hash to a DHT bootstrap
	// nodes that was hardcoded into it's code
	dhtClient := new(dht.DHTClient)
	config := dhtClient.DHTClientConfig()
	config.NetworkHash = argHash

	ptp := new(PTPCloud)
	ptp.FindNetworkAddresses()
	ptp.HardwareAddr = hw
	ptp.CreateDevice(argIp, argMac, argMask, argDev)
	ptp.UDPSocket = new(udpcs.UDPClient)
	ptp.UDPSocket.Init("", 0)
	port := ptp.UDPSocket.GetPort()
	log.Printf("[INFO] Started UDP Listener at port %d", port)
	config.P2PPort = port
	dhtClient = dhtClient.Initialize(config)

	go ptp.UDPSocket.Listen(ptp.HandleP2PMessage)

	// Capture SIGINT
	// This is used for development purposes only, but later we should consider updating
	// this code to handle signals
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)

	go func() {
		for sig := range c {
			fmt.Println("Received signal: ", sig)
			os.Exit(0)
		}
	}()

	go ptp.ListenInterface()
	for {
		time.Sleep(3 * time.Second)
		dhtClient.UpdatePeers()
		// Wait two seconds before synchronizing with catched peers
		time.Sleep(2 * time.Second)
		ptp.PurgePeers(dhtClient.LastCatch)
		newPeersNum := ptp.SyncPeers(dhtClient.LastCatch)
		if newPeersNum > 0 {
			ptp.IntroducePeers()
		}
	}
}

// This method sends information about himself to empty peers
// Empty peers is a peer that was not sent us information
// about his device
func (ptp *PTPCloud) IntroducePeers() {
	for i, peer := range ptp.NetworkPeers {
		if !peer.Unknown {
			continue
		}
		log.Printf("[DEBUG] Introducing to %s", peer.CleanAddr)
		addr, err := net.ResolveUDPAddr("udp", peer.CleanAddr)
		if err != nil {
			log.Printf("[ERROR] Failed to resolve UDP address during Introduction: %v", err)
			continue
		}
		ptp.NetworkPeers[i].PeerAddr = addr
		// Send introduction packet
		msg := ptp.PrepareIntroductionMessage(ptp.IP, ptp.Mac)
		_, err = ptp.UDPSocket.SendMessage(msg, addr)
		if err != nil {
			log.Printf("[ERROR] Failed to send introduction to %s", addr.String())
		}
	}
}

func (ptp *PTPCloud) PrepareIntroductionMessage(ip, mac string) *udpcs.P2PMessage {
	var intro string = ip + "," + mac
	msg := udpcs.CreateIntroP2PMessage(intro, 0)
	return msg
}

// This method goes over peers and removes obsolete ones
// Peer becomes obsolete when it goes out of DHT
func (ptp *PTPCloud) PurgePeers(catched []string) {
	var remove []int
	for i, peer := range ptp.NetworkPeers {
		var found bool = false
		for _, addr := range catched {
			if addr == peer.CleanAddr {
				found = true
			}
		}
		if !found {
			remove = append(remove, i)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(remove)))
	for i := range remove {
		ptp.NetworkPeers = append(ptp.NetworkPeers[:i], ptp.NetworkPeers[i+1:]...)
	}
}

// This method takes a list of catched peers from DHT and
// adds every new peer into list of peers
// Returns amount of peers that has been added
func (ptp *PTPCloud) SyncPeers(catched []string) int {
	var c int
	for _, addr := range catched {
		var found bool = false
		for _, peer := range ptp.NetworkPeers {
			if peer.CleanAddr == addr {
				found = true
			}
		}
		if !found {
			var newPeer NetworkPeer
			newPeer.CleanAddr = addr
			newPeer.Unknown = true
			ptp.NetworkPeers = append(ptp.NetworkPeers, newPeer)
			c = c + 1
		}
	}
	return c
}

// WriteToDevice writes data to created TUN/TAP device
func (ptp *PTPCloud) WriteToDevice(b []byte) {
	var p tuntap.Packet
	p.Protocol = 2054
	p.Truncated = false
	p.Packet = b
	if ptp.Device == nil {
		log.Printf("[ERROR] TUN/TAP Device not initialized")
		return
	}
	err := ptp.Device.WritePacket(&p)
	if err != nil {
		log.Printf("[ERROR] Failed to write to TUN/TAP device")
	}
}

func GenerateMAC() (string, net.HardwareAddr) {
	buf := make([]byte, 6)
	_, err := rand.Read(buf)
	if err != nil {
		log.Printf("[ERROR] Failed to generate MAC: %v", err)
		return "", nil
	}
	buf[0] |= 2
	mac := fmt.Sprintf("06:%02x:%02x:%02x:%02x:%02x", buf[1], buf[2], buf[3], buf[4], buf[5])
	hw, err := net.ParseMAC(mac)
	if err != nil {
		log.Printf("[ERROR] Corrupted MAC address generated: %v", err)
		return "", nil
	}
	return mac, hw
}

// AddPeer adds new peer into list of network participants. If peer was added previously
// information about him will be updated. If not, new entry will be added
func (ptp *PTPCloud) AddPeer(addr *net.UDPAddr, ip net.IP, mac net.HardwareAddr) {
	var found bool = false
	for i, peer := range ptp.NetworkPeers {
		if peer.CleanAddr == addr.String() {
			found = true
			ptp.NetworkPeers[i].PeerAddr = addr
			ptp.NetworkPeers[i].PeerLocalIP = ip
			ptp.NetworkPeers[i].PeerHW = mac
			ptp.NetworkPeers[i].Unknown = false
			ptp.NetworkPeers[i].Handshaked = true
		}
	}
	if !found {
		var newPeer NetworkPeer
		newPeer.CleanAddr = addr.String()
		newPeer.PeerAddr = addr
		newPeer.PeerLocalIP = ip
		newPeer.PeerHW = mac
		newPeer.Unknown = false
		newPeer.Handshaked = true
		ptp.NetworkPeers = append(ptp.NetworkPeers, newPeer)
	}
}

func (p *NetworkPeer) ProbeConnection() bool {
	return false
}

func (ptp *PTPCloud) ParseIntroString(intro string) (net.IP, net.HardwareAddr) {
	parts := strings.Split(intro, ",")
	if len(parts) != 2 {
		log.Printf("[ERROR] Failed to parse introduction stirng")
		return nil, nil
	}
	ip := net.ParseIP(parts[0])
	if ip == nil {
		log.Printf("[ERROR] Failed to parse IP address from introduction packet")
		return nil, nil
	}
	mac, err := net.ParseMAC(parts[1])
	if err != nil {
		log.Printf("[ERROR] Failed to parse MAC address from introduction packet: %v", err)
		return nil, nil
	}
	return ip, mac
}

func (ptp *PTPCloud) IsPeerUnknown(addr *net.UDPAddr) bool {
	for _, peer := range ptp.NetworkPeers {
		if peer.CleanAddr == addr.String() {
			return peer.Unknown
		}
	}
	return true
}

// Handler for new messages received from P2P network
func (ptp *PTPCloud) HandleP2PMessage(count int, src_addr *net.UDPAddr, err error, rcv_bytes []byte) {
	if err != nil {
		log.Printf("[ERROR] P2P Message Handle: %v", err)
		return
	}

	buf := make([]byte, count)
	copy(buf[:], rcv_bytes[:])

	msg, des_err := udpcs.P2PMessageFromBytes(buf)
	if des_err != nil {
		log.Printf("[ERROR] P2PMessageFromBytes error: %v", des_err)
		return
	}
	var msgType commons.MSG_TYPE = commons.MSG_TYPE(msg.Header.Type)
	switch msgType {
	case commons.MT_INTRO:
		log.Printf("[DEBUG] Introduction message received: %s", string(msg.Data))
		// Don't do anything if we already know everything about this peer
		if !ptp.IsPeerUnknown(src_addr) {
			return
		}
		ip, mac := ptp.ParseIntroString(string(msg.Data))
		ptp.AddPeer(src_addr, ip, mac)
		msg := ptp.PrepareIntroductionMessage(ptp.IP, ptp.Mac)
		_, err := ptp.UDPSocket.SendMessage(msg, src_addr)
		if err != nil {
			log.Printf("[ERROR] Failed to respond to introduction message: %v", err)
		}
	case commons.MT_NENC:
		log.Printf("[DEBUG] Received P2P Message")
		ptp.WriteToDevice(msg.Data)
	default:
		log.Printf("[ERROR] Unknown message received")
	}

	log.Printf("processed message from %s, msg_data : %s\n", src_addr.String(), msg.Data)
}

func (ptp *PTPCloud) SendTo(dst net.HardwareAddr, msg *udpcs.P2PMessage) (int, error) {
	for _, peer := range ptp.NetworkPeers {
		if peer.PeerHW.String() == dst.String() {
			size, err := ptp.UDPSocket.SendMessage(msg, peer.PeerAddr)
			return size, err
		}
	}
	return 0, nil
}
