# priorityXDS

Tento adresar obsahuje minimalni reproduktor pro testovani chovani Envoye pri
EDS odpovedi s vice endpoint groupami a prioritnim failoverem.

Smyslem testu je overit, ze Envoy korektne nacita `ClusterLoadAssignment` pro
`backend_cluster`, kde jsou dve skupiny endpointu:

- primarni skupina s `priority: 0`
- failover skupina s `priority: 1`

## Jak to funguje

- `main.go` implementuje jednoduchy gRPC EDS server v Go.
- Server posloucha na portu `5000`.
- Endpoint konfigurace se nacita ze souboru `endpoints.json` a je periodicky
  sledovana.
- Server exportuje `EndpointDiscoveryService` a gRPC health endpoint, aby
  odpovidal tomu, jak v repozitari vypada gRPC cluster pro Envoy.
- Pro resource type `envoy.config.endpoint.v3.ClusterLoadAssignment` vraci
  odpoved pro `backend_cluster`.
- Odpoved obsahuje dve endpoint groupy:
  - `priority: 0` s endpointy `192.168.1.10:8080` a `192.168.1.11:8080`
  - `priority: 1` s endpointem `192.168.1.20:8080`
- Pri zmene `endpoints.json` XDS server nacte novou konfiguraci a posle novou
  EDS verzi do otevrenych streamu.
- `envoy.yaml` obsahuje:
  - `xds_cluster` definovany jako gRPC upstream ve stylu `newGrpcCluster`
  - `backend_cluster` definovany jako EDS cluster ve stylu `newHttpEdsCluster`
  - staticky listener a route, ktere smeruji provoz do `backend_cluster`
- `docker-compose.yml` spousti dve sluzby:
  - `xds`: lokalni gRPC EDS server
  - `envoy`: Envoy `v1.37.1`

## Co se tim testuje

Test overuje, jak se Envoy zachova, kdyz:

1. nabootuje s validni konfiguraci,
2. uspesne se pripoji na gRPC EDS server,
3. dostane pro upstream cluster validni EDS odpoved,
4. tato odpoved obsahuje backend endpointy rozdelene do dvou priorit.

Ocekavany vysledek testu je, ze Envoy nespadne a v konfiguraci bude mit
`backend_cluster` se dvema prioritami endpointu. Pri nedostupnosti `priority: 0`
muze prejit na `priority: 1`.

## Dynamicka uprava endpointu

Konfigurace endpointu je v `endpoints.json` v tomto formatu:

```json
{
  "cluster_name": "backend_cluster",
  "endpoint_groups": [
    {
      "priority": 0,
      "locality": {
        "region": "test-region",
        "zone": "test-zone-a",
        "sub_zone": "primary"
      },
      "endpoints": [
        { "address": "192.168.1.10", "port": 8080 }
      ]
    }
  ]
}
```

Po ulozeni souboru se konfigurace behem cca 1 sekundy nacte a XDS server posle
aktualizaci klientum.

## Spusteni

Z adresare s testem:

```bash
docker compose up --build
```

Po startu budou dostupne:

- Envoy listener: `http://localhost:8080`
- Envoy admin: `http://localhost:10000`
- XDS server: `localhost:5000`

## Rucni overeni

Poslani requestu pres Envoy:

```bash
curl -v http://localhost:8080/
```

Uzitecne logy:

```bash
docker compose logs -f envoy
docker compose logs -f xds
```

U admin rozhrani Envoye lze zkontrolovat stav clusteru a config dump, napr.:

```bash
curl http://localhost:10000/clusters
curl http://localhost:10000/config_dump

# rychla kontrola, ze backend_cluster ma priority 0 a 1
curl -s http://localhost:10000/config_dump | grep -E 'backend_cluster|priority'

# overeni endpointu a priorit primo z admin /clusters
curl -s http://localhost:10000/clusters | sed -n '/backend_cluster::/,/xds_cluster::/p'
```

## Ukonceni

```bash
docker compose down
```
