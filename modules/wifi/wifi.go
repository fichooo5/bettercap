package wifi

import (
	"fmt"

	"net"
	"strconv"
	"sync"
	"time"

	"github.com/bettercap/bettercap/modules/utils"
	"github.com/bettercap/bettercap/network"
	"github.com/bettercap/bettercap/packets"
	"github.com/bettercap/bettercap/session"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pcap"

	"github.com/evilsocket/islazy/fs"
	"github.com/evilsocket/islazy/str"
	"github.com/evilsocket/islazy/tui"
)

type WiFiModule struct {
	session.SessionModule

	handle              *pcap.Handle
	source              string
	minRSSI             int
	channel             int
	hopPeriod           time.Duration
	hopChanges          chan bool
	frequencies         []int
	ap                  *network.AccessPoint
	stickChan           int
	skipBroken          bool
	pktSourceChan       chan gopacket.Packet
	pktSourceChanClosed bool
	deauthSkip          []net.HardwareAddr
	deauthSilent        bool
	deauthOpen          bool
	assocSkip           []net.HardwareAddr
	assocSilent         bool
	assocOpen           bool
	shakesFile          string
	apRunning           bool
	apConfig            packets.Dot11ApConfig
	writes              *sync.WaitGroup
	reads               *sync.WaitGroup
	chanLock            *sync.Mutex
	selector            *utils.ViewSelector
}

