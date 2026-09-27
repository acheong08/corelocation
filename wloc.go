package corelocation

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/http"

	"github.com/acheong08/corelocation/internal/pb"
	"google.golang.org/protobuf/proto"
)

// LookupWiFi returns located access points, which may include neighbors beyond
// the submitted BSSIDs. It does not perform RSSI positioning or multilateration.
func (c *Client) LookupWiFi(ctx context.Context, req WiFiRequest) (Result[AccessPoint], error) {
	if len(req.BSSIDs) == 0 {
		return Result[AccessPoint]{}, fmt.Errorf("at least one BSSID is required")
	}
	if err := validateQuery(req.MaxResults, req.Hint); err != nil {
		return Result[AccessPoint]{}, err
	}
	zero := int32(0)
	block := &pb.AppleWLoc{NumCellResults: &zero, NumWifiResults: &req.MaxResults, DeviceType: deviceType()}
	seen := make(map[string]bool, len(req.BSSIDs))
	for _, s := range req.BSSIDs {
		mac, err := NormalizeBSSID(s)
		if err != nil {
			return Result[AccessPoint]{}, err
		}
		if !seen[mac] {
			block.WifiDevices = append(block.WifiDevices, &pb.WifiDevice{Bssid: mac})
			seen[mac] = true
		}
	}
	payload, err := encodeWLoc(block)
	if err != nil {
		return Result[AccessPoint]{}, err
	}
	return query(ctx, c, req.Hint, 0, func(ctx context.Context, region Region) ([]AccessPoint, error) {
		resp, err := c.wloc(ctx, region, payload)
		if err != nil {
			return nil, err
		}
		out := make([]AccessPoint, 0)
		seen := make(map[string]bool)
		for _, d := range resp.WifiDevices {
			if err := ctx.Err(); err != nil {
				return nil, &transportError{err}
			}
			p, ok := location(d.GetLocation())
			mac, err := NormalizeBSSID(d.GetBssid())
			if !ok || err != nil || seen[mac] {
				continue
			}
			out = append(out, AccessPoint{BSSID: mac, Location: p})
			seen[mac] = true
		}
		return truncate(out, req.MaxResults), nil
	})
}

// LookupCell queries Apple's LTE dataset. The returned cells can include nearby
// towers rather than only the requested tower.
func (c *Client) LookupCell(ctx context.Context, req CellRequest) (Result[Cell], error) {
	if err := req.Tower.Validate(); err != nil {
		return Result[Cell]{}, err
	}
	if err := validateQuery(req.MaxResults, req.Hint); err != nil {
		return Result[Cell]{}, err
	}
	disabled := int32(-1)
	block := &pb.AppleWLoc{NumCellResults: &req.MaxResults, NumWifiResults: &disabled, DeviceType: deviceType(), CellTowerRequest: &pb.CellTower{Mcc: req.Tower.MCC, Mnc: req.Tower.MNC, CellId: req.Tower.CellID, TacId: req.Tower.TAC}}
	payload, err := encodeWLoc(block)
	if err != nil {
		return Result[Cell]{}, err
	}
	return query(ctx, c, req.Hint, req.Tower.MCC, func(ctx context.Context, region Region) ([]Cell, error) {
		resp, err := c.wloc(ctx, region, payload)
		if err != nil {
			return nil, err
		}
		out := make([]Cell, 0)
		seen := make(map[Tower]bool)
		for _, d := range resp.CellTowerResponse {
			if err := ctx.Err(); err != nil {
				return nil, &transportError{err}
			}
			p, ok := location(d.GetLocation())
			tower := Tower{MCC: d.GetMcc(), MNC: d.GetMnc(), CellID: d.GetCellId(), TAC: d.GetTacId()}
			if !ok || tower.Validate() != nil || seen[tower] {
				continue
			}
			out = append(out, Cell{Tower: tower, Location: p})
			seen[tower] = true
		}
		return truncate(out, req.MaxResults), nil
	})
}

// truncate enforces MaxResults locally: Apple's service overshoots the
// requested count with neighboring records, so the caller-side cap is what
// makes a positive MaxResults a real maximum.
func truncate[T any](records []T, max int32) []T {
	if max > 0 && int32(len(records)) > max {
		return records[:max]
	}
	return records
}

func validateQuery(limit int32, hint *Point) error {
	if limit < 0 {
		return fmt.Errorf("max results cannot be negative")
	}
	if hint != nil {
		return hint.Validate()
	}
	return nil
}

func deviceType() *pb.DeviceType {
	return &pb.DeviceType{OperatingSystem: "iPhone OS17.5/21F79", Model: "iPhone12,1"}
}

func location(loc *pb.Location) (Point, bool) {
	if loc == nil || loc.Latitude == nil || loc.Longitude == nil {
		return Point{}, false
	}
	p := Point{Latitude: float64(*loc.Latitude) * 1e-8, Longitude: float64(*loc.Longitude) * 1e-8}
	return p, p.Validate() == nil
}

// encodeWLoc keeps the legacy ARPC envelope confined to this wire adapter.
func encodeWLoc(block *pb.AppleWLoc) ([]byte, error) {
	payload, err := proto.Marshal(block)
	if err != nil {
		return nil, err
	}
	if uint64(len(payload)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("WLOC request too large")
	}
	out := binary.BigEndian.AppendUint16(nil, 1)
	for _, s := range []string{"en-001_001", "com.apple.locationd", "18.6.2.22G100"} {
		out = binary.BigEndian.AppendUint16(out, uint16(len(s)))
		out = append(out, s...)
	}
	out = binary.BigEndian.AppendUint32(out, 1)
	out = binary.BigEndian.AppendUint32(out, uint32(len(payload)))
	return append(out, payload...), nil
}

func decodeWLoc(data []byte) (*pb.AppleWLoc, error) {
	// Response: uint16 version, uint32 function, uint32 payload length, protobuf.
	// Never slice an untrusted body before checking the envelope.
	if len(data) < 10 {
		return nil, fmt.Errorf("%w: truncated WLOC envelope", ErrInvalidResponse)
	}
	if binary.BigEndian.Uint16(data[:2]) != 1 || binary.BigEndian.Uint32(data[2:6]) != 1 {
		return nil, fmt.Errorf("%w: unsupported WLOC envelope", ErrInvalidResponse)
	}
	if uint64(binary.BigEndian.Uint32(data[6:10])) != uint64(len(data)-10) {
		return nil, fmt.Errorf("%w: WLOC payload length mismatch", ErrInvalidResponse)
	}
	var block pb.AppleWLoc
	if err := proto.Unmarshal(data[10:], &block); err != nil {
		return nil, fmt.Errorf("%w: WLOC protobuf: %v", ErrInvalidResponse, err)
	}
	return &block, nil
}

func (c *Client) wloc(ctx context.Context, region Region, payload []byte) (*pb.AppleWLoc, error) {
	endpoint := c.endpoints.WLocInternational
	if region == China {
		endpoint = c.endpoints.WLocChina
	}
	data, err := c.request(ctx, http.MethodPost, endpoint, payload, "")
	if err != nil {
		return nil, err
	}
	return decodeWLoc(data)
}
