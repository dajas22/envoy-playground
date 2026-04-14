package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	clusterv3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpointv3 "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	cdsv3 "github.com/envoyproxy/go-control-plane/envoy/service/cluster/v3"
	discoveryv3 "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"
	edsv3 "github.com/envoyproxy/go-control-plane/envoy/service/endpoint/v3"
	"google.golang.org/grpc"
	grpcHealth "google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"gopkg.in/yaml.v3"
)

const (
	port          = ":5000"
	edsTypeURL    = "type.googleapis.com/envoy.config.endpoint.v3.ClusterLoadAssignment"
	cdsTypeURL    = "type.googleapis.com/envoy.config.cluster.v3.Cluster"
	xdsCluster    = "xds_cluster"
	defaultConfig = "endpoints.yaml"
)

var watchInterval = 1 * time.Second

type endpointFileConfig struct {
	// Preferred format: multiple services under services[].
	Services []serviceConfig `yaml:"services"`

	// Backward compatibility for old single-service top-level format.
	ClusterName     string                `yaml:"cluster_name"`
	ClusterLBPolicy string                `yaml:"cluster_lb_policy"`
	ClusterType     string                `yaml:"cluster_type"`
	EndpointGroups  []endpointGroupConfig `yaml:"endpoint_groups"`
}

type serviceConfig struct {
	ClusterName     string                `yaml:"cluster_name"`
	ClusterLBPolicy string                `yaml:"cluster_lb_policy"`
	ClusterType     string                `yaml:"cluster_type"`
	EndpointGroups  []endpointGroupConfig `yaml:"endpoint_groups"`
}

type endpointGroupConfig struct {
	Priority  uint32           `yaml:"priority"`
	Locality  endpointLocality `yaml:"locality"`
	Endpoints []hostConfig     `yaml:"endpoints"`
}

type endpointLocality struct {
	Region  string `yaml:"region"`
	Zone    string `yaml:"zone"`
	SubZone string `yaml:"sub_zone"`
}

type hostConfig struct {
	Address string `yaml:"address"`
	Port    uint32 `yaml:"port"`
}

type serviceState struct {
	assignment      *endpointv3.ClusterLoadAssignment
	clusterLBPolicy clusterv3.Cluster_LbPolicy
	clusterType     clusterv3.Cluster_DiscoveryType
}

type ConfigStore struct {
	mu          sync.RWMutex
	services    map[string]serviceState
	version     uint64
	subscribers map[chan struct{}]struct{}
}

func NewConfigStore(services map[string]serviceState) *ConfigStore {
	return &ConfigStore{
		services:    services,
		version:     1,
		subscribers: make(map[chan struct{}]struct{}),
	}
}

func (s *ConfigStore) Subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

func (s *ConfigStore) Unsubscribe(ch chan struct{}) {
	s.mu.Lock()
	delete(s.subscribers, ch)
	s.mu.Unlock()
	close(ch)
}

func (s *ConfigStore) ServiceNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.services))
	for name := range s.services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *ConfigStore) Snapshot(resourceNames []string) (string, string, []*anypb.Any, error) {
	s.mu.RLock()
	version := s.version
	services := copyServiceMap(s.services)
	s.mu.RUnlock()

	versionInfo := strconv.FormatUint(version, 10)
	resources := make([]*anypb.Any, 0, len(services))

	names := sortedServiceNames(services)
	for _, name := range names {
		svc := services[name]
		if svc.assignment == nil {
			continue
		}
		if !isResourceRequested(resourceNames, name) {
			continue
		}

		anyAssignment, err := anypb.New(svc.assignment)
		if err != nil {
			return "", "", nil, err
		}
		resources = append(resources, anyAssignment)
	}

	return versionInfo, versionInfo, resources, nil
}

