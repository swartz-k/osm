package lds

import (
	"context"

	xds "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	envoy_api_v2_core "github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	listener "github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"github.com/golang/protobuf/ptypes"

	"github.com/open-service-mesh/osm/pkg/catalog"
	"github.com/open-service-mesh/osm/pkg/constants"
	"github.com/open-service-mesh/osm/pkg/endpoint"
	"github.com/open-service-mesh/osm/pkg/envoy"
	"github.com/open-service-mesh/osm/pkg/envoy/route"
	"github.com/open-service-mesh/osm/pkg/smi"
)

type empty struct{}

// NewResponse creates a new Listener Discovery Response.
func NewResponse(ctx context.Context, catalog catalog.MeshCataloger, meshSpec smi.MeshSpec, proxy *envoy.Proxy, request *xds.DiscoveryRequest) (*xds.DiscoveryResponse, error) {
	log.Info().Msgf("[%s] Composing listener Discovery Response for proxy: %s", packageName, proxy.GetCommonName())
	proxyServiceName := proxy.GetService()
	resp := &xds.DiscoveryResponse{
		TypeUrl: string(envoy.TypeLDS),
	}

	clientConnManager, err := ptypes.MarshalAny(getHTTPConnectionManager(route.OutboundRouteConfig))
	if err != nil {
		log.Error().Err(err).Msgf("[%s] Could not construct FilterChain", packageName)
		return nil, err
	}

	outboundListenerName := "outbound_listener"
	clientListener := &xds.Listener{
		Name:    outboundListenerName,
		Address: envoy.GetAddress(constants.WildcardIPAddr, constants.EnvoyOutboundListenerPort),
		FilterChains: []*listener.FilterChain{
			{
				Filters: []*listener.Filter{
					{
						Name: wellknown.HTTPConnectionManager,
						ConfigType: &listener.Filter_TypedConfig{
							TypedConfig: clientConnManager,
						},
					},
				},
			},
		},
	}
	log.Info().Msgf("Creating an %s for proxy %s for service %s: %+v", outboundListenerName, proxy.GetCommonName(), proxy.GetService(), clientListener)

	serverConnManager, err := ptypes.MarshalAny(getHTTPConnectionManager(route.InboundRouteConfig))
	if err != nil {
		log.Error().Err(err).Msgf("[%s] Could not construct inbound listener FilterChain", packageName)
		return nil, err
	}

	inboundListenerName := "inbound_listener"
	serverNames, err := getFilterChainMatchServerNames(proxyServiceName, catalog)
	if err != nil {
		log.Error().Err(err).Msgf("[%s] Failed to get client server names for proxy %s", packageName, proxy.GetCommonName())
		return nil, err
	}
	serverListener := &xds.Listener{
		Name:    inboundListenerName,
		Address: envoy.GetAddress(constants.WildcardIPAddr, constants.EnvoyInboundListenerPort),
		FilterChains: []*listener.FilterChain{
			{
				Filters: []*listener.Filter{
					{
						Name: wellknown.HTTPConnectionManager,
						ConfigType: &listener.Filter_TypedConfig{
							TypedConfig: serverConnManager,
						},
					},
				},
				// The FilterChainMatch uses SNI from mTLS to match against the provided list of ServerNames.
				// This ensures only clients authorized to talk to this listener are permitted to.
				FilterChainMatch: &listener.FilterChainMatch{
					ServerNames: serverNames,
				},
				TransportSocket: &envoy_api_v2_core.TransportSocket{
					Name: envoy.TransportSocketTLS,
					ConfigType: &envoy_api_v2_core.TransportSocket_TypedConfig{
						TypedConfig: envoy.GetDownstreamTLSContext(proxyServiceName),
					},
				},
			},
		},
	}
	log.Info().Msgf("Created an %s for proxy %s for service %s: %+v", inboundListenerName, proxy.GetCommonName(), proxy.GetService(), serverListener)

	marshalledOutbound, err := ptypes.MarshalAny(clientListener)
	if err != nil {
		log.Error().Err(err).Msgf("[%s] Failed to marshal outbound listener for proxy %s", packageName, proxy.GetCommonName())
		return nil, err
	}
	resp.Resources = append(resp.Resources, marshalledOutbound)

	marshalledInbound, err := ptypes.MarshalAny(serverListener)
	if err != nil {
		log.Error().Err(err).Msgf("[%s] Failed to marshal inbound listener for proxy %s", packageName, proxy.GetCommonName())
		return nil, err
	}
	resp.Resources = append(resp.Resources, marshalledInbound)
	return resp, nil
}

func getFilterChainMatchServerNames(proxyServiceName endpoint.NamespacedService, catalog catalog.MeshCataloger) ([]string, error) {
	serverNamesMap := make(map[string]interface{})
	var serverNames []string

	allTrafficPolicies, err := catalog.ListTrafficRoutes(proxyServiceName)
	if err != nil {
		log.Error().Err(err).Msgf("[%s] Failed listing traffic routes", packageName)
		return nil, err
	}

	for _, trafficPolicies := range allTrafficPolicies {
		isDestinationService := envoy.Contains(proxyServiceName, trafficPolicies.Destination.Services)
		if isDestinationService {
			for _, source := range trafficPolicies.Source.Services {
				if _, server := serverNamesMap[source.String()]; !server {
					serverNamesMap[source.String()] = nil
					serverNames = append(serverNames, source.String())
				}
			}

		}
	}
	return serverNames, nil
}
