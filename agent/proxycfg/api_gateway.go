package proxycfg

import (
	"context"
	"fmt"

	cachetype "github.com/hashicorp/consul/agent/cache-types"
	"github.com/hashicorp/consul/agent/proxycfg/internal/watch"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/proto/pbpeering"
)

var _ kindHandler = (*handlerAPIGateway)(nil)

// handlerAPIGateway generates a new ConfigSnapshot in response to
// changes related to an api-gateway.
type handlerAPIGateway struct {
	handlerState
}

// initialize sets up the initial watches needed based on the api-gateway registration
func (h *handlerAPIGateway) initialize(ctx context.Context) (ConfigSnapshot, error) {
	snap := newConfigSnapshotFromServiceInstance(h.serviceInstance, h.stateConfig)

	// Watch for root changes
	err := h.dataSources.CARoots.Notify(ctx, &structs.DCSpecificRequest{
		Datacenter:   h.source.Datacenter,
		QueryOptions: structs.QueryOptions{Token: h.token},
		Source:       *h.source,
	}, rootsWatchID, h.ch)
	if err != nil {
		return snap, err
	}

	// Get information about the entire service mesh.
	err = h.dataSources.ConfigEntry.Notify(ctx, &structs.ConfigEntryQuery{
		Kind:           structs.MeshConfig,
		Name:           structs.MeshConfigMesh,
		Datacenter:     h.source.Datacenter,
		QueryOptions:   structs.QueryOptions{Token: h.token},
		EnterpriseMeta: *structs.DefaultEnterpriseMetaInPartition(h.proxyID.PartitionOrDefault()),
	}, meshConfigEntryID, h.ch)
	if err != nil {
		return snap, err
	}

	// Watch the api-gateway's config entry
	err = h.subscribeToConfigEntry(ctx, structs.APIGateway, h.service, gatewayConfigWatchID)
	if err != nil {
		return snap, err
	}

	// Watch the bound-api-gateway's config entry
	err = h.subscribeToConfigEntry(ctx, structs.BoundAPIGateway, h.service, gatewayConfigWatchID)
	if err != nil {
		return snap, err
	}

	snap.APIGateway.Listeners = make(map[string]structs.APIGatewayListener)
	snap.APIGateway.BoundListeners = make(map[string]structs.BoundAPIGatewayListener)
	snap.APIGateway.HTTPRoutes = watch.NewMap[structs.ResourceReference, *structs.HTTPRouteConfigEntry]()
	snap.APIGateway.TCPRoutes = watch.NewMap[structs.ResourceReference, *structs.TCPRouteConfigEntry]()
	snap.APIGateway.Certificates = watch.NewMap[structs.ResourceReference, *structs.InlineCertificateConfigEntry]()

	snap.APIGateway.Upstreams = make(listenerRouteUpstreams)
	snap.APIGateway.UpstreamsSet = make(routeUpstreamSet)

	// These need to be initialized here but are set by handlerUpstreams
	snap.APIGateway.DiscoveryChain = make(map[UpstreamID]*structs.CompiledDiscoveryChain)
	snap.APIGateway.PeerUpstreamEndpoints = watch.NewMap[UpstreamID, structs.CheckServiceNodes]()
	snap.APIGateway.PeerUpstreamEndpointsUseHostnames = make(map[UpstreamID]struct{})
	snap.APIGateway.UpstreamPeerTrustBundles = watch.NewMap[string, *pbpeering.PeeringTrustBundle]()
	snap.APIGateway.WatchedDiscoveryChains = make(map[UpstreamID]context.CancelFunc)
	snap.APIGateway.WatchedGateways = make(map[UpstreamID]map[string]context.CancelFunc)
	snap.APIGateway.WatchedGatewayEndpoints = make(map[UpstreamID]map[string]structs.CheckServiceNodes)
	snap.APIGateway.WatchedUpstreams = make(map[UpstreamID]map[string]context.CancelFunc)
	snap.APIGateway.WatchedUpstreamEndpoints = make(map[UpstreamID]map[string]structs.CheckServiceNodes)

	return snap, nil
}

func (h *handlerAPIGateway) subscribeToConfigEntry(ctx context.Context, kind, name, watchID string) error {
	return h.dataSources.ConfigEntry.Notify(ctx, &structs.ConfigEntryQuery{
		Kind:           kind,
		Name:           name,
		Datacenter:     h.source.Datacenter,
		QueryOptions:   structs.QueryOptions{Token: h.token},
		EnterpriseMeta: h.proxyID.EnterpriseMeta,
	}, watchID, h.ch)
}