func (s *ConfigStore) ClusterSnapshot(resourceNames []string) (string, string, []*anypb.Any, error) {
	s.mu.RLock()
	version := s.version
	services := copyServiceMap(s.services)
	s.mu.RUnlock()

	versionInfo := strconv.FormatUint(version, 10)
	resources := make([]*anypb.Any, 0, len(services))

	names := sortedServiceNames(services)
	for _, name := range names {
		svc := services[name]
		if svc.assignment == nil {
			continue
		}
		if !isResourceRequested(resourceNames, name) {
			continue
		}

		cluster := buildBackendCluster(name, svc.clusterLBPolicy, svc.clusterType)
		anyCluster, err := anypb.New(cluster)
		if err != nil {
			return "", "", nil, err
		}
		resources = append(resources, anyCluster)
	}

	return versionInfo, versionInfo, resources, nil
}

func (s *ConfigStore) DeltaEndpointResources(includeAll bool, subscribed map[string]struct{}) (string, []*discoveryv3.Resource, error) {
	s.mu.RLock()
	version := s.version
	services := copyServiceMap(s.services)
	s.mu.RUnlock()

	versionStr := strconv.FormatUint(version, 10)
	names := targetNames(services, includeAll, subscribed)
	resources := make([]*discoveryv3.Resource, 0, len(names))

	for _, name := range names {
		svc := services[name]
		if svc.assignment == nil {
			continue
		}
		anyAssignment, err := anypb.New(svc.assignment)
		if err != nil {
			return "", nil, err
		}
		resources = append(resources, &discoveryv3.Resource{
			Name:     name,
			Version:  versionStr,
			Resource: anyAssignment,
		})
	}

	return versionStr, resources, nil
}

func (s *ConfigStore) DeltaClusterResources(includeAll bool, subscribed map[string]struct{}) (string, []*discoveryv3.Resource, error) {
	s.mu.RLock()
	version := s.version
	services := copyServiceMap(s.services)
	s.mu.RUnlock()

	versionStr := strconv.FormatUint(version, 10)
	names := targetNames(services, includeAll, subscribed)
	resources := make([]*discoveryv3.Resource, 0, len(names))

	for _, name := range names {
		svc := services[name]
		if svc.assignment == nil {
			continue
		}
		cluster := buildBackendCluster(name, svc.clusterLBPolicy, svc.clusterType)
		anyCluster, err := anypb.New(cluster)
		if err != nil {
			return "", nil, err
		}
		resources = append(resources, &discoveryv3.Resource{
			Name:     name,
			Version:  versionStr,
			Resource: anyCluster,
		})
	}

	return versionStr, resources, nil
}

func (s *ConfigStore) Update(services map[string]serviceState) string {
	s.mu.Lock()
	s.services = services
	s.version++
	versionInfo := strconv.FormatUint(s.version, 10)
	subscribers := make([]chan struct{}, 0, len(s.subscribers))
	for ch := range s.subscribers {
		subscribers = append(subscribers, ch)
	}
	s.mu.Unlock()

	for _, ch := range subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}

	return versionInfo
}

// EDSServer implements the endpoint and cluster discovery service.
type EDSServer struct {
	edsv3.UnimplementedEndpointDiscoveryServiceServer
	cdsv3.UnimplementedClusterDiscoveryServiceServer
	store *ConfigStore
}

