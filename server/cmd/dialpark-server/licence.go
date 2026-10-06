package main

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"dialpark/server/internal/enroll"
	"dialpark/server/internal/events"
	"dialpark/server/internal/gateway"
	"dialpark/server/internal/licence"
	"dialpark/server/internal/pbxline"
	"dialpark/server/internal/registry"
)

// startLicence reads the installation's identity and its licence (SPEC
// §4.9), before anything that depends on the seat count: the install id at
// <data-dir>/install.id, minted on the first start and kept for ever, and
// the licence at <data-dir>/licence, if one has been installed. A missing or
// unusable licence is not fatal: the server runs with no seats, says so
// once, and takes a licence from the admin API.
func startLicence(log *slog.Logger, dataDir string) (installID string, lic *licence.Manager, err error) {
	installID, err = licence.LoadOrCreateInstallID(filepath.Join(dataDir, "install.id"))
	if err != nil {
		return "", nil, fmt.Errorf("install id: %w", err)
	}
	lic = licence.Open(filepath.Join(dataDir, "licence"), installID, nil, log)
	s := lic.Summary(0)
	log.Info("licence", "state", s.State, "install_id", installID, "customer", s.Customer, "seats", s.Seats, "valid_until", s.ValidUntil, "id", s.ID)
	return installID, lic, nil
}

// licenceGate is the gateway's question after the credential has passed:
// does this device hold a seat, and if not, why not, in the words the
// phone shows its user.
func licenceGate(devices *enroll.Store, lic *licence.Manager) func(deviceID string) (bool, string) {
	return func(deviceID string) (bool, string) {
		if devices.Licensed(deviceID) {
			return true, ""
		}
		switch lic.State() {
		case licence.StateActive, licence.StateExpiring:
			return false, "no licence seat"
		case licence.StateExpired:
			return false, "licence expired"
		}
		return false, "no licence"
	}
}

// seatHooks is what a seat change does to the live server (SPEC §4.9): a
// device that lost its seat has its gateway sessions closed with the
// reason, its SIP binding dropped and, in lines mode, its PBX line
// unregistered; one that gained a seat has its line registered again. The
// registry entry stays either way, so a call to a suspended device is
// answered 480 here, never routed on to the trunk. A call in progress runs
// to its end: in-dialog requests are not challenged, as on revoke.
func seatHooks(log *slog.Logger, ring *events.Ring, reg *registry.Registry, gw *gateway.Gateway, lines *pbxline.Manager, devices *enroll.Store) func(enroll.SeatChange) {
	return func(c enroll.SeatChange) {
		log.Info("licence seats", "used", c.Used, "seats", c.Seats, "lost", c.Lost, "gained", c.Gained)
		for _, id := range c.Lost {
			reason := "no licence seat"
			if c.Seats == 0 {
				reason = "licence expired"
			}
			gw.DisconnectWith(id, reason)
			if user, ok := devices.UserFor(id); ok {
				reg.Unregister(user)
				if lines != nil {
					lines.Delete(user)
				}
			}
			ring.Emit(events.KindSeatLost, id, "", map[string]any{"reason": reason})
		}
		for _, id := range c.Gained {
			registerLine(log, lines, devices, id)
			ring.Emit(events.KindSeatGained, id, "", nil)
		}
	}
}