// handleUpdate responds to changes in the api-gateway. In general, we want
// to crawl the various resources related to or attached to the gateway and
// collect the list of things need to generate xDS.  This list of resources
// includes the bound-api-gateway, http-routes, tcp-routes, and inline-certificates.
func (h *handlerAPIGateway) handleUpdate(ctx context.Context, u UpdateEvent, snap *ConfigSnapshot) error {
	if u.Err != nil {
		return fmt.Errorf("error filling agent cache: %v", u.Err)
	}

	switch {
	case u.CorrelationID == rootsWatchID:
		// Handle change in the CA roots
		if err := h.handleRootCAUpdate(u, snap); err != nil {
			return err
		}
	case u.CorrelationID == gatewayConfigWatchID:
		// Handle change in the api-gateway or bound-api-gateway config entry
		if err := h.handleGatewayConfigUpdate(ctx, u, snap); err != nil {
			return err
		}
	case u.CorrelationID == inlineCertificateConfigWatchID:
		// Handle change in an attached inline-certificate config entry
		if err := h.handleInlineCertConfigUpdate(ctx, u, snap); err != nil {
			return err
		}
	case u.CorrelationID == routeConfigWatchID:
		// Handle change in an attached http-route or tcp-route config entry
		if err := h.handleRouteConfigUpdate(ctx, u, snap); err != nil {
			return err
		}
	default:
		return (*handlerUpstreams)(h).handleUpdateUpstreams(ctx, u, snap)
	}

	return nil
}

// handleRootCAUpdate responds to changes in the watched root CA for a gateway
func (h *handlerAPIGateway) handleRootCAUpdate(u UpdateEvent, snap *ConfigSnapshot) error {
	roots, ok := u.Result.(*structs.IndexedCARoots)
	if !ok {
		return fmt.Errorf("invalid type for response: %T", u.Result)
	}
	snap.Roots = roots
	return nil
}

// handleGatewayConfigUpdate responds to changes in the watched config entry for a gateway.
// In particular, we want to make sure that we're subscribing to any attached resources such
// as routes and certificates. These additional subscriptions will enable us to update the
// config snapshot appropriately for any route or certificate changes.
func (h *handlerAPIGateway) handleGatewayConfigUpdate(ctx context.Context, u UpdateEvent, snap *ConfigSnapshot) error {
	resp, ok := u.Result.(*structs.ConfigEntryResponse)
	if !ok {
		return fmt.Errorf("invalid type for response: %T", u.Result)
	} else if resp.Entry == nil {
		return nil
	}

	switch gwConf := resp.Entry.(type) {
	case *structs.BoundAPIGatewayConfigEntry:
		snap.APIGateway.BoundGatewayConfig = gwConf

		seenRefs := make(map[structs.ResourceReference]any)
		for _, listener := range gwConf.Listeners {
			snap.APIGateway.BoundListeners[listener.Name] = listener

			// Subscribe to changes in each attached x-route config entry
			for _, ref := range listener.Routes {
				seenRefs[ref] = struct{}{}

				ctx, cancel := context.WithCancel(ctx)
				switch ref.Kind {
				case structs.HTTPRoute:
					snap.APIGateway.HTTPRoutes.InitWatch(ref, cancel)
				case structs.TCPRoute:
					snap.APIGateway.TCPRoutes.InitWatch(ref, cancel)
				default:
					cancel()
					return fmt.Errorf("unexpected route kind on gateway: %s", ref.Kind)
				}

				err := h.subscribeToConfigEntry(ctx, ref.Kind, ref.Name, routeConfigWatchID)
				if err != nil {
					// TODO May want to continue
					return err
				}
			}

			// Subscribe to changes in each attached inline-certificate config entry
			for _, ref := range listener.Certificates {
				ctx, cancel := context.WithCancel(ctx)
				seenRefs[ref] = struct{}{}
				snap.APIGateway.Certificates.InitWatch(ref, cancel)

				err := h.subscribeToConfigEntry(ctx, ref.Kind, ref.Name, inlineCertificateConfigWatchID)
				if err != nil {
					// TODO May want to continue
					return err
				}
			}
		}

		// Unsubscribe from any config entries that are no longer attached
		snap.APIGateway.HTTPRoutes.ForEachKey(func(ref structs.ResourceReference) bool {
			if _, ok := seenRefs[ref]; !ok {
				snap.APIGateway.Upstreams.delete(ref)
				snap.APIGateway.UpstreamsSet.delete(ref)
				snap.APIGateway.HTTPRoutes.CancelWatch(ref)
			}
			return true
		})

		snap.APIGateway.TCPRoutes.ForEachKey(func(ref structs.ResourceReference) bool {
			if _, ok := seenRefs[ref]; !ok {
				snap.APIGateway.Upstreams.delete(ref)
				snap.APIGateway.UpstreamsSet.delete(ref)
				snap.APIGateway.TCPRoutes.CancelWatch(ref)
			}
			return true
		})

		snap.APIGateway.Certificates.ForEachKey(func(ref structs.ResourceReference) bool {
			if _, ok := seenRefs[ref]; !ok {
				snap.APIGateway.Certificates.CancelWatch(ref)
			}
			return true
		})

		snap.APIGateway.BoundGatewayConfigLoaded = true
		break
	case *structs.APIGatewayConfigEntry:
		snap.APIGateway.GatewayConfig = gwConf

		for _, listener := range gwConf.Listeners {
			snap.APIGateway.Listeners[listener.Name] = listener
		}

		snap.APIGateway.GatewayConfigLoaded = true
		break
	default:
		return fmt.Errorf("invalid type for config entry: %T", resp.Entry)
	}

	return h.watchIngressLeafCert(ctx, snap)
}

