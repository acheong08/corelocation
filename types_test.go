package corelocation

import (
	"math"
	"testing"
)

func TestPreferredRegion(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    Point
		want Region
	}{
		{"Beijing", Point{39.9, 116.4}, China},
		{"Shanghai", Point{31.23, 121.47}, China},
		{"Urumqi", Point{43.8, 87.6}, China},
		{"Lhasa", Point{29.65, 91.1}, China},
		{"Hainan", Point{18.25, 109.5}, China},
		{"Hong Kong", Point{22.32, 114.17}, International},
		{"Macau", Point{22.19, 113.54}, International},
		{"Taipei", Point{25.03, 121.56}, International},
		{"London", Point{51.5, -.1}, International},
		{"Tokyo false positive", Point{35.68, 139.69}, International},
		{"Seoul tolerated false positive", Point{37.56, 126.98}, China},
		{"bounds minimum", Point{18, 73}, China},
		{"bounds maximum", Point{54, 135}, China},
		{"just outside", Point{54.001, 135}, International},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := PreferredRegion(&tt.p, 0); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
		})
	}
	if PreferredRegion(nil, 460) != China || PreferredRegion(nil, 234) != International || PreferredRegion(nil, 0) != International {
		t.Fatal("MCC routing failed")
	}
	p := Point{51.5, -.1}
	if PreferredRegion(&p, 460) != International {
		t.Fatal("hint must take priority")
	}
	if PreferChina(Point{math.NaN(), 100}) {
		t.Fatal("accepted NaN")
	}
}
func TestNormalizeBSSID(t *testing.T) {
	for _, s := range []string{"AA:BB:CC:DD:EE:FF", "aa-bb-cc-dd-ee-ff", "aabb.ccdd.eeff", " aa:bb:cc:dd:ee:ff "} {
		got, err := NormalizeBSSID(s)
		if err != nil || got != "aa:bb:cc:dd:ee:ff" {
			t.Fatalf("%q: %q %v", s, got, err)
		}
	}
	if got, err := NormalizeBSSID("0:1:2:a:B:f"); err != nil || got != "00:01:02:0a:0b:0f" {
		t.Fatalf("short octets: %q %v", got, err)
	}
	for _, s := range []string{"", "aa:bb:cc:dd:ee", "aa:bb:cc:dd:ee:ff:00:11", "gg:bb:cc:dd:ee:ff", "+1:2:3:4:5:6", "001:2:3:4:5:6", "0::2:3:4:5"} {
		if _, err := NormalizeBSSID(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func TestPointValidation(t *testing.T) {
	for _, p := range []Point{{90, 180}, {-90, -180}, {0, 0}} {
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []Point{{91, 0}, {0, 181}, {math.Inf(1), 0}, {0, math.NaN()}} {
		if err := p.Validate(); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}
func TestTowerValidation(t *testing.T) {
	for _, tower := range []Tower{{MCC: 1}, {MCC: 999, MNC: 999, CellID: 0xfffffff, TAC: 0xffff}} {
		if err := tower.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, tower := range []Tower{{}, {MCC: 1000}, {MCC: 1, MNC: 1000}, {MCC: 1, CellID: 0x10000000}, {MCC: 1, TAC: 0x10000}} {
		if err := tower.Validate(); err == nil {
			t.Fatalf("accepted %+v", tower)
		}
	}
}
