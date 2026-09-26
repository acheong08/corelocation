# corelocation

A small Go client and unified CLI for Apple's **undocumented location-data
services**. This is a rewrite of the core query functionality in
[apple-corelocation-experiments](https://github.com/acheong08/apple-corelocation-experiments),
not a wrapper around the macOS CoreLocation framework.


## Build

Go 1.22 or newer; no protobuf compiler is needed for normal builds.
From this repository's root:

```sh
go build -trimpath -o bin/corelocation ./cmd/corelocation
go test ./...
go test -race ./...

# Optional compact, CGo-free executable (about 8.6 MiB on darwin/arm64 with Go 1.27):
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/corelocation ./cmd/corelocation
```

## CLI

Flags follow the subcommand. Query results are JSON on stdout; diagnostics go to
stderr.

```sh
# Look up one or more known BSSIDs; zero limit uses Apple's default behavior.
bin/corelocation wifi --bssid 02:11:22:33:44:55 --bssid 02:aa:bb:cc:dd:ee --limit 10

# Location hints only choose endpoint order; they are not sent on the wire.
bin/corelocation wifi --bssid 02:11:22:33:44:55 --lat 39.9 --lon 116.4

# Force China first, or pin to China with no international fallback.
bin/corelocation wifi --bssid 02:11:22:33:44:55 --region china --no-fallback

# LTE identifiers are all explicit; MCC 460 prefers China without a hint.
bin/corelocation cell --mcc 460 --mnc 0 --cell-id 12345 --tac 123 --limit 10

# Fetch a single tile by key or by coordinates (always zoom 13).
bin/corelocation tile --key 81644851
bin/corelocation tile --lat 51.48 --lon -3.18

# Offline conversion; this never contacts Apple.
bin/corelocation tile-key --lat 51.48 --lon -3.18 --zoom 13
bin/corelocation tile-key --key 81644851

bin/corelocation --help
bin/corelocation wifi --help
```

The example BSSIDs and cell identifiers are illustrative, not known fixtures.
All query commands accept `--region auto|international|china`, `--no-fallback`,
`--timeout 20s` (whole query), and `--attempt-timeout 10s` (each endpoint).
`--lat` and `--lon` must be supplied together. Tile commands accept either a key
or coordinates, never both. Network `tile` queries always use zoom 13 and have
no `--zoom` flag; keys encoding a different zoom are rejected before networking.
Only offline `tile-key` encoding accepts (and requires) an explicit `--zoom`;
its JSON contains `key` as a decimal string (to preserve large values in
JavaScript), `zoom`, and `center`.
LTE identifier flags are decimal, including zero-padded input.

Wi-Fi and tile success shape:

```json
{
  "records": [
    {"bssid": "02:11:22:33:44:55", "location": {"latitude": 39.9, "longitude": 116.4}}
  ],
  "region": "china",
  "attempts": [
    {"region": "international", "error": "no usable locations returned"},
    {"region": "china"}
  ]
}
```

## Library

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "time"

    "github.com/acheong08/corelocation"
)

