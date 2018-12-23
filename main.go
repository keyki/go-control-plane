package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/endpoint"
	"net"
	"os"

	"sync"
	"sync/atomic"
	"time"

	"github.com/envoyproxy/go-control-plane/pkg/cache"
	xds "github.com/envoyproxy/go-control-plane/pkg/server"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	"github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/auth"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/core"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/route"
	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v2"

	hcm "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	"github.com/envoyproxy/go-control-plane/pkg/util"
)

var (
	debug       bool
	onlyLogging bool

	port        uint
	gatewayPort uint
	alsPort     uint

	mode string

	version int32

	config cache.SnapshotCache
)

const (
	Ads  = "ads"
	Xds  = "xds"
	Rest = "rest"
)

func init() {
	flag.BoolVar(&debug, "debug", true, "Use debug logging")
	flag.BoolVar(&onlyLogging, "onlyLogging", false, "Only demo AccessLogging Service")
	flag.UintVar(&port, "port", 18000, "Management server port")
	flag.UintVar(&gatewayPort, "gateway", 18001, "Management server port for HTTP gateway")
	flag.UintVar(&alsPort, "als", 18090, "Accesslog server port")
	flag.StringVar(&mode, "ads", Ads, "Management server type (ads, xds, rest)")
}

type logger struct{}

func (logger logger) Infof(format string, args ...interface{}) {
	log.Infof(format, args...)
}
func (logger logger) Errorf(format string, args ...interface{}) {
	log.Errorf(format, args...)
}
func (cb *callbacks) Report() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	log.WithFields(log.Fields{"fetches": cb.fetches, "requests": cb.requests}).Info("cb.Report()  callbacks")
}
func (cb *callbacks) OnStreamOpen(c context.Context, i int64, s string) error {
	log.Infof("OnStreamOpen %d open for %s", i, s)
	return nil
}
func (cb *callbacks) OnStreamClosed(id int64) {
	log.Infof("OnStreamClosed %d closed", id)
}
func (cb *callbacks) OnStreamRequest(i int64, req *v2.DiscoveryRequest) {
	log.Debugf("stream request received for %s", req.TypeUrl)
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.requests++
	if cb.signal != nil {
		close(cb.signal)
		cb.signal = nil
	}
}
func (cb *callbacks) OnStreamResponse(i int64, req *v2.DiscoveryRequest, resp *v2.DiscoveryResponse) {
	cb.Report()
}
func (cb *callbacks) OnFetchRequest(ctx context.Context, req *v2.DiscoveryRequest) error {
	log.Infof("OnFetchRequest...")
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.fetches++
	if cb.signal != nil {
		close(cb.signal)
		cb.signal = nil
	}
	return nil
}
func (cb *callbacks) OnFetchResponse(*v2.DiscoveryRequest, *v2.DiscoveryResponse) {
	cb.Report()
}

type callbacks struct {
	signal   chan struct{}
	fetches  int
	requests int
	mu       sync.Mutex
}

const grpcMaxConcurrentStreams = 1000000

// RunManagementServer starts an xDS server at the given port.
func RunManagementServer(ctx context.Context, server xds.Server, port uint) {
	var grpcOptions []grpc.ServerOption
	grpcOptions = append(grpcOptions, grpc.MaxConcurrentStreams(grpcMaxConcurrentStreams))
	grpcServer := grpc.NewServer(grpcOptions...)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.WithError(err).Fatal("failed to listen")
	}

	// register services
	discovery.RegisterAggregatedDiscoveryServiceServer(grpcServer, server)
	v2.RegisterEndpointDiscoveryServiceServer(grpcServer, server)
	v2.RegisterClusterDiscoveryServiceServer(grpcServer, server)
	v2.RegisterRouteDiscoveryServiceServer(grpcServer, server)
	v2.RegisterListenerDiscoveryServiceServer(grpcServer, server)

	log.WithFields(log.Fields{"port": port}).Info("management server listening")
	go func() {
		if err = grpcServer.Serve(lis); err != nil {
			log.Error(err)
		}
	}()
	<-ctx.Done()

	grpcServer.GracefulStop()
}

type hasher2 struct {
}

func (h hasher2) ID(node *core.Node) string {
	if node == nil {
		return "unknown"
	}
	return node.Id
}

