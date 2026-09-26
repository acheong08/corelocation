package corelocation

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"net/http"
	"reflect"
	"testing"

	"github.com/acheong08/corelocation/internal/pb"
	"google.golang.org/protobuf/proto"
)

// Decode requests independently of the production encoder so tests catch
// envelope and optional-field regressions, not just self-consistent round trips.
func requestBlock(t *testing.T, r *http.Request) *pb.AppleWLoc {
	t.Helper()
	if r.Method != "POST" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Header.Get("User-Agent") == "" {
		t.Errorf("bad request headers %v", r.Header)
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	reader := bytes.NewReader(data)
	var version uint16
	if err = binary.Read(reader, binary.BigEndian, &version); err != nil || version != 1 {
		t.Fatalf("version %d err %v", version, err)
	}
	for _, want := range []string{"en-001_001", "com.apple.locationd", "18.6.2.22G100"} {
		var n uint16
		if err = binary.Read(reader, binary.BigEndian, &n); err != nil {
			t.Fatal(err)
		}
		s := make([]byte, n)
		if _, err = io.ReadFull(reader, s); err != nil {
			t.Fatal(err)
		}
		if string(s) != want {
			t.Fatalf("ARPC string %q want %q", s, want)
		}
	}
	var function, n uint32
	if err = binary.Read(reader, binary.BigEndian, &function); err != nil || function != 1 {
		t.Fatalf("function %d err %v", function, err)
	}
	if err = binary.Read(reader, binary.BigEndian, &n); err != nil || int(n) != reader.Len() {
		t.Fatalf("length %d remaining %d err %v", n, reader.Len(), err)
	}
	payload := make([]byte, n)
	if _, err = io.ReadFull(reader, payload); err != nil {
		t.Fatal(err)
	}
	var block pb.AppleWLoc
	if err = proto.Unmarshal(payload, &block); err != nil {
		t.Fatal(err)
	}
	return &block
}
func TestWiFiWireAndFiltering(t *testing.T) {
	good := &pb.Location{Latitude: ptr(int64(0)), Longitude: ptr(int64(0))}
	missing := &pb.Location{Latitude: ptr(int64(0))}
	sentinel := &pb.Location{Latitude: ptr(int64(-18000000000)), Longitude: ptr(int64(-18000000000))}
	c := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		block := requestBlock(t, r)
		if len(block.WifiDevices) != 1 || block.WifiDevices[0].Bssid != "aa:bb:cc:dd:ee:ff" || block.NumCellResults == nil || *block.NumCellResults != 0 || block.NumWifiResults == nil || *block.NumWifiResults != 7 || block.DeviceType == nil {
			t.Errorf("bad block %v", block)
		}
		w.Write(frame(t, &pb.AppleWLoc{WifiDevices: []*pb.WifiDevice{
			{Bssid: "02:11:22:33:44:55", Location: good},
			{Bssid: "02:11:22:33:44:55", Location: good},
			{Bssid: "02:11:22:33:44:56", Location: missing},
			{Bssid: "02:11:22:33:44:57", Location: sentinel},
			{Bssid: "02:11:22:33:44:58"},
			{Bssid: "invalid", Location: good},
		}}))
	}, nil)
	result, err := c.LookupWiFi(context.Background(), WiFiRequest{BSSIDs: []string{"AA-BB-CC-DD-EE-FF", "aa:bb:cc:dd:ee:ff"}, MaxResults: 7, Hint: &Point{1, 2}})
	if err != nil || len(result.Records) != 1 || result.Records[0].Location != (Point{}) {
		t.Fatalf("result %+v err %v", result, err)
	}
}
func TestCellWireAndMCCRouting(t *testing.T) {
	tower := Tower{MCC: 460, MNC: 0, CellID: 123, TAC: 0}
	paths := []string{}
	c := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		block := requestBlock(t, r)
		want := &pb.CellTower{Mcc: 460, Mnc: 0, CellId: 123, TacId: 0}
		if !proto.Equal(block.CellTowerRequest, want) || block.NumWifiResults == nil || *block.NumWifiResults != -1 || block.NumCellResults == nil || *block.NumCellResults != 0 {
			t.Errorf("bad cell block %v", block)
		}
		if len(paths) == 1 {
			w.Write(frame(t, &pb.AppleWLoc{CellTowerResponse: []*pb.CellTower{{Mcc: 460, CellId: 123}}}))
			return
		}
		want.Location = &pb.Location{Latitude: ptr(int64(3123000000)), Longitude: ptr(int64(12147000000))}
		w.Write(frame(t, &pb.AppleWLoc{CellTowerResponse: []*pb.CellTower{want, want, {Location: want.Location}}}))
	}, nil)
	result, err := c.LookupCell(context.Background(), CellRequest{Tower: tower})
	if err != nil || len(result.Records) != 1 || result.Records[0].Tower != tower || !reflect.DeepEqual(paths, []string{"/china", "/international"}) {
		t.Fatalf("paths %v result %+v err %v", paths, result, err)
	}
}
func TestTileWireAndFiltering(t *testing.T) {
	key, err := TileKeyFromPoint(Point{39.9, 116.4}, 13)
	if err != nil {
		t.Fatal(err)
	}
	tile := &pb.WifiTile{Region: []*pb.WifiTile_Region{{Devices: []*pb.WifiTile_Device{
		{Bssid: 0x021122334455, Entry: &pb.WifiTile_TileLocation{Lat: 399000000, Long: 1164000000}},
		{Bssid: 0x021122334455, Entry: &pb.WifiTile_TileLocation{}},
		{Bssid: 0x021122334456},
		{Bssid: 0x021122334457, Entry: &pb.WifiTile_TileLocation{Lat: -1800000000}},
		{Bssid: 1 << 48, Entry: &pb.WifiTile_TileLocation{}},
		{Bssid: -1, Entry: &pb.WifiTile_TileLocation{}},
		{Bssid: 0, Entry: &pb.WifiTile_TileLocation{}},
	}}}}
	data, err := proto.Marshal(tile)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	c := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method != "GET" || r.Header.Get("X-tilekey") == "" || r.Header.Get("X-os-version") == "" {
			t.Errorf("bad tile request %+v", r)
		}
		if len(paths) == 1 {
			w.WriteHeader(404)
			return
		}
		w.Write(data)
	}, nil)
	result, err := c.FetchTile(context.Background(), key)
	if err != nil || len(result.Records) != 1 || result.Records[0].BSSID != "02:11:22:33:44:55" || math.Abs(result.Records[0].Location.Latitude-39.9) > 1e-7 || !reflect.DeepEqual(paths, []string{"/china", "/international"}) {
		t.Fatalf("paths %v result %+v err %v", paths, result, err)
	}
}
func TestTileMalformedAndEmpty(t *testing.T) {
	for _, data := range [][]byte{nil, {0xff}} {
		calls := 0
		c := localClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.Write(data) }, nil)
		_, err := c.FetchTile(context.Background(), 1)
		if len(data) == 0 {
			if calls != 2 || !errors.Is(err, ErrNoResults) {
				t.Fatalf("calls %d err %v", calls, err)
			}
		} else if calls != 1 || !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("calls %d err %v", calls, err)
		}
	}
}
func FuzzDecodeWLoc(f *testing.F) {
	f.Add([]byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = decodeWLoc(data) })
}
func FuzzDecodeTile(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xff})
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = decodeTile(data) })
}