// handleInlineCertConfigUpdate stores the certificate for the gateway
func (h *handlerAPIGateway) handleInlineCertConfigUpdate(_ context.Context, u UpdateEvent, snap *ConfigSnapshot) error {
	resp, ok := u.Result.(*structs.ConfigEntryResponse)
	if !ok {
		return fmt.Errorf("invalid type for response: %T", u.Result)
	} else if resp.Entry == nil {
		return nil
	}

	cfg, ok := resp.Entry.(*structs.InlineCertificateConfigEntry)
	if !ok {
		return fmt.Errorf("invalid type for config entry: %T", resp.Entry)
	}

	ref := structs.ResourceReference{
		Kind:           cfg.GetKind(),
		Name:           cfg.GetName(),
		EnterpriseMeta: *cfg.GetEnterpriseMeta(),
	}

	snap.APIGateway.Certificates.Set(ref, cfg)

	return nil
}

// handleRouteConfigUpdate builds the list of upstreams for services on
// the route and watches the related discovery chains.
func (h *handlerAPIGateway) handleRouteConfigUpdate(ctx context.Context, u UpdateEvent, snap *ConfigSnapshot) error {
	resp, ok := u.Result.(*structs.ConfigEntryResponse)
	if !ok {
		return fmt.Errorf("invalid type for response: %T", u.Result)
	} else if resp.Entry == nil {
		return nil
	}

	ref := structs.ResourceReference{
		Kind:           resp.Entry.GetKind(),
		Name:           resp.Entry.GetName(),
		EnterpriseMeta: *resp.Entry.GetEnterpriseMeta(),
	}

	seenUpstreamIDs := make(upstreamIDSet)
	upstreams := make(map[APIGatewayListenerKey]structs.Upstreams)

	switch route := resp.Entry.(type) {
	case *structs.HTTPRouteConfigEntry:
		snap.APIGateway.HTTPRoutes.Set(ref, route)

		for _, rule := range route.Rules {
			for _, service := range rule.Services {
				for _, listener := range snap.APIGateway.Listeners {
					shouldBind := false
					for _, parent := range route.Parents {
						if h.referenceIsForListener(parent, listener, snap) {
							shouldBind = true
							break
						}
					}
					if !shouldBind {
						continue
					}

					upstream := structs.Upstream{
						DestinationName:      service.Name,
						DestinationNamespace: service.NamespaceOrDefault(),
						DestinationPartition: service.PartitionOrDefault(),
						LocalBindPort:        listener.Port,
						// TODO IngressHosts:         g.Hosts,
						// Pass the protocol that was configured on the listener in order
						// to force that protocol on the Envoy listener.
						Config: map[string]interface{}{
							"protocol": "http",
						},
					}

					listenerKey := APIGatewayListenerKey{Protocol: string(listener.Protocol), Port: listener.Port}
					upstreams[listenerKey] = append(upstreams[listenerKey], upstream)
				}

				upstreamID := NewUpstreamIDFromServiceName(service.ServiceName())
				seenUpstreamIDs[upstreamID] = struct{}{}

				watchOpts := discoveryChainWatchOpts{
					id:         upstreamID,
					name:       service.Name,
					namespace:  service.NamespaceOrDefault(),
					partition:  service.PartitionOrDefault(),
					datacenter: h.stateConfig.source.Datacenter,
				}

				handler := &handlerUpstreams{handlerState: h.handlerState}
				if err := handler.watchDiscoveryChain(ctx, snap, watchOpts); err != nil {
					return fmt.Errorf("failed to watch discovery chain for %s: %w", upstreamID, err)
				}
			}
		}

	case *structs.TCPRouteConfigEntry:
		snap.APIGateway.TCPRoutes.Set(ref, route)

		for _, service := range route.Services {
			upstreamID := NewUpstreamIDFromServiceName(service.ServiceName())
			seenUpstreamIDs.add(upstreamID)

			// For each listener, check if this route should bind and, if so, create an upstream.
			for _, listener := range snap.APIGateway.Listeners {
				shouldBind := false
				for _, parent := range route.Parents {
					if h.referenceIsForListener(parent, listener, snap) {
						shouldBind = true
						break
					}
				}
				if !shouldBind {
					continue
				}

				upstream := structs.Upstream{
					DestinationName:      service.Name,
					DestinationNamespace: service.NamespaceOrDefault(),
					DestinationPartition: service.PartitionOrDefault(),
					LocalBindPort:        listener.Port,
					// Pass the protocol that was configured on the ingress listener in order
					// to force that protocol on the Envoy listener.
					Config: map[string]interface{}{
						"protocol": "tcp",
					},
				}

				listenerKey := APIGatewayListenerKey{Protocol: string(listener.Protocol), Port: listener.Port}
				upstreams[listenerKey] = append(upstreams[listenerKey], upstream)
			}

			watchOpts := discoveryChainWatchOpts{
				id:         upstreamID,
				name:       service.Name,
				namespace:  service.NamespaceOrDefault(),
				partition:  service.PartitionOrDefault(),
				datacenter: h.stateConfig.source.Datacenter,
			}

			handler := &handlerUpstreams{handlerState: h.handlerState}
			if err := handler.watchDiscoveryChain(ctx, snap, watchOpts); err != nil {
				return fmt.Errorf("failed to watch discovery chain for %s: %w", upstreamID, err)
			}
		}
	default:
		return fmt.Errorf("invalid type for config entry: %T", resp.Entry)
	}

	for listener, set := range upstreams {
		snap.APIGateway.Upstreams.set(ref, listener, set)
	}
	snap.APIGateway.UpstreamsSet.set(ref, seenUpstreamIDs)
	//snap.APIGateway.Hosts = TODO
	snap.APIGateway.AreHostsSet = true

	// Stop watching any upstreams and discovery chains that have become irrelevant
	for upstreamID, cancelDiscoChain := range snap.APIGateway.WatchedDiscoveryChains {
		if snap.APIGateway.UpstreamsSet.hasUpstream(upstreamID) {
			continue
		}

		for targetID, cancelUpstream := range snap.APIGateway.WatchedUpstreams[upstreamID] {
			cancelUpstream()
			delete(snap.APIGateway.WatchedUpstreams[upstreamID], targetID)
			delete(snap.APIGateway.WatchedUpstreamEndpoints[upstreamID], targetID)

			if targetUID := NewUpstreamIDFromTargetID(targetID); targetUID.Peer != "" {
				snap.APIGateway.PeerUpstreamEndpoints.CancelWatch(targetUID)
				snap.APIGateway.UpstreamPeerTrustBundles.CancelWatch(targetUID.Peer)
			}
		}

		cancelDiscoChain()
		delete(snap.APIGateway.WatchedDiscoveryChains, upstreamID)
	}

	return nil
}