func (s *EDSServer) StreamEndpoints(stream edsv3.EndpointDiscoveryService_StreamEndpointsServer) error {
	ctx := stream.Context()
	log.Println("EDS client connected")
	updates := s.store.Subscribe()
	defer s.store.Unsubscribe(updates)

	reqCh := make(chan *discoveryv3.DiscoveryRequest)
	errCh := make(chan error, 1)

	go func() {
		defer close(reqCh)
		for {
			req, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}

			select {
			case reqCh <- req:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
	}()

	resourceNames := []string{}

	sendSnapshot := func(reason string) error {
		version, nonce, resources, err := s.store.Snapshot(resourceNames)
		if err != nil {
			return err
		}

		resp := &discoveryv3.DiscoveryResponse{
			VersionInfo: version,
			Resources:   resources,
			TypeUrl:     edsTypeURL,
			Nonce:       nonce,
		}

		if err := stream.Send(resp); err != nil {
			return err
		}

		log.Printf("Sent EDS response (%s): version=%s resources=%d\n", reason, version, len(resources))
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("EDS client disconnected")
			return ctx.Err()
		case err := <-errCh:
			if errors.Is(err, io.EOF) {
				log.Println("EDS stream closed by client")
				return nil
			}
			if errors.Is(err, context.Canceled) {
				log.Println("EDS stream canceled")
				return nil
			}
			log.Printf("Error receiving EDS request: %v\n", err)
			return err
		case req, ok := <-reqCh:
			if !ok {
				continue
			}

			if len(req.ResourceNames) > 0 {
				resourceNames = req.ResourceNames
			}

			log.Printf("Received EDS request: VersionInfo=%s, ResourceNames=%v\n", req.VersionInfo, req.ResourceNames)
			if req.ErrorDetail != nil {
				log.Printf("EDS client reported error: %s\n", req.ErrorDetail.Message)
			}

			if err := sendSnapshot("request"); err != nil {
				log.Printf("Error sending EDS response: %v\n", err)
				return err
			}
		case <-updates:
			if len(resourceNames) == 0 {
				continue
			}

			if err := sendSnapshot("config update"); err != nil {
				log.Printf("Error sending EDS update: %v\n", err)
				return err
			}
		}
	}
}

func (s *EDSServer) DeltaEndpoints(stream edsv3.EndpointDiscoveryService_DeltaEndpointsServer) error {
	ctx := stream.Context()
	log.Println("Delta EDS client connected")
	updates := s.store.Subscribe()
	defer s.store.Unsubscribe(updates)

	reqCh := make(chan *discoveryv3.DeltaDiscoveryRequest)
	errCh := make(chan error, 1)

	go func() {
		defer close(reqCh)
		for {
			req, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}
			select {
			case reqCh <- req:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
	}()

	subscribed := map[string]struct{}{}
	subscribedAll := false
	clientVersions := map[string]string{}

	sendDelta := func(reason string) error {
		sysVersion, resources, err := s.store.DeltaEndpointResources(subscribedAll, subscribed)
		if err != nil {
			return err
		}

		out := make([]*discoveryv3.Resource, 0, len(resources))
		for _, res := range resources {
			if clientVersions[res.Name] != res.Version {
				out = append(out, res)
			}
		}
		if len(out) == 0 {
			return nil
		}

		resp := &discoveryv3.DeltaDiscoveryResponse{
			SystemVersionInfo: sysVersion,
			Resources:         out,
			TypeUrl:           edsTypeURL,
			Nonce:             sysVersion,
		}

		if err := stream.Send(resp); err != nil {
			return err
		}

		log.Printf("Sent delta EDS (%s): system_version=%s resources=%d\n", reason, sysVersion, len(out))
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Delta EDS client disconnected")
			return ctx.Err()
		case err := <-errCh:
			if errors.Is(err, io.EOF) {
				log.Println("Delta EDS stream closed by client")
				return nil
			}
			if errors.Is(err, context.Canceled) {
				log.Println("Delta EDS stream canceled")
				return nil
			}
			log.Printf("Error receiving delta EDS request: %v\n", err)
			return err
		case req, ok := <-reqCh:
			if !ok {
				continue
			}

			for name, ver := range req.InitialResourceVersions {
				clientVersions[name] = ver
			}
			if len(req.ResourceNamesSubscribe) == 0 && len(req.ResourceNamesUnsubscribe) == 0 && len(req.InitialResourceVersions) == 0 && len(subscribed) == 0 {
				subscribedAll = true
			}
			for _, name := range req.ResourceNamesSubscribe {
				subscribed[name] = struct{}{}
				subscribedAll = false
			}
			for _, name := range req.ResourceNamesUnsubscribe {
				if subscribedAll {
					subscribed[name] = struct{}{}
				}
				delete(subscribed, name)
				delete(clientVersions, name)
			}

			log.Printf("Delta EDS request: subscribe=%v unsubscribe=%v nonce=%q\n",
				req.ResourceNamesSubscribe, req.ResourceNamesUnsubscribe, req.ResponseNonce)

			if req.ErrorDetail != nil {
				log.Printf("Delta EDS NACK (nonce=%q): %s\n", req.ResponseNonce, req.ErrorDetail.Message)
				if subscribedAll {
					for _, name := range s.store.ServiceNames() {
						delete(clientVersions, name)
					}
				} else {
					for name := range subscribed {
						delete(clientVersions, name)
					}
				}
				if err := sendDelta("nack-retry"); err != nil {
					return err
				}
				continue
			}

			if req.ResponseNonce != "" {
				if subscribedAll {
					for _, name := range s.store.ServiceNames() {
						clientVersions[name] = req.ResponseNonce
					}
				} else {
					for name := range subscribed {
						clientVersions[name] = req.ResponseNonce
					}
				}
			}

			if err := sendDelta("request"); err != nil {
				log.Printf("Error sending delta EDS response: %v\n", err)
				return err
			}
		case <-updates:
			if !subscribedAll && len(subscribed) == 0 {
				continue
			}
			if err := sendDelta("config update"); err != nil {
				log.Printf("Error sending delta EDS update: %v\n", err)
				return err
			}
		}
	}
}