func main() {
    client, err := corelocation.NewClient(corelocation.Config{})
    if err != nil {
        panic(err)
    }
    ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
    defer cancel()

    result, err := client.LookupWiFi(ctx, corelocation.WiFiRequest{
        BSSIDs: []string{"02:11:22:33:44:55"},
        Hint: &corelocation.Point{Latitude: 39.9, Longitude: 116.4},
    })
    if errors.Is(err, corelocation.ErrNoResults) {
        fmt.Println("No usable data; attempts:", result.Attempts)
        return
    }
    if err != nil {
        panic(err)
    }
    fmt.Println(result.Region, result.Records)
}
```

The three network operations are `LookupWiFi`, `LookupCell`, and `FetchTile`.
`TileKeyFromPoint`, `TileKey.Center`, and `TileKey.Zoom` handle offline geometry.
For a queryable key use `TileKeyFromPoint(point, corelocation.WiFiTileZoom)`;
`FetchTile` rejects keys at any other zoom before network I/O.
Clients may be reused concurrently. Configuration is captured at construction;
do not mutate a shared HTTP transport or request inputs during a call.

`Config.Region` changes preference, **not** exclusivity. Use
`Config{Region: corelocation.China, DisableFallback: true}` to pin requests.
`Config.Endpoints` and `Config.HTTPClient` support proxies and deterministic tests.
Blank endpoint fields select defaults. Redirects are not followed, even when a
custom HTTP client has a redirect policy, to avoid forwarding identifiers.
The library never logs requests or records. HTTP error response bodies are not
exposed. `HTTPError` preserves `StatusCode` and `RetryAfter`; errors remain
inspectable with `errors.Is` / `errors.As`, including failures from both attempts.

### Result semantics

- Wi-Fi/cell responses can include **neighbors**, not only submitted identifiers.
- Zero `MaxResults` leaves the selection to the service. Positive limits are
  request hints, not a promise about returned count; negative values are invalid.
- Out-of-range coordinates, missing WLOC coordinates, absent tile location
  messages, WLOC's `(-180, -180)` sentinel, invalid identifiers, and duplicates
  are filtered. A real `(0, 0)` is not treated as missing. Tile coordinates are
  nonoptional proto3 scalars, so an empty tile location message cannot be
  distinguished from a real zero coordinate with the inherited schema.
- Results come from the first endpoint with any usable data. A partially resolved
  BSSID batch does **not** trigger a second query or merge. Split batches if
  per-identifier regional coverage is required.
- Only latitude/longitude are exposed. Uncertain altitude, accuracy, timestamp,
  and other reverse-engineered fields are not silently assigned guessed units.
  No WGS84/GCJ-02 conversion is applied; coordinates are as supplied by Apple.
- `Result.Attempts` is available even on network failure. Validation errors occur
  before any request and return a zero result.

## Regional routing without shapefiles

A rough routing hint is enough because a mistake changes endpoint order, not
which endpoint is available. The old embedded assets were about **2.6 MiB for
China and 34 MiB for water**; neither belongs in a focused query client.

Auto preference uses:

1. A caller's location hint for Wi-Fi/cell queries, or the decoded tile center.
2. Otherwise, China MCC **460** for LTE queries.
3. Otherwise, international first. A BSSID contains no reliable location hint.

The China preference box is latitude **18–54**, longitude **73–135**, inclusive.
Small exclusion boxes prefer international for Hong Kong (21.8–22.6,
113.8–114.5), Macau (22–22.25, 113.5–113.65), and Taiwan (21.8–25.4,
119.3–122.1).

Each query makes **at most two attempts**, one per region:

| First outcome | Behavior |
| --- | --- |
| Any usable records | Return immediately; do not merge regions |
| Empty/only unusable records | Try the other region |
| HTTP 404, 408, 429, or 5xx | Try the other region |
| Transport/read failure or per-attempt timeout | Try the other region if the caller's context is still alive |
| Caller cancellation/deadline | Stop immediately |
| Other HTTP status (including 401/403), malformed protobuf/envelope, oversized body | Stop; do not hide a protocol or configuration failure |

There is no retry loop, backoff, endpoint cache, or parallel racing. A 429 may
cause one alternate-region attempt, not a retry against the throttled endpoint;
`Retry-After` is exposed when a query fails. Applications doing repeated queries
must supply their own rate limiting and honor service throttling. Disable fallback
to avoid crossing infrastructures or doubling requests for genuinely empty areas.
The default response-body limit is 8 MiB and the per-attempt timeout is 10 seconds;
use a caller context to set a whole-operation deadline. The byte limit applies
before protobuf decoding (after HTTP decompression), not to total heap use;
decoded messages can occupy more memory. Use a lower limit for constrained
applications or untrusted endpoint overrides.

## Protocol and scope

The retained endpoints are international/China variants of `/clls/wloc` and
`/wifi_request_tile`. The WLOC adapter validates its 10-byte response envelope
before protobuf decoding. The version/function/32-bit-length interpretation is
inferred from the original client's companion server, not an Apple specification;
large real-world response fixtures remain a useful follow-up compatibility check.
WLOC coordinates scale by 1e-8, tile coordinates by 1e-7.
Request fingerprints are inherited from the original experiments, not
claimed to represent the latest iOS version.

Tile keys use sentinel-prefixed Morton interleaving of Web Mercator x/y indexes.
Coordinate conversion supports zoom 0–30 and latitude within ±85.0511287798066°;
+180° maps to the final longitude column. Decode returns the tile **center**, not
the original coordinate or the northwest corner. Network queries are restricted
to **zoom 13** (`WiFiTileZoom`); broader offline geometry support does not imply
API support, and keys at other zooms are rejected rather than silently remapped.

A live comparison on the international tile endpoint used the same Cardiff
coordinate (51.46769695622337, -3.27392578125), with fallback disabled:

| Zoom | Tile key | Observed result |
| --- | --- | --- |
| 12 | `20411212` | HTTP 404 |
| 13 | `81644851` | Success: 454 decoded access points |
| 14 | `326579407` | HTTP 404 |

These are observations from a single location, not an Apple service specification
or a guarantee that every zoom-13 tile contains data. They support exposing only
the known working service zoom instead of an unsupported query option.

Not included: crawlers, seed collection, nearest-AP exploration, shapefile/water
filtering, SQLite, UI servers, spoofing, data submission, multilateration, and
Ichnaea compatibility. Those can be independent consumers of this library rather
than dependencies of the core.

## Development and provenance

Tests use local HTTP servers and synthetic protocol fixtures, never Apple's live
services. They cover framing, coordinates, input validation, response limits,
region preference/fallback, cancellation, transport/status failures, CLI behavior,
and concurrent use. Fuzz targets exercise both response decoders. These tests
validate the inherited protocol contract, not current live-service availability.
Separate manual smoke checks during the rewrite sent only the all-zero placeholder
BSSID to each WLOC endpoint: both returned validly framed responses with no usable
records, producing the expected CLI exit code 1. This checks basic endpoint and
framing compatibility, not real-record accuracy, tile availability, or every
possible response shape.

```sh
go vet ./...
go test -race ./...
go test -run '^$' -fuzz '^FuzzDecodeWLoc$' -fuzztime 10s .
go test -run '^$' -fuzz '^FuzzDecodeTile$' -fuzztime 10s .
```

Generated protobuf sources are committed. To regenerate (requires `protoc` and
`protoc-gen-go` v1.34.1 on PATH):

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.1
protoc -I internal --go_out=internal --go_opt=paths=source_relative \
  internal/pb/BSSIDApple.proto internal/pb/wifiTiles.proto
```

The two query schemas and their generated Go sources are retained verbatim from
upstream commit `0b136860e80585c02c12f64933995b872d7623af`; their legacy `go_package`
option is retained for reproducibility. They are physically under Go's `internal`
visibility boundary, not the public API. All client, routing, geometry, and CLI
code is rewritten. The original GPLv3 license is retained; see [LICENSE](LICENSE)
and [NOTICE](NOTICE).