func NewWiFiModule(s *session.Session) *WiFiModule {
	mod := &WiFiModule{
		SessionModule: session.NewSessionModule("wifi", s),
		minRSSI:       -200,
		channel:       0,
		stickChan:     0,
		hopPeriod:     250 * time.Millisecond,
		hopChanges:    make(chan bool),
		ap:            nil,
		skipBroken:    true,
		apRunning:     false,
		deauthSkip:    []net.HardwareAddr{},
		deauthSilent:  false,
		deauthOpen:    false,
		assocSkip:     []net.HardwareAddr{},
		assocSilent:   false,
		assocOpen:     false,
		writes:        &sync.WaitGroup{},
		reads:         &sync.WaitGroup{},
		chanLock:      &sync.Mutex{},
	}

	mod.AddHandler(session.NewModuleHandler("wifi.recon on", "",
		"Start 802.11 wireless base stations discovery and channel hopping.",
		func(args []string) error {
			return mod.Start()
		}))

	mod.AddHandler(session.NewModuleHandler("wifi.recon off", "",
		"Stop 802.11 wireless base stations discovery and channel hopping.",
		func(args []string) error {
			return mod.Stop()
		}))

	mod.AddHandler(session.NewModuleHandler("wifi.recon MAC", "wifi.recon ((?:[0-9A-Fa-f]{2}[:-]){5}(?:[0-9A-Fa-f]{2}))",
		"Set 802.11 base station address to filter for.",
		func(args []string) error {
			bssid, err := net.ParseMAC(args[0])
			if err != nil {
				return err
			} else if ap, found := mod.Session.WiFi.Get(bssid.String()); found {
				mod.ap = ap
				mod.stickChan = ap.Channel()
				return nil
			}
			return fmt.Errorf("Could not find station with BSSID %s", args[0])
		}))

	mod.AddHandler(session.NewModuleHandler("wifi.recon clear", "",
		"Remove the 802.11 base station filter.",
		func(args []string) (err error) {
			mod.ap = nil
			mod.stickChan = 0
			mod.frequencies, err = network.GetSupportedFrequencies(mod.Session.Interface.Name())
			mod.hopChanges <- true
			return err
		}))

	mod.AddParam(session.NewIntParameter("wifi.rssi.min",
		"-200",
		"Minimum WiFi signal strength in dBm."))

	mod.AddHandler(session.NewModuleHandler("wifi.deauth BSSID", `wifi\.deauth ((?:[a-fA-F0-9:]{11,})|all|\*)`,
		"Start a 802.11 deauth attack, if an access point BSSID is provided, every client will be deauthenticated, otherwise only the selected client. Use 'all', '*' or a broadcast BSSID (ff:ff:ff:ff:ff:ff) to iterate every access point with at least one client and start a deauth attack for each one.",
		func(args []string) error {
			if args[0] == "all" || args[0] == "*" {
				args[0] = "ff:ff:ff:ff:ff:ff"
			}
			bssid, err := net.ParseMAC(args[0])
			if err != nil {
				return err
			}
			return mod.startDeauth(bssid)
		}))

	mod.AddParam(session.NewStringParameter("wifi.deauth.skip",
		"",
		"",
		"Comma separated list of BSSID to skip while sending deauth packets."))

	mod.AddParam(session.NewBoolParameter("wifi.deauth.silent",
		"false",
		"If true, messages from wifi.deauth will be suppressed."))

	mod.AddParam(session.NewBoolParameter("wifi.deauth.open",
		"true",
		"Send wifi deauth packets to open networks."))

	mod.AddHandler(session.NewModuleHandler("wifi.assoc BSSID", `wifi\.assoc ((?:[a-fA-F0-9:]{11,})|all|\*)`,
		"Send an association request to the selected BSSID in order to receive a RSN PMKID key. Use 'all', '*' or a broadcast BSSID (ff:ff:ff:ff:ff:ff) to iterate for every access point.",
		func(args []string) error {
			if args[0] == "all" || args[0] == "*" {
				args[0] = "ff:ff:ff:ff:ff:ff"
			}
			bssid, err := net.ParseMAC(args[0])
			if err != nil {
				return err
			}
			return mod.startAssoc(bssid)
		}))

	mod.AddParam(session.NewStringParameter("wifi.assoc.skip",
		"",
		"",
		"Comma separated list of BSSID to skip while sending association requests."))

	mod.AddParam(session.NewBoolParameter("wifi.assoc.silent",
		"false",
		"If true, messages from wifi.assoc will be suppressed."))

	mod.AddParam(session.NewBoolParameter("wifi.assoc.open",
		"false",
		"Send association requests to open networks."))

	mod.AddHandler(session.NewModuleHandler("wifi.ap", "",
		"Inject fake management beacons in order to create a rogue access point.",
		func(args []string) error {
			if err := mod.parseApConfig(); err != nil {
				return err
			} else {
				return mod.startAp()
			}
		}))

	mod.AddParam(session.NewStringParameter("wifi.handshakes.file",
		"~/bettercap-wifi-handshakes.pcap",
		"",
		"File path of the pcap file to save handshakes to."))

	mod.AddParam(session.NewStringParameter("wifi.ap.ssid",
		"FreeWiFi",
		"",
		"SSID of the fake access point."))

	mod.AddParam(session.NewStringParameter("wifi.ap.bssid",
		session.ParamRandomMAC,
		"[a-fA-F0-9]{2}:[a-fA-F0-9]{2}:[a-fA-F0-9]{2}:[a-fA-F0-9]{2}:[a-fA-F0-9]{2}:[a-fA-F0-9]{2}",
		"BSSID of the fake access point."))

	mod.AddParam(session.NewIntParameter("wifi.ap.channel",
		"1",
		"Channel of the fake access point."))

	mod.AddParam(session.NewBoolParameter("wifi.ap.encryption",
		"true",
		"If true, the fake access point will use WPA2, otherwise it'll result as an open AP."))

	mod.AddHandler(session.NewModuleHandler("wifi.show.wps BSSID",
		`wifi\.show\.wps ((?:[a-fA-F0-9:]{11,})|all|\*)`,
		"Show WPS information about a given station (use 'all', '*' or a broadcast BSSID for all).",
		func(args []string) error {
			if args[0] == "all" || args[0] == "*" {
				args[0] = "ff:ff:ff:ff:ff:ff"
			}
			return mod.ShowWPS(args[0])
		}))

	mod.AddHandler(session.NewModuleHandler("wifi.show", "",
		"Show current wireless stations list (default sorting by essid).",
		func(args []string) error {
			return mod.Show()
		}))

	mod.selector = utils.ViewSelectorFor(&mod.SessionModule, "wifi.show",
		[]string{"rssi", "bssid", "essid", "channel", "encryption", "clients", "seen", "sent", "rcvd"}, "rssi asc")

	mod.AddHandler(session.NewModuleHandler("wifi.recon.channel", `wifi\.recon\.channel[\s]+([0-9]+(?:[, ]+[0-9]+)*|clear)`,
		"WiFi channels (comma separated) or 'clear' for channel hopping.",
		func(args []string) (err error) {
			freqs := []int{}

			if args[0] != "clear" {
				mod.Debug("setting hopping channels to %s", args[0])
				for _, s := range str.Comma(args[0]) {
					if ch, err := strconv.Atoi(s); err != nil {
						return err
					} else {
						if f := network.Dot11Chan2Freq(ch); f == 0 {
							return fmt.Errorf("%d is not a valid wifi channel.", ch)
						} else {
							freqs = append(freqs, f)
						}
					}
				}
			}

			if len(freqs) == 0 {
				mod.Debug("resetting hopping channels")
				if freqs, err = network.GetSupportedFrequencies(mod.Session.Interface.Name()); err != nil {
					return err
				}
			}

			mod.Debug("new frequencies: %v", freqs)
			mod.frequencies = freqs

			// if wifi.recon is not running, this would block forever
			if mod.Running() {
				mod.hopChanges <- true
			}

			return nil
		}))

	mod.AddParam(session.NewStringParameter("wifi.source.file",
		"",
		"",
		"If set, the wifi module will read from this pcap file instead of the hardware interface."))

	mod.AddParam(session.NewIntParameter("wifi.hop.period",
		"250",
		"If channel hopping is enabled (empty wifi.recon.channel), this is the time in milliseconds the algorithm will hop on every channel (it'll be doubled if both 2.4 and 5.0 bands are available)."))

	mod.AddParam(session.NewBoolParameter("wifi.skip-broken",
		"true",
		"If true, dot11 packets with an invalid checksum will be skipped."))

	return mod
}