func (s *EDSServer) StreamClusters(stream cdsv3.ClusterDiscoveryService_StreamClustersServer) error {
	ctx := stream.Context()
	log.Println("CDS client connected (streaming mode)")
	updates := s.store.Subscribe()
	defer s.store.Unsubscribe(updates)

	reqCh := make(chan *discoveryv3.DiscoveryRequest)
	errCh := make(chan error, 1)

	go func() {
		defer close(reqCh)
		for {
			req, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}

			select {
			case reqCh <- req:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
	}()

	resourceNames := []string{}

	sendSnapshot := func(reason string) error {
		version, nonce, resources, err := s.store.ClusterSnapshot(resourceNames)
		if err != nil {
			return err
		}

		resp := &discoveryv3.DiscoveryResponse{
			VersionInfo: version,
			Resources:   resources,
			TypeUrl:     cdsTypeURL,
			Nonce:       nonce,
		}

		if err := stream.Send(resp); err != nil {
			return err
		}

		log.Printf("Sent CDS response (%s): version=%s resources=%d\n", reason, version, len(resources))
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("CDS client disconnected")
			return ctx.Err()
		case err := <-errCh:
			if errors.Is(err, io.EOF) {
				log.Println("CDS stream closed by client")
				return nil
			}
			if errors.Is(err, context.Canceled) {
				log.Println("CDS stream canceled")
				return nil
			}
			log.Printf("Error receiving CDS request: %v\n", err)
			return err
		case req, ok := <-reqCh:
			if !ok {
				continue
			}

			if len(req.ResourceNames) > 0 {
				resourceNames = req.ResourceNames
			}

			log.Printf("Received CDS request: VersionInfo=%s, ResourceNames=%v\n", req.VersionInfo, req.ResourceNames)
			if req.ErrorDetail != nil {
				log.Printf("CDS client reported error: %s\n", req.ErrorDetail.Message)
			}

			if err := sendSnapshot("request"); err != nil {
				log.Printf("Error sending CDS response: %v\n", err)
				return err
			}
		case <-updates:
			if len(resourceNames) == 0 {
				continue
			}

			if err := sendSnapshot("config update"); err != nil {
				log.Printf("Error sending CDS update: %v\n", err)
				return err
			}
		}
	}
}