// referenceIsForListener returns whether the provided structs.ResourceReference
// targets the provided structs.APIGatewayListener. For this to be true, the kind
// and name must match the structs.APIGatewayConfigEntry containing the listener,
// and the reference must specify either no section name or the name of the listener
// as the section name.
//
// TODO This would probably be more generally useful as a helper in the structs pkg
func (h *handlerAPIGateway) referenceIsForListener(ref structs.ResourceReference, listener structs.APIGatewayListener, snap *ConfigSnapshot) bool {
	if ref.Kind != structs.APIGateway && ref.Kind != "" {
		return false
	}
	if ref.Name != snap.APIGateway.GatewayConfig.Name {
		return false
	}
	return ref.SectionName == "" || ref.SectionName == listener.Name
}

func (h *handlerAPIGateway) watchIngressLeafCert(ctx context.Context, snap *ConfigSnapshot) error {
	// Note that we DON'T test for TLS.enabled because we need a leaf cert for the
	// gateway even without TLS to use as a client cert.
	if !snap.APIGateway.GatewayConfigLoaded {
		return nil
	}

	// Watch the leaf cert
	if snap.APIGateway.LeafCertWatchCancel != nil {
		snap.APIGateway.LeafCertWatchCancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	err := h.dataSources.LeafCertificate.Notify(ctx, &cachetype.ConnectCALeafRequest{
		Datacenter:     h.source.Datacenter,
		Token:          h.token,
		Service:        h.service,
		EnterpriseMeta: h.proxyID.EnterpriseMeta,
	}, leafWatchID, h.ch)
	if err != nil {
		cancel()
		return err
	}
	snap.APIGateway.LeafCertWatchCancel = cancel

	return nil
}
