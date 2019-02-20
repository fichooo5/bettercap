package hid

import (
	"time"

	"github.com/bettercap/bettercap/network"

	"github.com/bettercap/nrf24"
	"github.com/evilsocket/islazy/tui"
)

func (mod *HIDRecon) isSniffing() bool {
	return mod.sniffAddrRaw != nil
}

func (mod *HIDRecon) setSniffMode(mode string) error {
	mod.sniffLock.Lock()
	defer mod.sniffLock.Unlock()

	mod.inSniffMode = false
	if mode == "clear" {
		mod.Debug("restoring recon mode")
		mod.sniffAddrRaw = nil
		mod.sniffAddr = ""
	} else {
		if err, raw := nrf24.ConvertAddress(mode); err != nil {
			return err
		} else {
			mod.Debug("sniffing device %s ...", tui.Bold(mode))
			mod.sniffAddr = network.NormalizeHIDAddress(mode)
			mod.sniffAddrRaw = raw
		}
	}
	return nil
}

func (mod *HIDRecon) doPing() {
	if mod.inSniffMode == false {
		if err := mod.dongle.EnterSnifferModeFor(mod.sniffAddrRaw); err != nil {
			mod.Error("error entering sniffer mode for %s: %v", mod.sniffAddr, err)
		} else {
			mod.inSniffMode = true
			mod.inPromMode = false
			mod.Debug("device entered sniffer mode for %s", mod.sniffAddr)
		}
	}

	if time.Since(mod.lastPing) >= mod.pingPeriod {
		// try on the current channel first
		if err := mod.dongle.TransmitPayload(mod.pingPayload, 250, 1); err != nil {
			for mod.channel = 1; mod.channel <= nrf24.TopChannel; mod.channel++ {
				if err := mod.dongle.SetChannel(mod.channel); err != nil {
					mod.Error("error setting channel %d: %v", mod.channel, err)
				} else if err = mod.dongle.TransmitPayload(mod.pingPayload, 250, 1); err == nil {
					mod.lastPing = time.Now()
					return
				}
			}
		}
	}
}

func (mod *HIDRecon) onSniffedBuffer(buf []byte) {
	if sz := len(buf); sz > 0 && buf[0] == 0x00 {
		buf = buf[1:]
		mod.Debug("sniffed payload %x for %s", buf, mod.sniffAddr)
		if dev, found := mod.Session.HID.Get(mod.sniffAddr); found {
			dev.LastSeen = time.Now()
			dev.AddPayload(buf)
			dev.AddChannel(mod.channel)
		} else {
			mod.Warning("got a payload for unknown device %s", mod.sniffAddr)
		}
	}
}