func (s *EDSServer) DeltaClusters(stream cdsv3.ClusterDiscoveryService_DeltaClustersServer) error {
	ctx := stream.Context()
	log.Println("Delta CDS client connected")
	updates := s.store.Subscribe()
	defer s.store.Unsubscribe(updates)

	reqCh := make(chan *discoveryv3.DeltaDiscoveryRequest)
	errCh := make(chan error, 1)

	go func() {
		defer close(reqCh)
		for {
			req, err := stream.Recv()
			if err != nil {
				errCh <- err
				return
			}
			select {
			case reqCh <- req:
			case <-ctx.Done():
				errCh <- ctx.Err()
				return
			}
		}
	}()

	subscribed := map[string]struct{}{}
	subscribedAll := false
	clientVersions := map[string]string{}

	sendDelta := func(reason string) error {
		sysVersion, resources, err := s.store.DeltaClusterResources(subscribedAll, subscribed)
		if err != nil {
			return err
		}

		out := make([]*discoveryv3.Resource, 0, len(resources))
		for _, res := range resources {
			if clientVersions[res.Name] != res.Version {
				out = append(out, res)
			}
		}
		if len(out) == 0 {
			return nil
		}

		resp := &discoveryv3.DeltaDiscoveryResponse{
			SystemVersionInfo: sysVersion,
			Resources:         out,
			TypeUrl:           cdsTypeURL,
			Nonce:             sysVersion,
		}
		if err := stream.Send(resp); err != nil {
			return err
		}

		log.Printf("Sent delta CDS (%s): system_version=%s resources=%d\n", reason, sysVersion, len(out))
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("Delta CDS client disconnected")
			return ctx.Err()
		case err := <-errCh:
			if errors.Is(err, io.EOF) {
				log.Println("Delta CDS stream closed by client")
				return nil
			}
			if errors.Is(err, context.Canceled) {
				log.Println("Delta CDS stream canceled")
				return nil
			}
			log.Printf("Error receiving delta CDS request: %v\n", err)
			return err
		case req, ok := <-reqCh:
			if !ok {
				continue
			}

			for name, ver := range req.InitialResourceVersions {
				clientVersions[name] = ver
			}
			if len(req.ResourceNamesSubscribe) == 0 && len(req.ResourceNamesUnsubscribe) == 0 && len(req.InitialResourceVersions) == 0 && len(subscribed) == 0 {
				subscribedAll = true
			}
			for _, name := range req.ResourceNamesSubscribe {
				subscribed[name] = struct{}{}
				subscribedAll = false
			}
			for _, name := range req.ResourceNamesUnsubscribe {
				if subscribedAll {
					subscribed[name] = struct{}{}
				}
				delete(subscribed, name)
				delete(clientVersions, name)
			}

			log.Printf("Delta CDS request: subscribe=%v unsubscribe=%v nonce=%q\n",
				req.ResourceNamesSubscribe, req.ResourceNamesUnsubscribe, req.ResponseNonce)

			if req.ErrorDetail != nil {
				log.Printf("Delta CDS NACK (nonce=%q): %s\n", req.ResponseNonce, req.ErrorDetail.Message)
				if subscribedAll {
					for _, name := range s.store.ServiceNames() {
						delete(clientVersions, name)
					}
				} else {
					for name := range subscribed {
						delete(clientVersions, name)
					}
				}
				if err := sendDelta("nack-retry"); err != nil {
					return err
				}
				continue
			}

			if req.ResponseNonce != "" {
				if subscribedAll {
					for _, name := range s.store.ServiceNames() {
						clientVersions[name] = req.ResponseNonce
					}
				} else {
					for name := range subscribed {
						clientVersions[name] = req.ResponseNonce
					}
				}
			}

			if err := sendDelta("request"); err != nil {
				log.Printf("Error sending delta CDS response: %v\n", err)
				return err
			}
		case <-updates:
			if !subscribedAll && len(subscribed) == 0 {
				continue
			}
			if err := sendDelta("config update"); err != nil {
				log.Printf("Error sending delta CDS update: %v\n", err)
				return err
			}
		}
	}
}

func (s *EDSServer) FetchEndpoints(context.Context, *discoveryv3.DiscoveryRequest) (*discoveryv3.DiscoveryResponse, error) {
	return nil, fmt.Errorf("FetchEndpoints is not implemented")
}

func (s *EDSServer) FetchClusters(context.Context, *discoveryv3.DiscoveryRequest) (*discoveryv3.DiscoveryResponse, error) {
	return nil, fmt.Errorf("FetchClusters is not implemented")
}

