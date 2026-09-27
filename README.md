# Corelocation

A small Go client and unified CLI for Apple's undocumented location-data
services. This is a rewrite of the core query functionality in
[apple-corelocation-experiments](https://github.com/acheong08/apple-corelocation-experiments).

## Build

```sh
go build ./cmd/corelocation

# Optional compact
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/corelocation ./cmd/corelocation
```

## CLI

```bash
# You can query one or more BSSIDs, and limit the number of responses.
# Set --limit 0 for unlimited records.
bin/corelocation wifi --bssid 6C:63:F8:27:47:3F -bssid 00:13:ef:12:43:b4 --limit 5

# Find points nearby a certain point
bin/corelocation tile -lat 29.713767 -lon 107.396009

# Force China first, or pin to China with no international fallback
bin/corelocation wifi --bssid 60:da:83:9d:70:60 --limit 3 --region china --no-fallback

# Find nearby cell towers
bin/corelocation cell --mcc 260 --mnc 6 --cell-id 119093 --tac 58 --limit 10

bin/corelocation --help
bin/corelocation wifi --help
```

Response structure:

```json
{
  "records": [
    {
      "bssid": "60:da:83:9d:70:60",
      "location": {
        "latitude": 29.72016143,
        "longitude": 107.37326812
      }
    },
    {
      "bssid": "10:5d:dc:17:44:21",
      "location": {
        "latitude": 29.71990966,
        "longitude": 107.37315368
      }
    },
    {
      "bssid": "10:9f:4f:bf:3a:e9",
      "location": {
        "latitude": 29.72004699,
        "longitude": 107.37274932
      }
    }
  ],
  "region": "china",
  "attempts": [
    {
      "region": "international",
      "error": "no usable locations returned"
    },
    {
      "region": "china"
    }
  ]
}
```

<details>

<summary>

## Library Doc

</summary>

```go
package corelocation // import "github.com/acheong08/corelocation"

Package corelocation queries Apple's undocumented Wi-Fi, LTE cell, and Wi-Fi
tile services. It is not a binding to the macOS CoreLocation framework.

CONSTANTS

const WiFiTileZoom = 13
    WiFiTileZoom is the supported zoom for Apple's Wi-Fi tile service. Other
    zooms are useful for offline geometry but are not accepted by FetchTile.


VARIABLES

var (
	ErrNoResults        = errors.New("no usable locations returned")
	ErrInvalidResponse  = errors.New("invalid service response")
	ErrResponseTooLarge = errors.New("service response exceeds size limit")
)

FUNCTIONS

func NormalizeBSSID(s string) (string, error)
    NormalizeBSSID accepts six-byte colon, hyphen, or dotted MAC notation,
    including single-digit colon-separated octets returned by some Apple
    records.

func PreferChina(p Point) bool
    PreferChina is an intentionally coarse routing hint, NOT a political
    boundary or geofence. Border false positives are handled by fallback.
    Small exclusion boxes prefer the international dataset for Hong Kong, Macau,
    and Taiwan.


TYPES

type AccessPoint struct {
	BSSID    string `json:"bssid"`
	Location Point  `json:"location"`
}
    AccessPoint is a located Wi-Fi access point, not an estimate of the caller's
    location. Unknown optional wire fields are deliberately not given units.

type Attempt struct {
	Region Region `json:"region"`
	Error  string `json:"error,omitempty"`
}
    Attempt records endpoint provenance without logging submitted identifiers.

type Cell struct {
	Tower    Tower `json:"tower"`
	Location Point `json:"location"`
}

type CellRequest struct {
	Tower      Tower
	MaxResults int32
	Hint       *Point
}

type Client struct {
	// Has unexported fields.
}
    Client is immutable after construction and safe for concurrent use.

func NewClient(cfg Config) (*Client, error)

func (c *Client) FetchTile(ctx context.Context, key TileKey) (Result[AccessPoint], error)
    FetchTile returns located access points in a zoom-13 Wi-Fi tile. Keys at
    other zooms are rejected before network I/O. Its center determines initial
    endpoint preference; empty tiles fall back unless Config.DisableFallback is
    set.

func (c *Client) LookupCell(ctx context.Context, req CellRequest) (Result[Cell], error)
    LookupCell queries Apple's LTE dataset. The returned cells can include
    nearby towers rather than only the requested tower.

func (c *Client) LookupWiFi(ctx context.Context, req WiFiRequest) (Result[AccessPoint], error)
    LookupWiFi returns located access points, which may include neighbors
    beyond the submitted BSSIDs. It does not perform RSSI positioning or
    multilateration.

type Config struct {
	// HTTPClient is copied at construction. Its Transport must support concurrent
	// use. Redirects are always disabled to avoid forwarding location queries.
	HTTPClient *http.Client
	// Region sets first preference, not a hard pin. Pair with DisableFallback to
	// guarantee queries are sent to only one region.
	Region          Region
	DisableFallback bool
	// AttemptTimeout defaults to 10 seconds; the caller's context bounds the
	// entire operation, including both attempts.
	AttemptTimeout time.Duration
	// MaxResponseBytes defaults to 8 MiB, including the WLOC envelope.
	MaxResponseBytes int64
	// Empty endpoint fields use defaults. Overrides are useful for local testing.
	Endpoints Endpoints
}

type Endpoints struct {
	WLocInternational string
	WLocChina         string
	TileInternational string
	TileChina         string
}

func DefaultEndpoints() Endpoints

type HTTPError struct {
	StatusCode int
	RetryAfter string
}
    HTTPError preserves status and Retry-After without exposing response bodies.

func (e *HTTPError) Error() string

type Point struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}
    Point contains geographic coordinates in degrees. Coordinates are returned
    as supplied by the service; no datum transformation is performed.

func (p Point) Validate() error

type Region string
    Region chooses endpoint preference. Auto uses a location hint, cell MCC,
    or tile center; without a hint it starts with International.

const (
	Auto          Region = "auto"
	International Region = "international"
	China         Region = "china"
)
func PreferredRegion(hint *Point, mcc uint32) Region
    PreferredRegion selects the initial endpoint. An explicit coordinate hint
    wins over the MCC (460); no inference is possible from a BSSID alone.

type Result[T any] struct {
	Records  []T       `json:"records"`
	Region   Region    `json:"region,omitempty"`
	Attempts []Attempt `json:"attempts"`
}
    Result retains attempts even on failure. Records come from the first
    endpoint with usable data, not a merge across regions. Partial batches do
    not fallback.

type TileKey uint64
    TileKey identifies a Web Mercator tile using Morton order: column (x) bits
    occupy even positions, row (y) bits occupy odd positions, and a sentinel bit
    at position 2*zoom records the zoom level. Offline geometry supports zooms 0
    to 30; network queries through FetchTile accept only WiFiTileZoom.

func TileKeyFromPoint(p Point, zoom int) (TileKey, error)
    TileKeyFromPoint returns the tile containing p at zoom. Coordinates
    outside the Web Mercator latitude range are rejected rather than clamped.
    Longitude +180 belongs to the last column, not the first column across the
    antimeridian.

func (k TileKey) Center() (Point, error)
    Center returns the geographic center of k's tile, not its northwest corner.

func (k TileKey) Zoom() (int, error)
    Zoom returns the zoom encoded by k, rejecting zero, an odd-position
    sentinel, or a zoom greater than 30. Every lower bit is part of the Morton
    payload.

type Tower struct {
	MCC    uint32 `json:"mcc"`
	MNC    uint32 `json:"mnc"`
	CellID uint32 `json:"cell_id"`
	TAC    uint32 `json:"tac"`
}
    Tower identifies an LTE cell. GSM, UMTS and NR are not supported.

func (t Tower) Validate() error

type WiFiRequest struct {
	BSSIDs []string
	// MaxResults is passed to Apple. Zero selects the service default; positive
	// values are enforced client-side as a hard cap on returned records.
	MaxResults int32
	// Hint affects routing only and is never transmitted to Apple.
	Hint *Point
}
```

</details>

## Development

Generated protobuf sources are committed. To regenerate (requires `protoc` and
`protoc-gen-go` v1.34.1 on PATH):

```sh
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.34.1
protoc -I internal --go_out=internal --go_opt=paths=source_relative \
  internal/pb/BSSIDApple.proto internal/pb/wifiTiles.proto
```