func (mod WiFiModule) Name() string {
	return "wifi"
}

func (mod WiFiModule) Description() string {
	return "A module to monitor and perform wireless attacks on 802.11."
}

func (mod WiFiModule) Author() string {
	return "Simone Margaritelli <evilsocket@gmail.com> && Gianluca Braga <matrix86@gmail.com>"
}

const (
	// Ugly, but gopacket folks are not exporting pcap errors, so ...
	// ref. https://github.com/google/gopacket/blob/96986c90e3e5c7e01deed713ff8058e357c0c047/pcap/pcap.go#L281
	ErrIfaceNotUp = "Interface Not Up"
)

func (mod *WiFiModule) Configure() error {
	var hopPeriod int
	var err error

	if err, mod.source = mod.StringParam("wifi.source.file"); err != nil {
		return err
	}

	if err, mod.shakesFile = mod.StringParam("wifi.handshakes.file"); err != nil {
		return err
	} else if mod.shakesFile != "" {
		if mod.shakesFile, err = fs.Expand(mod.shakesFile); err != nil {
			return err
		}
	}

	if err, mod.minRSSI = mod.IntParam("wifi.rssi.min"); err != nil {
		return err
	}

	ifName := mod.Session.Interface.Name()

	if mod.source != "" {
		if mod.handle, err = pcap.OpenOffline(mod.source); err != nil {
			return fmt.Errorf("error while opening file %s: %s", mod.source, err)
		}
	} else {
		for retry := 0; ; retry++ {
			ihandle, err := pcap.NewInactiveHandle(ifName)
			if err != nil {
				return fmt.Errorf("error while opening interface %s: %s", ifName, err)
			}
			defer ihandle.CleanUp()

			if err = ihandle.SetRFMon(true); err != nil {
				return fmt.Errorf("error while setting interface %s in monitor mode: %s", tui.Bold(ifName), err)
			} else if err = ihandle.SetSnapLen(65536); err != nil {
				return fmt.Errorf("error while settng span len: %s", err)
			}
			/*
			 * We don't want to pcap.BlockForever otherwise pcap_close(handle)
			 * could hang waiting for a timeout to expire ...
			 */
			readTimeout := 500 * time.Millisecond
			if err = ihandle.SetTimeout(readTimeout); err != nil {
				return fmt.Errorf("error while setting timeout: %s", err)
			} else if mod.handle, err = ihandle.Activate(); err != nil {
				if retry == 0 && err.Error() == ErrIfaceNotUp {
					mod.Warning("interface %s is down, bringing it up ...", ifName)
					if err := network.ActivateInterface(ifName); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("error while activating handle: %s", err)
			}

			break
		}
	}

	if err, mod.skipBroken = mod.BoolParam("wifi.skip-broken"); err != nil {
		return err
	} else if err, hopPeriod = mod.IntParam("wifi.hop.period"); err != nil {
		return err
	}

	mod.hopPeriod = time.Duration(hopPeriod) * time.Millisecond

	if mod.source == "" {
		// No channels setted, retrieve frequencies supported by the card
		if len(mod.frequencies) == 0 {
			if mod.frequencies, err = network.GetSupportedFrequencies(ifName); err != nil {
				return fmt.Errorf("error while getting supported frequencies of %s: %s", ifName, err)
			}

			mod.Debug("wifi supported frequencies: %v", mod.frequencies)

			// we need to start somewhere, this is just to check if
			// this OS supports switching channel programmatically.
			if err = network.SetInterfaceChannel(ifName, 1); err != nil {
				return fmt.Errorf("error while initializing %s to channel 1: %s", ifName, err)
			}
			mod.Info("started (min rssi: %d dBm)", mod.minRSSI)
		}
	}

	return nil
}

func (mod *WiFiModule) updateInfo(dot11 *layers.Dot11, packet gopacket.Packet) {
	if ok, enc, cipher, auth := packets.Dot11ParseEncryption(packet, dot11); ok {
		bssid := dot11.Address3.String()
		if station, found := mod.Session.WiFi.Get(bssid); found {
			station.Encryption = enc
			station.Cipher = cipher
			station.Authentication = auth
		}
	}

	if ok, bssid, info := packets.Dot11ParseWPS(packet, dot11); ok {
		if station, found := mod.Session.WiFi.Get(bssid.String()); found {
			for name, value := range info {
				station.WPS[name] = value
			}
		}
	}
}

func (mod *WiFiModule) updateStats(dot11 *layers.Dot11, packet gopacket.Packet) {
	// collect stats from data frames
	if dot11.Type.MainType() == layers.Dot11TypeData {
		bytes := uint64(len(packet.Data()))

		dst := dot11.Address1.String()
		if station, found := mod.Session.WiFi.Get(dst); found {
			station.Received += bytes
		}

		src := dot11.Address2.String()
		if station, found := mod.Session.WiFi.Get(src); found {
			station.Sent += bytes
		}
	}
}

func (mod *WiFiModule) Start() error {
	if err := mod.Configure(); err != nil {
		return err
	}

	mod.SetRunning(true, func() {
		// start channel hopper if needed
		if mod.channel == 0 && mod.source == "" {
			go mod.channelHopper()
		}

		// start the pruner
		go mod.stationPruner()

		mod.reads.Add(1)
		defer mod.reads.Done()

		src := gopacket.NewPacketSource(mod.handle, mod.handle.LinkType())
		mod.pktSourceChan = src.Packets()
		for packet := range mod.pktSourceChan {
			if !mod.Running() {
				break
			} else if packet == nil {
				continue
			}

			mod.Session.Queue.TrackPacket(uint64(len(packet.Data())))

			// perform initial dot11 parsing and layers validation
			if ok, radiotap, dot11 := packets.Dot11Parse(packet); ok {
				// check FCS checksum
				if mod.skipBroken && !dot11.ChecksumValid() {
					mod.Debug("skipping dot11 packet with invalid checksum.")
					continue
				}

				mod.discoverProbes(radiotap, dot11, packet)
				mod.discoverAccessPoints(radiotap, dot11, packet)
				mod.discoverClients(radiotap, dot11, packet)
				mod.discoverHandshakes(radiotap, dot11, packet)
				mod.updateInfo(dot11, packet)
				mod.updateStats(dot11, packet)
			}
		}

		mod.pktSourceChanClosed = true
	})

	return nil
}

func (mod *WiFiModule) Stop() error {
	return mod.SetRunning(false, func() {
		// wait any pending write operation
		mod.writes.Wait()
		// signal the main for loop we want to exit
		if !mod.pktSourceChanClosed {
			mod.pktSourceChan <- nil
		}
		mod.reads.Wait()
		// close the pcap handle to make the main for exit
		mod.handle.Close()
	})
}