func main() {
	configPath := os.Getenv("EDS_CONFIG_PATH")
	if configPath == "" {
		configPath = defaultConfig
	}

	if err := ensureConfigFile(configPath); err != nil {
		log.Fatalf("Failed to prepare config file %s: %v", configPath, err)
	}

	services, err := loadServicesFromFile(configPath)
	if err != nil {
		log.Fatalf("Failed to load initial endpoint config %s: %v", configPath, err)
	}

	store := NewConfigStore(services)
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	go watchConfigFile(watchCtx, configPath, store)

	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", port, err)
	}

	grpcServer := grpc.NewServer()
	healthServer := grpcHealth.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	xdsServer := &EDSServer{store: store}
	edsv3.RegisterEndpointDiscoveryServiceServer(grpcServer, xdsServer)
	cdsv3.RegisterClusterDiscoveryServiceServer(grpcServer, xdsServer)
	healthpb.RegisterHealthServer(grpcServer, healthServer)

	fmt.Printf("xDS Server listening on %s (config: %s)\n", port, configPath)
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}

func makeEndpoint(address string, port uint32) *endpointv3.LbEndpoint {
	return &endpointv3.LbEndpoint{
		HostIdentifier: &endpointv3.LbEndpoint_Endpoint{
			Endpoint: &endpointv3.Endpoint{
				Address: &corev3.Address{
					Address: &corev3.Address_SocketAddress{
						SocketAddress: &corev3.SocketAddress{
							Address: address,
							PortSpecifier: &corev3.SocketAddress_PortValue{
								PortValue: port,
							},
						},
					},
				},
			},
		},
		HealthStatus: corev3.HealthStatus_HEALTHY,
	}
}

func isResourceRequested(resourceNames []string, clusterName string) bool {
	if len(resourceNames) == 0 {
		return true
	}

	for _, resourceName := range resourceNames {
		if resourceName == clusterName {
			return true
		}
	}

	return false
}

func watchConfigFile(ctx context.Context, configPath string, store *ConfigStore) {
	lastMod := time.Time{}
	if stat, err := os.Stat(configPath); err == nil {
		lastMod = stat.ModTime()
	}

	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stat, err := os.Stat(configPath)
			if err != nil {
				log.Printf("Config watch warning, stat failed: %v\n", err)
				continue
			}

			if !stat.ModTime().After(lastMod) {
				continue
			}

			services, err := loadServicesFromFile(configPath)
			if err != nil {
				log.Printf("Config reload failed: %v\n", err)
				lastMod = stat.ModTime()
				continue
			}

			version := store.Update(services)
			lastMod = stat.ModTime()
			log.Printf("Reloaded endpoint config from %s, version=%s\n", configPath, version)
		}
	}
}

func ensureConfigFile(configPath string) error {
	_, err := os.Stat(configPath)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	dir := filepath.Dir(configPath)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	configYAML, err := yaml.Marshal(defaultEndpointConfig())
	if err != nil {
		return err
	}

	if err := os.WriteFile(configPath, configYAML, 0o644); err != nil {
		return err
	}

	log.Printf("Created default endpoint config at %s\n", configPath)
	return nil
}

