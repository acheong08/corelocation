package corelocation

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

func TestTileKeyKnownVectors(t *testing.T) {
	tests := []struct {
		name   string
		point  Point
		zoom   int
		key    TileKey
		center Point
	}{
		{"world", Point{51.48, -3.27}, 0, 1, Point{0, 0}},
		{"northwest", Point{45, -90}, 1, 4, Point{66.51326044311186, -90}},
		{"northeast", Point{45, 90}, 1, 5, Point{66.51326044311186, 90}},
		{"southwest", Point{-45, -90}, 1, 6, Point{-66.51326044311186, -90}},
		{"southeast", Point{-45, 90}, 1, 7, Point{-66.51326044311186, 90}},
		{"zoom two", Point{30, -45}, 2, 19, Point{40.97989806962013, -45}},
		{"original Cardiff", Point{51.48, -3.27}, 13, 81644851, Point{51.46769695622337, -3.27392578125}},
		{"Cardiff city center", Point{51.4816, -3.1791}, 13, 81644853, Point{51.49506473014368, -3.18603515625}},
		{"zoom thirty equator", Point{0, 0}, 30, 2017612633061982208, Point{-0.00000016763806343078613, 0.00000016763806343078613}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, err := TileKeyFromPoint(tt.point, tt.zoom)
			if err != nil || key != tt.key {
				t.Fatalf("TileKeyFromPoint(%+v, %d) = %d, %v; want %d", tt.point, tt.zoom, key, err, tt.key)
			}
			zoom, err := key.Zoom()
			if err != nil || zoom != tt.zoom {
				t.Fatalf("Zoom() = %d, %v; want %d", zoom, err, tt.zoom)
			}
			center, err := key.Center()
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(center.Latitude-tt.center.Latitude) > 1e-11 || math.Abs(center.Longitude-tt.center.Longitude) > 1e-11 {
				t.Errorf("Center() = %+v; want %+v", center, tt.center)
			}
		})
	}
}

func TestTileKeyGeographicEndpoints(t *testing.T) {
	for zoom := 0; zoom <= 30; zoom++ {
		n := uint64(1) << uint(zoom)
		tests := []struct {
			point Point
			x, y  uint64
		}{
			{Point{85.0511287798066, -180}, 0, 0},
			{Point{85.0511287798066, 180}, n - 1, 0},
			{Point{-85.0511287798066, -180}, 0, n - 1},
			{Point{-85.0511287798066, 180}, n - 1, n - 1},
			{Point{math.Nextafter(85.0511287798066, 0), math.Nextafter(-180, 0)}, 0, 0},
			{Point{math.Nextafter(-85.0511287798066, 0), math.Nextafter(180, 0)}, n - 1, n - 1},
		}
		for _, tt := range tests {
			key, err := TileKeyFromPoint(tt.point, zoom)
			want := tileTestKey(tt.x, tt.y, zoom)
			if err != nil || key != want {
				t.Errorf("point %+v zoom %d: got %d, %v; want %d", tt.point, zoom, key, err, want)
			}
		}
	}
}

func TestTileKeyInteriorBoundaries(t *testing.T) {
	// Boundaries belong to the tile to their east/south. Check both sides of
	// the equator and exact longitude boundaries without unstable inverse
	// projections of non-equatorial latitude boundaries.
	for _, longitude := range []float64{-90, 0, 90} {
		for _, delta := range []float64{-1e-7, 0, 1e-7} {
			for _, latitude := range []float64{-1e-7, 0, 1e-7} {
				x := uint64((longitude + 180) / 90)
				if delta < 0 {
					x--
				}
				y := uint64(2)
				if latitude > 0 {
					y = 1
				}
				point := Point{latitude, longitude + delta}
				key, err := TileKeyFromPoint(point, 2)
				if want := tileTestKey(x, y, 2); err != nil || key != want {
					t.Errorf("point %+v: got %d, %v; want %d", point, key, err, want)
				}
			}
		}
	}
}

func TestTileKeyRejectsInvalidPointsAndZooms(t *testing.T) {
	points := []Point{
		{math.NaN(), 0}, {math.Inf(1), 0}, {math.Inf(-1), 0},
		{0, math.NaN()}, {0, math.Inf(1)}, {0, math.Inf(-1)},
		{90, 0}, {-90, 0}, {91, 0}, {-91, 0},
		{math.Nextafter(85.0511287798066, math.Inf(1)), 0},
		{math.Nextafter(-85.0511287798066, math.Inf(-1)), 0},
		{0, math.Nextafter(180, math.Inf(1))},
		{0, math.Nextafter(-180, math.Inf(-1))},
	}
	for i, point := range points {
		for _, zoom := range []int{0, 13, 30} {
			if key, err := TileKeyFromPoint(point, zoom); err == nil || key != 0 {
				t.Errorf("invalid point %d zoom %d: got %d, %v; want zero and error", i, zoom, key, err)
			}
		}
	}
	maxInt := int(^uint(0) >> 1)
	for _, zoom := range []int{-maxInt - 1, -1, 31, 32, 64, maxInt} {
		if key, err := TileKeyFromPoint(Point{}, zoom); err == nil || key != 0 {
			t.Errorf("invalid zoom %d: got %d, %v; want zero and error", zoom, key, err)
		}
	}
}