func main() {
	flag.Parse()
	if debug {
		log.SetLevel(log.DebugLevel)
	}
	ctx := context.Background()

	log.Printf("Starting control plane")

	signal := make(chan struct{})
	cb := &callbacks{
		signal:   signal,
		fetches:  0,
		requests: 0,
	}
	h := new(hasher2)
	config = cache.NewSnapshotCache(mode == Ads, h, logger{})

	srv := xds.NewServer(config, cb)

	if onlyLogging {
		cc := make(chan struct{})
		<-cc
		os.Exit(0)
	}

	// start the xDS server
	go RunManagementServer(ctx, srv, port)

	<-signal

	cb.Report()

	for {
		atomic.AddInt32(&version, 1)
		nodeId := config.GetStatusKeys()[1]

		var clusterName = "service_google"
		var remoteHost = "google.com"
		var fullRemoteHost = "www." + remoteHost

		loadAssignment := &v2.ClusterLoadAssignment{
			ClusterName: clusterName,
			Endpoints: []endpoint.LocalityLbEndpoints{{
				LbEndpoints: []endpoint.LbEndpoint{{
					Endpoint: &endpoint.Endpoint{
						Address: &core.Address{
							Address: &core.Address_SocketAddress{
								SocketAddress: &core.SocketAddress{
									Protocol: core.TCP,
									Address:  remoteHost,
									PortSpecifier: &core.SocketAddress_PortValue{
										PortValue: uint32(443),
									},
								},
							},
						},
					},
				}},
			}},
		}

		cluster := []cache.Resource{
			&v2.Cluster{
				Name:            clusterName,
				ConnectTimeout:  1 * time.Second,
				Type:            v2.Cluster_LOGICAL_DNS,
				DnsLookupFamily: v2.Cluster_V4_ONLY,
				LbPolicy:        v2.Cluster_ROUND_ROBIN,
				LoadAssignment:loadAssignment,
				TlsContext: &auth.UpstreamTlsContext{
					Sni: fullRemoteHost,
				},
			},
		}

		virtualHost := route.VirtualHost{
			Name:    "local_service",
			Domains: []string{"*"},
			Routes: []route.Route{{
				Match: route.RouteMatch{
					PathSpecifier: &route.RouteMatch_Prefix{
						Prefix: "/",
					},
				},
				Action: &route.Route_Route{
					Route: &route.RouteAction{
						HostRewriteSpecifier: &route.RouteAction_HostRewrite{
							HostRewrite: fullRemoteHost,
						},
						ClusterSpecifier: &route.RouteAction_Cluster{
							Cluster: clusterName,
						},
					},
				},
			}}}

		connectionManager := &hcm.HttpConnectionManager{
			CodecType:  hcm.AUTO,
			StatPrefix: "ingress_http",
			RouteSpecifier: &hcm.HttpConnectionManager_RouteConfig{
				RouteConfig: &v2.RouteConfiguration{
					Name:         "local_route",
					VirtualHosts: []route.VirtualHost{virtualHost},
				},
			},
			HttpFilters: []*hcm.HttpFilter{{
				Name: util.Router,
			}},
		}

		cmStruct, err := util.MessageToStruct(connectionManager)
		if err != nil {
			panic(err)
		}

		listenr := []cache.Resource{
			&v2.Listener{
				Name: "listener_0",
				Address: core.Address{
					Address: &core.Address_SocketAddress{
						SocketAddress: &core.SocketAddress{
							Protocol: core.TCP,
							Address:  "0.0.0.0",
							PortSpecifier: &core.SocketAddress_PortValue{
								PortValue: 10000,
							},
						},
					},
				},
				FilterChains: []listener.FilterChain{{
					Filters: []listener.Filter{{
						Name: util.HTTPConnectionManager,
						ConfigType: &listener.Filter_Config{
							Config: cmStruct,
						},
					}},
				}},
			}}

		log.Infof("creating snapshot Version %s for node %s", fmt.Sprint(version), nodeId)
		snap := cache.NewSnapshot(fmt.Sprint(version), nil, cluster, nil, listenr)
		if err = config.SetSnapshot(nodeId, snap); err != nil {
			log.Error(err)
		}

		reader := bufio.NewReader(os.Stdin)
		_, _ = reader.ReadString('\n')
	}

}