func loadServicesFromFile(configPath string) (map[string]serviceState, error) {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil, err
	}

	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	if len(root.Content) == 0 {
		return nil, fmt.Errorf("invalid YAML: empty document")
	}

	var servicesCfg []serviceConfig
	switch root.Content[0].Kind {
	case yaml.SequenceNode:
		// Support top-level array format: [ {cluster_name: ...}, ... ]
		if err := root.Content[0].Decode(&servicesCfg); err != nil {
			return nil, fmt.Errorf("invalid YAML service array: %w", err)
		}
	case yaml.MappingNode:
		var config endpointFileConfig
		if err := root.Content[0].Decode(&config); err != nil {
			return nil, fmt.Errorf("invalid YAML mapping: %w", err)
		}

		servicesCfg = config.Services
		if len(servicesCfg) == 0 {
			servicesCfg = []serviceConfig{{
				ClusterName:     config.ClusterName,
				ClusterLBPolicy: config.ClusterLBPolicy,
				ClusterType:     config.ClusterType,
				EndpointGroups:  config.EndpointGroups,
			}}
		}
	default:
		return nil, fmt.Errorf("invalid YAML root: expected mapping or sequence")
	}

	if len(servicesCfg) == 0 {
		return nil, fmt.Errorf("no services defined in config")
	}

	services := make(map[string]serviceState, len(servicesCfg))
	for i, svc := range servicesCfg {
		clusterName := strings.TrimSpace(svc.ClusterName)
		if clusterName == "" {
			return nil, fmt.Errorf("services[%d].cluster_name is required", i)
		}
		if _, exists := services[clusterName]; exists {
			return nil, fmt.Errorf("services[%d].cluster_name duplicates an existing service: %s", i, clusterName)
		}

		clusterLBPolicy, err := parseLBPolicy(svc.ClusterLBPolicy)
		if err != nil {
			return nil, fmt.Errorf("services[%d]: %w", i, err)
		}

		clusterType, err := parseClusterType(svc.ClusterType)
		if err != nil {
			return nil, fmt.Errorf("services[%d]: %w", i, err)
		}

		localityGroups := make([]*endpointv3.LocalityLbEndpoints, 0, len(svc.EndpointGroups))
		for j, group := range svc.EndpointGroups {
			if len(group.Endpoints) == 0 {
				return nil, fmt.Errorf("services[%d].endpoint_groups[%d] must contain at least one endpoint", i, j)
			}

			lbEndpoints := make([]*endpointv3.LbEndpoint, 0, len(group.Endpoints))
			for k, host := range group.Endpoints {
				if strings.TrimSpace(host.Address) == "" {
					return nil, fmt.Errorf("services[%d].endpoint_groups[%d].endpoints[%d].address is required", i, j, k)
				}
				if host.Port == 0 {
					return nil, fmt.Errorf("services[%d].endpoint_groups[%d].endpoints[%d].port must be > 0", i, j, k)
				}

				lbEndpoints = append(lbEndpoints, makeEndpoint(host.Address, host.Port))
			}

			localityGroups = append(localityGroups, &endpointv3.LocalityLbEndpoints{
				Priority: group.Priority,
				Locality: &corev3.Locality{
					Region:  group.Locality.Region,
					Zone:    group.Locality.Zone,
					SubZone: group.Locality.SubZone,
				},
				LbEndpoints: lbEndpoints,
			})
		}

		services[clusterName] = serviceState{
			assignment: &endpointv3.ClusterLoadAssignment{
				ClusterName: clusterName,
				Endpoints:   localityGroups,
			},
			clusterLBPolicy: clusterLBPolicy,
			clusterType:     clusterType,
		}
	}

	return services, nil
}

func parseLBPolicy(policy string) (clusterv3.Cluster_LbPolicy, error) {
	normalized := strings.ToUpper(strings.TrimSpace(policy))
	if normalized == "" {
		normalized = "LEAST_REQUEST"
	}

	switch normalized {
	case "ROUND_ROBIN":
		return clusterv3.Cluster_ROUND_ROBIN, nil
	case "LEAST_REQUEST":
		return clusterv3.Cluster_LEAST_REQUEST, nil
	case "RING_HASH":
		return clusterv3.Cluster_RING_HASH, nil
	case "RANDOM":
		return clusterv3.Cluster_RANDOM, nil
	default:
		return clusterv3.Cluster_ROUND_ROBIN, fmt.Errorf("unsupported cluster_lb_policy: %s", policy)
	}
}

func parseClusterType(clusterType string) (clusterv3.Cluster_DiscoveryType, error) {
	normalized := strings.ToUpper(strings.TrimSpace(clusterType))
	if normalized == "" {
		normalized = "EDS"
	}

	switch normalized {
	case "EDS":
		return clusterv3.Cluster_EDS, nil
	case "STATIC":
		return clusterv3.Cluster_STATIC, nil
	case "STRICT_DNS":
		return clusterv3.Cluster_STRICT_DNS, nil
	case "LOGICAL_DNS":
		return clusterv3.Cluster_LOGICAL_DNS, nil
	default:
		return clusterv3.Cluster_EDS, fmt.Errorf("unsupported cluster_type: %s", clusterType)
	}
}

