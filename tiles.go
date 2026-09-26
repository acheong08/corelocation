package corelocation

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/acheong08/corelocation/internal/pb"
	"google.golang.org/protobuf/proto"
)

// FetchTile returns located access points in a Wi-Fi tile. Its center is used
// only for initial endpoint preference; empty tiles fall back to the other
// region unless Config.DisableFallback is set. Apple may not serve every zoom.
func (c *Client) FetchTile(ctx context.Context, key TileKey) (Result[AccessPoint], error) {
	center, err := key.Center()
	if err != nil {
		return Result[AccessPoint]{}, err
	}
	return query(ctx, c, &center, 0, func(ctx context.Context, region Region) ([]AccessPoint, error) {
		endpoint := c.endpoints.TileInternational
		if region == China {
			endpoint = c.endpoints.TileChina
		}
		data, err := c.request(ctx, http.MethodGet, endpoint, nil, strconv.FormatUint(uint64(key), 10))
		if err != nil {
			return nil, err
		}
		return decodeTile(data)
	})
}
func decodeTile(data []byte) ([]AccessPoint, error) {
	var tile pb.WifiTile
	if err := proto.Unmarshal(data, &tile); err != nil {
		return nil, fmt.Errorf("%w: tile protobuf: %v", ErrInvalidResponse, err)
	}
	out := make([]AccessPoint, 0)
	seen := make(map[int64]bool)
	for _, region := range tile.Region {
		for _, d := range region.GetDevices() {
			if d == nil || d.Bssid <= 0 || d.Bssid > 0xffffffffffff || d.Entry == nil || seen[d.Bssid] {
				continue
			}
			p := Point{Latitude: float64(d.Entry.Lat) * 1e-7, Longitude: float64(d.Entry.Long) * 1e-7}
			if p.Validate() != nil {
				continue
			}
			n := uint64(d.Bssid)
			mac := fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", byte(n>>40), byte(n>>32), byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
			out = append(out, AccessPoint{BSSID: mac, Location: p})
			seen[d.Bssid] = true
		}
	}
	return out, nil
}
