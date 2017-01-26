// Copyright © 2017 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package gateway

import (
	"sync"
	"time"

	pb "github.com/TheThingsNetwork/ttn/api/gateway"
	pb_monitor "github.com/TheThingsNetwork/ttn/api/monitor"
	pb_lorawan "github.com/TheThingsNetwork/ttn/api/protocol/lorawan"
	pb_router "github.com/TheThingsNetwork/ttn/api/router"
	"github.com/apex/log"
)

// NewGateway creates a new in-memory Gateway structure
func NewGateway(ctx log.Interface, id string) *Gateway {
	ctx = ctx.WithField("GatewayID", id)
	gtw := &Gateway{
		ID:          id,
		Status:      NewStatusStore(),
		Utilization: NewUtilization(),
		Schedule:    NewSchedule(ctx),
		Monitors:    pb_monitor.NewRegistry(ctx),
		Ctx:         ctx,
	}
	gtw.Schedule.(*schedule).gateway = gtw // FIXME: Issue #420
	return gtw
}

// Gateway contains the state of a gateway
type Gateway struct {
	ID          string
	Status      StatusStore
	Utilization Utilization
	Schedule    Schedule
	LastSeen    time.Time

	mu            sync.RWMutex // Protect token and authenticated
	token         string
	authenticated bool

	Monitors pb_monitor.Registry

	Ctx log.Interface
}

func (g *Gateway) SetAuth(token string, authenticated bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.authenticated = authenticated
	if token == g.token {
		return
	}
	g.token = token
	g.Monitors.SetGatewayToken(g.ID, g.token)
}

func (g *Gateway) updateLastSeen() {
	g.LastSeen = time.Now()
}

func (g *Gateway) HandleStatus(status *pb.Status) (err error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	status.GatewayTrusted = g.authenticated
	if err = g.Status.Update(status); err != nil {
		return err
	}
	g.updateLastSeen()

	clone := *status // Avoid race conditions
	for _, monitor := range g.Monitors.GatewayClients(g.ID) {
		go monitor.SendStatus(&clone)
	}

	return nil
}

func (g *Gateway) HandleUplink(uplink *pb_router.UplinkMessage) (err error) {
	if err = g.Utilization.AddRx(uplink); err != nil {
		return err
	}
	g.Schedule.Sync(uplink.GatewayMetadata.Timestamp)
	g.updateLastSeen()

	status, err := g.Status.Get()
	if err == nil {
		// Inject Gateway location
		if uplink.GatewayMetadata.Gps == nil {
			uplink.GatewayMetadata.Gps = status.GetGps()
		}
		// Inject Gateway region
		if region, ok := pb_lorawan.Region_value[status.Region]; ok {
			if lorawan := uplink.GetProtocolMetadata().GetLorawan(); lorawan != nil {
				lorawan.Region = pb_lorawan.Region(region)
			}
		}
	}

	// Inject authenticated as GatewayTrusted
	g.mu.RLock()
	defer g.mu.RUnlock()
	uplink.GatewayMetadata.GatewayTrusted = g.authenticated
	uplink.GatewayMetadata.GatewayId = g.ID

	clone := *uplink
	for _, monitor := range g.Monitors.GatewayClients(g.ID) {
		go monitor.SendUplink(&clone)
	}
	return nil
}

func (g *Gateway) HandleDownlink(identifier string, downlink *pb_router.DownlinkMessage) (err error) {
	ctx := g.Ctx.WithField("Identifier", identifier)
	if err = g.Schedule.Schedule(identifier, downlink); err != nil {
		ctx.WithError(err).Warn("Could not schedule downlink")
		return err
	}

	clone := *downlink // Avoid race conditions
	for _, monitor := range g.Monitors.GatewayClients(g.ID) {
		go monitor.SendDownlink(&clone)
	}
	return nil
}
