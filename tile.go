package corelocation

import (
	"fmt"
	"math"
	"math/bits"
)

// TileKey identifies a Web Mercator tile using Morton order: column (x) bits
// occupy even positions, row (y) bits occupy odd positions, and a sentinel bit
// at position 2*zoom records the zoom level. Offline geometry supports zooms
// 0 to 30; network queries through FetchTile accept only WiFiTileZoom.
type TileKey uint64

const tileMercatorMaxLatitude = 85.0511287798066

// TileKeyFromPoint returns the tile containing p at zoom. Coordinates outside
// the Web Mercator latitude range are rejected rather than clamped. Longitude
// +180 belongs to the last column, not the first column across the antimeridian.
func TileKeyFromPoint(p Point, zoom int) (TileKey, error) {
	if zoom < 0 || zoom > 30 {
		return 0, fmt.Errorf("tile zoom %d is outside [0, 30]", zoom)
	}
	if err := p.Validate(); err != nil {
		return 0, fmt.Errorf("tile point: %w", err)
	}
	if p.Latitude < -tileMercatorMaxLatitude || p.Latitude > tileMercatorMaxLatitude {
		return 0, fmt.Errorf("tile latitude %g is outside Web Mercator range", p.Latitude)
	}

	n := uint64(1) << uint(zoom)
	xf := (p.Longitude + 180) / 360 * float64(n)
	latitudeRadians := p.Latitude * (math.Pi / 180)
	yf := (1 - math.Asinh(math.Tan(latitudeRadians))/math.Pi) / 2 * float64(n)
	// Geographic input has already been validated. Bound floating-point tile
	// coordinates to handle closed geographic endpoints and rounding there.
	x := uint64(math.Max(0, math.Min(float64(n-1), math.Floor(xf))))
	y := uint64(math.Max(0, math.Min(float64(n-1), math.Floor(yf))))

	key := uint64(1) << uint(2*zoom)
	for i := 0; i < zoom; i++ {
		key |= ((x >> uint(i)) & 1) << uint(2*i)
		key |= ((y >> uint(i)) & 1) << uint(2*i+1)
	}
	return TileKey(key), nil
}

// Zoom returns the zoom encoded by k, rejecting zero, an odd-position sentinel,
// or a zoom greater than 30. Every lower bit is part of the Morton payload.
func (k TileKey) Zoom() (int, error) {
	if k == 0 {
		return 0, fmt.Errorf("invalid tile key: zero has no sentinel")
	}
	sentinel := bits.Len64(uint64(k)) - 1
	if sentinel%2 != 0 || sentinel > 60 {
		return 0, fmt.Errorf("invalid tile key %d: sentinel bit %d", k, sentinel)
	}
	return sentinel / 2, nil
}

// Center returns the geographic center of k's tile, not its northwest corner.
func (k TileKey) Center() (Point, error) {
	zoom, err := k.Zoom()
	if err != nil {
		return Point{}, err
	}
	var x, y uint64
	for i := 0; i < zoom; i++ {
		x |= ((uint64(k) >> uint(2*i)) & 1) << uint(i)
		y |= ((uint64(k) >> uint(2*i+1)) & 1) << uint(i)
	}
	n := float64(uint64(1) << uint(zoom))
	return Point{
		Latitude:  math.Atan(math.Sinh(math.Pi*(1-2*(float64(y)+0.5)/n))) * (180 / math.Pi),
		Longitude: (float64(x)+0.5)/n*360 - 180,
	}, nil
}