func buildBackendCluster(name string, lbPolicy clusterv3.Cluster_LbPolicy, clusterType clusterv3.Cluster_DiscoveryType) *clusterv3.Cluster {
	cluster := &clusterv3.Cluster{
		Name:           name,
		ConnectTimeout: durationpb.New(1 * time.Second),
		ClusterDiscoveryType: &clusterv3.Cluster_Type{
			Type: clusterType,
		},
		LbPolicy: lbPolicy,
		HealthChecks: []*corev3.HealthCheck{
			{
				HealthChecker: &corev3.HealthCheck_HttpHealthCheck_{
					HttpHealthCheck: &corev3.HealthCheck_HttpHealthCheck{
						Path: "/ready",
						Host: name,
					},
				},
				Timeout:            durationpb.New(1 * time.Second),
				Interval:           durationpb.New(1 * time.Second),
				UnhealthyThreshold: &wrapperspb.UInt32Value{Value: 1},
				HealthyThreshold:   &wrapperspb.UInt32Value{Value: 2},
			},
		},
	}

	// EdsClusterConfig is only valid for EDS clusters.
	if clusterType == clusterv3.Cluster_EDS {
		cluster.EdsClusterConfig = &clusterv3.Cluster_EdsClusterConfig{
			EdsConfig: &corev3.ConfigSource{
				ResourceApiVersion: corev3.ApiVersion_V3,
				ConfigSourceSpecifier: &corev3.ConfigSource_ApiConfigSource{
					ApiConfigSource: &corev3.ApiConfigSource{
						ApiType:             corev3.ApiConfigSource_DELTA_GRPC,
						TransportApiVersion: corev3.ApiVersion_V3,
						GrpcServices: []*corev3.GrpcService{
							{
								TargetSpecifier: &corev3.GrpcService_EnvoyGrpc_{
									EnvoyGrpc: &corev3.GrpcService_EnvoyGrpc{ClusterName: xdsCluster},
								},
								Timeout: durationpb.New(1 * time.Second),
							},
						},
					},
				},
			},
		}
	}

	return cluster
}

func defaultEndpointConfig() endpointFileConfig {
	return endpointFileConfig{
		Services: []serviceConfig{
			{
				ClusterName:     "backend_cluster",
				ClusterLBPolicy: "LEAST_REQUEST",
				EndpointGroups: []endpointGroupConfig{
					{
						Priority: 0,
						Locality: endpointLocality{
							Region:  "test-region",
							Zone:    "test-zone-a",
							SubZone: "primary",
						},
						Endpoints: []hostConfig{
							{Address: "192.168.1.10", Port: 8080},
							{Address: "192.168.1.11", Port: 8080},
						},
					},
					{
						Priority: 1,
						Locality: endpointLocality{
							Region:  "test-region",
							Zone:    "test-zone-b",
							SubZone: "failover",
						},
						Endpoints: []hostConfig{
							{Address: "192.168.1.20", Port: 8080},
						},
					},
				},
			},
			{
				ClusterName:     "foo",
				ClusterLBPolicy: "RING_HASH",
				EndpointGroups: []endpointGroupConfig{
					{
						Priority: 0,
						Locality: endpointLocality{
							Region:  "region1",
							Zone:    "zone1",
							SubZone: "primary",
						},
						Endpoints: []hostConfig{
							{Address: "192.168.1.1", Port: 5000},
						},
					},
				},
			},
		},
	}
}

func sortedServiceNames(services map[string]serviceState) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func copyServiceMap(in map[string]serviceState) map[string]serviceState {
	out := make(map[string]serviceState, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func targetNames(services map[string]serviceState, includeAll bool, subscribed map[string]struct{}) []string {
	if includeAll {
		return sortedServiceNames(services)
	}

	names := make([]string, 0, len(subscribed))
	for name := range subscribed {
		if _, ok := services[name]; ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