func TestTileKeyRejectsMalformedKeys(t *testing.T) {
	keys := []TileKey{0}
	for bit := 0; bit < 64; bit++ {
		if bit%2 == 1 || bit > 60 {
			sentinel := uint64(1) << uint(bit)
			keys = append(keys, TileKey(sentinel), TileKey(sentinel|(sentinel-1)))
		}
	}
	for _, key := range keys {
		t.Run(fmt.Sprintf("%d", key), func(t *testing.T) {
			if _, err := key.Zoom(); err == nil {
				t.Error("Zoom accepted malformed key")
			}
			if point, err := key.Center(); err == nil || point != (Point{}) {
				t.Errorf("Center() = %+v, %v; want zero point and error", point, err)
			}
		})
	}
}

func TestTileKeyRoundTrip(t *testing.T) {
	// Exhaust every payload at small zoom levels, then cover all zooms with
	// deterministic random payloads and extreme rows/columns.
	for zoom := 0; zoom <= 6; zoom++ {
		sentinel := uint64(1) << uint(2*zoom)
		for key := sentinel; key < 2*sentinel; key++ {
			tileTestRoundTrip(t, TileKey(key), zoom)
		}
	}
	rng := rand.New(rand.NewSource(81644851))
	for zoom := 0; zoom <= 30; zoom++ {
		sentinel := uint64(1) << uint(2*zoom)
		for i := 0; i < 100; i++ {
			tileTestRoundTrip(t, TileKey(sentinel|(rng.Uint64()&(sentinel-1))), zoom)
		}
		n := uint64(1) << uint(zoom)
		for _, x := range []uint64{0, n / 2, n - 1} {
			for _, y := range []uint64{0, n / 2, n - 1} {
				tileTestRoundTrip(t, tileTestKey(x, y, zoom), zoom)
			}
		}
	}
}

func TestTileKeyPointRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(30))
	for zoom := 0; zoom <= 30; zoom++ {
		for i := 0; i < 100; i++ {
			point := Point{(rng.Float64()*2 - 1) * 85.0511287798066, rng.Float64()*360 - 180}
			key, err := TileKeyFromPoint(point, zoom)
			if err != nil {
				t.Fatal(err)
			}
			tileTestRoundTrip(t, key, zoom)
			center, err := key.Center()
			if err != nil {
				t.Fatal(err)
			}
			// Check containment independently in normalized projected space.
			n := math.Ldexp(1, zoom)
			pointY := math.Asinh(math.Tan(point.Latitude * math.Pi / 180))
			centerY := math.Asinh(math.Tan(center.Latitude * math.Pi / 180))
			if math.Abs(point.Longitude-center.Longitude) > 180/n+1e-12 || math.Abs(pointY-centerY) > math.Pi/n+1e-12 {
				t.Fatalf("point %+v is outside tile %d centered at %+v, zoom %d", point, key, center, zoom)
			}
		}
	}
}

func tileTestRoundTrip(t *testing.T, key TileKey, wantZoom int) {
	t.Helper()
	zoom, err := key.Zoom()
	if err != nil || zoom != wantZoom {
		t.Fatalf("key %d Zoom() = %d, %v; want %d", key, zoom, err, wantZoom)
	}
	center, err := key.Center()
	if err != nil {
		t.Fatalf("key %d Center(): %v", key, err)
	}
	if err := center.Validate(); err != nil {
		t.Fatalf("key %d center invalid: %v", key, err)
	}
	if math.Abs(center.Latitude) >= 85.0511287798066 || math.Abs(center.Longitude) >= 180 {
		t.Fatalf("key %d center lies on or beyond geographic edge: %+v", key, center)
	}
	got, err := TileKeyFromPoint(center, zoom)
	if err != nil || got != key {
		t.Fatalf("key %d center %+v zoom %d roundtrip = %d, %v", key, center, zoom, got, err)
	}
}

// Build a test key by appending base-four Morton digits, independently of the
// production bit-position loop.
func tileTestKey(x, y uint64, zoom int) TileKey {
	key := uint64(1)
	for bit := zoom - 1; bit >= 0; bit-- {
		key = key*4 + ((x >> uint(bit)) & 1) + 2*((y>>uint(bit))&1)
	}
	return TileKey(key)
}
