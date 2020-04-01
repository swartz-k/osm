package cds

import (
	"context"

	xds "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/golang/protobuf/ptypes"

	"github.com/open-service-mesh/osm/pkg/catalog"
	"github.com/open-service-mesh/osm/pkg/envoy"

	"github.com/open-service-mesh/osm/pkg/smi"
)

// NewResponse creates a new Cluster Discovery Response.
func NewResponse(ctx context.Context, catalog catalog.MeshCataloger, meshSpec smi.MeshSpec, proxy *envoy.Proxy, request *xds.DiscoveryRequest) (*xds.DiscoveryResponse, error) {
	proxyServiceName := proxy.GetService()
	allTrafficPolicies, err := catalog.ListTrafficRoutes(proxyServiceName)
	if err != nil {
		log.Error().Err(err).Msgf("[%s] Failed listing traffic routes", packageName)
		return nil, err
	}
	log.Debug().Msgf("[%s] TrafficPolicies: %+v for proxy %s", packageName, allTrafficPolicies, proxy.CommonName)
	resp := &xds.DiscoveryResponse{
		TypeUrl: string(envoy.TypeCDS),
	}

	var clusterFactories []xds.Cluster
	for _, trafficPolicies := range allTrafficPolicies {
		isSourceService := envoy.Contains(proxyServiceName, trafficPolicies.Source.Services)
		isDestinationService := envoy.Contains(proxyServiceName, trafficPolicies.Destination.Services)
		if isSourceService {
			for _, cluster := range trafficPolicies.Source.Clusters {
				remoteCluster := envoy.GetServiceCluster(string(cluster.ClusterName), proxyServiceName)
				clusterFactories = append(clusterFactories, remoteCluster)
			}
		} else if isDestinationService {
			for _, cluster := range trafficPolicies.Destination.Clusters {
				clusterFactories = append(clusterFactories, getServiceClusterLocal(catalog, proxyServiceName, string(cluster.ClusterName+envoy.LocalClusterSuffix)))
			}
		}
	}

	clusterFactories = uniques(clusterFactories)
	for _, cluster := range clusterFactories {
		log.Debug().Msgf("[%s] Proxy service %s constructed ClusterConfiguration: %+v ", packageName, proxyServiceName, cluster)
		marshalledClusters, err := ptypes.MarshalAny(&cluster)
		if err != nil {
			log.Error().Err(err).Msgf("[%s] Failed to marshal cluster for proxy %s", packageName, proxy.GetCommonName())
			return nil, err
		}
		resp.Resources = append(resp.Resources, marshalledClusters)
	}
	return resp, nil
}

func uniques(slice []xds.Cluster) []xds.Cluster {
	var isPresent bool
	var clusters []xds.Cluster
	for _, entry := range slice {
		isPresent = false
		for _, cluster := range clusters {
			if cluster.Name == entry.Name {
				isPresent = true
			}
		}
		if !isPresent {
			clusters = append(clusters, entry)
		}
	}
	return clusters
}
