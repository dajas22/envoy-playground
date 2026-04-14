# priorityXDS

This directory contains a minimal reproducer for testing Envoy behavior with
an EDS response that contains multiple endpoint groups and priority failover.

The purpose of this test is to verify that Envoy correctly loads a
`ClusterLoadAssignment` for `backend_cluster`, where there are two endpoint
groups:

- primary group with `priority: 0`
- failover group with `priority: 1`

## How It Works

- `main.go` implements a simple gRPC EDS server in Go.
- The server listens on port `5000`.
- Endpoint configuration is loaded from `endpoints.yaml` and watched
  periodically.
- The server exports `EndpointDiscoveryService` and a gRPC health endpoint, so
  it matches how the Envoy gRPC cluster is defined in this repository.
- For resource type `envoy.config.endpoint.v3.ClusterLoadAssignment`, it
  returns a response for `backend_cluster`.
- The response contains two endpoint groups:
  - `priority: 0` with endpoints `192.168.1.10:8080` and `192.168.1.11:8080`
  - `priority: 1` with endpoint `192.168.1.20:8080`
- When `endpoints.yaml` changes, the XDS server loads the new configuration and
  pushes a new EDS version to open streams.
- `envoy.yaml` contains:
  - `xds_cluster` defined as a gRPC upstream in the style of `newGrpcCluster`
  - `backend_cluster` defined as an EDS cluster in the style of
    `newHttpEdsCluster`
  - a static listener and route that direct traffic to `backend_cluster`
- `docker-compose.yml` starts two services:
  - `xds`: local gRPC EDS server
  - `envoy`: Envoy `v1.37.1`

## What This Tests

The test verifies how Envoy behaves when it:

1. boots with a valid configuration,
2. successfully connects to the gRPC EDS server,
3. receives a valid EDS response for the upstream cluster,
4. receives backend endpoints split across two priorities.

The expected result is that Envoy does not crash and has a `backend_cluster`
configuration with two endpoint priorities. If `priority: 0` becomes
unavailable, Envoy can fail over to `priority: 1`.

## Dynamic Endpoint Updates

Endpoint configuration is stored in `endpoints.yaml` in this format:

```yaml
services:
  - cluster_name: backend_cluster
    cluster_lb_policy: LEAST_REQUEST
    endpoint_groups:
      - priority: 0
        locality:
          region: test-region
          zone: test-zone-a
          sub_zone: primary
        endpoints:
          - address: 192.168.1.10
            port: 8080

  - cluster_name: foo
    cluster_lb_policy: RING_HASH
    endpoint_groups:
      - priority: 0
        locality:
          region: region1
          zone: zone1
          sub_zone: primary
        endpoints:
          - address: 192.168.1.1
            port: 5000
```

After saving the file, the configuration is reloaded in about 1 second and the
XDS server pushes an update to connected clients.

## Run

From the test directory:

```bash
docker compose up --build
```

After startup, these endpoints are available:

- Envoy listener: `http://localhost:8080`
- Envoy admin: `http://localhost:10000`
- XDS server: `localhost:5000`

## Manual Verification

Send a request through Envoy:

```bash
curl -v http://localhost:8080/
```

Useful logs:

```bash
docker compose logs -f envoy
docker compose logs -f xds
```

In the Envoy admin interface, you can inspect cluster state and config dump,
for example:

```bash
curl http://localhost:10000/clusters
curl http://localhost:10000/config_dump

# quick check that backend_cluster has priorities 0 and 1
curl -s http://localhost:10000/config_dump | grep -E 'backend_cluster|priority'

# verify endpoints and priorities directly from admin /clusters
curl -s http://localhost:10000/clusters | sed -n '/backend_cluster::/,/xds_cluster::/p'
```

## Stop

```bash
docker compose down
```
