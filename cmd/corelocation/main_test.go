package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acheong08/corelocation"
	"github.com/acheong08/corelocation/internal/pb"
	"google.golang.org/protobuf/proto"
)

type fakeClient struct {
	wifi func(context.Context, corelocation.WiFiRequest) (corelocation.Result[corelocation.AccessPoint], error)
	cell func(context.Context, corelocation.CellRequest) (corelocation.Result[corelocation.Cell], error)
	tile func(context.Context, corelocation.TileKey) (corelocation.Result[corelocation.AccessPoint], error)
}

func (c fakeClient) LookupWiFi(ctx context.Context, r corelocation.WiFiRequest) (corelocation.Result[corelocation.AccessPoint], error) {
	return c.wifi(ctx, r)
}
func (c fakeClient) LookupCell(ctx context.Context, r corelocation.CellRequest) (corelocation.Result[corelocation.Cell], error) {
	return c.cell(ctx, r)
}
func (c fakeClient) FetchTile(ctx context.Context, k corelocation.TileKey) (corelocation.Result[corelocation.AccessPoint], error) {
	return c.tile(ctx, k)
}
func injected(c lookupClient) runConfig {
	return runConfig{NewClient: func(corelocation.Config) (lookupClient, error) { return c, nil }}
}
func noClient(t *testing.T) runConfig {
	t.Helper()
	return runConfig{NewClient: func(corelocation.Config) (lookupClient, error) {
		t.Error("unexpected client construction")
		return nil, errors.New("unexpected client")
	}}
}
func invoke(t *testing.T, args []string, cfg runConfig) (int, string, string) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	code := run(context.Background(), args, &out, &diagnostic, cfg)
	return code, out.String(), diagnostic.String()
}
func decode[T any](t *testing.T, text string) T {
	t.Helper()
	var value T
	decoder := json.NewDecoder(strings.NewReader(text))
	if err := decoder.Decode(&value); err != nil {
		t.Fatalf("invalid JSON %q: %v", text, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout has extra content: %q (%v)", text, err)
	}
	return value
}

func TestHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"help", "wifi"}, {"wifi", "--help"}, {"cell", "-h"}, {"tile", "--help"}, {"tile-key", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, diagnostic := invoke(t, args, noClient(t))
			if code != 0 || diagnostic != "" || !strings.Contains(out, "Usage:") {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, out, diagnostic)
			}
		})
	}
}

func TestRejectsInputBeforeClientConstruction(t *testing.T) {
	wifi := "wifi --bssid 02:11:22:33:44:55"
	cell := "cell --mcc 460 --mnc 0 --cell-id 0 --tac 0"
	cases := []string{
		"", "unknown", "--region china wifi", "--help extra", "wifi", "wifi --bssid invalid", "wifi --bssid 00:11:22:33:44:55:66:77",
		wifi + " --bssid invalid", wifi + " extra", wifi + " --unknown", wifi + " --lat 0", wifi + " --lon 0", wifi + " --lat NaN --lon 0",
		wifi + " --lat 0 --lat 1 --lon 0", wifi + " --lat 0 --lon 0 --lon 1",
		wifi + " --lat 0 --lon +Inf", wifi + " --lat 91 --lon 0", wifi + " --lat 0 --lon -181", wifi + " --limit -1",
		wifi + " --limit 2147483648", wifi + " --limit 9223372036854775808", wifi + " --timeout -1s", wifi + " --timeout 0",
		wifi + " --timeout invalid", wifi + " --attempt-timeout -1s", wifi + " --attempt-timeout 0", wifi + " --region bogus",
		"cell --mcc 460 --mnc 0 --cell-id 0", "cell --mcc 460 --mnc 0 --tac 0", "cell --mcc 460 --cell-id 0 --tac 0", "cell --mnc 0 --cell-id 0 --tac 0",
		strings.Replace(cell, "--mcc 460", "--mcc 0", 1), strings.Replace(cell, "--mcc 460", "--mcc 1000", 1),
		strings.Replace(cell, "--mnc 0", "--mnc 1000", 1), strings.Replace(cell, "--cell-id 0", "--cell-id 268435456", 1),
		strings.Replace(cell, "--tac 0", "--tac 65536", 1), strings.Replace(cell, "--tac 0", "--tac -1", 1),
		strings.Replace(cell, "--mcc 460", "--mcc 4294967756", 1), strings.Replace(cell, "--mnc 0", "--mnc 18446744073709551616", 1),
		cell + " --lat 0", cell + " --limit 2147483648", "tile", "tile --key 0", "tile --key 2", "tile --key 0x10", "tile --key -1",
		"tile --key 18446744073709551616", "tile --key 18446744073709551615", "tile --key 1 --zoom 13", "tile --key 1 --lat 0 --lon 0",
		"tile --key 1 --lat 0", "tile --lat 0", "tile --lat 0 --lon 0 --zoom -1", "tile --lat 0 --lon 0 --zoom 31",
		"tile --lat 90 --lon 0", "tile --lat NaN --lon 0", "tile-key", "tile-key --lat 0 --lon 0", "tile-key --key 1 --zoom 0",
	}
	for _, command := range cases {
		t.Run(command, func(t *testing.T) {
			code, out, diagnostic := invoke(t, strings.Fields(command), noClient(t))
			problem := decode[struct {
				Error string `json:"error"`
			}](t, out)
			if code != 2 || diagnostic == "" || problem.Error == "" {
				t.Fatalf("code=%d stdout=%s stderr=%s", code, out, diagnostic)
			}
		})
	}
	// An explicitly empty value must not be mistaken for an omitted flag.
	code, out, _ := invoke(t, []string{"tile", "--key", ""}, noClient(t))
	if code != 2 || decode[struct{ Error string }](t, out).Error == "" {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

func TestWiFiRequestAndConfiguration(t *testing.T) {
	var gotConfig corelocation.Config
	called := false
	fake := fakeClient{wifi: func(ctx context.Context, r corelocation.WiFiRequest) (corelocation.Result[corelocation.AccessPoint], error) {
		called = true
		if !reflect.DeepEqual(r.BSSIDs, []string{"aa:bb:cc:dd:ee:ff", "02:11:22:33:44:55"}) || r.MaxResults != 2147483647 || r.Hint == nil || *r.Hint != (corelocation.Point{}) {
			t.Errorf("request=%+v", r)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 3*time.Second {
			t.Errorf("deadline=%v ok=%v", deadline, ok)
		}
		return corelocation.Result[corelocation.AccessPoint]{Records: []corelocation.AccessPoint{{BSSID: r.BSSIDs[0], Location: corelocation.Point{Latitude: 1, Longitude: 2}}}, Region: corelocation.China, Attempts: []corelocation.Attempt{{Region: corelocation.China}}}, nil
	}}
	cfg := runConfig{ClientConfig: corelocation.Config{MaxResponseBytes: 12345}, NewClient: func(c corelocation.Config) (lookupClient, error) { gotConfig = c; return fake, nil }}
	args := strings.Fields("wifi --bssid AA-BB-CC-DD-EE-FF --bssid 0211.2233.4455 --lat 0 --lon 0 --limit 2147483647 --region china --no-fallback --timeout 3s --attempt-timeout 2s")
	code, out, diagnostic := invoke(t, args, cfg)
	result := decode[lookupOutput[corelocation.AccessPoint]](t, out)
	if code != 0 || diagnostic != "" || !called || len(result.Records) != 1 || len(result.Attempts) != 1 || result.Error != "" || result.Region != corelocation.China {
		t.Fatalf("code=%d result=%+v stderr=%s", code, result, diagnostic)
	}
	if gotConfig.Region != corelocation.China || !gotConfig.DisableFallback || gotConfig.AttemptTimeout != 2*time.Second || gotConfig.MaxResponseBytes != 12345 {
		t.Fatalf("config=%+v", gotConfig)
	}
}

func TestCellZeroIDsAndDefaults(t *testing.T) {
	fake := fakeClient{cell: func(ctx context.Context, r corelocation.CellRequest) (corelocation.Result[corelocation.Cell], error) {
		if r.Tower != (corelocation.Tower{MCC: 460}) || r.MaxResults != 0 || r.Hint != nil {
			t.Errorf("request=%+v", r)
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 19*time.Second || time.Until(deadline) > 20*time.Second {
			t.Errorf("deadline=%v", deadline)
		}
		return corelocation.Result[corelocation.Cell]{Records: []corelocation.Cell{{Tower: r.Tower}}, Region: corelocation.International, Attempts: []corelocation.Attempt{{Region: corelocation.International}}}, nil
	}}
	cfg := runConfig{NewClient: func(c corelocation.Config) (lookupClient, error) {
		if c.Region != corelocation.Auto || c.DisableFallback || c.AttemptTimeout != 10*time.Second {
			t.Errorf("config=%+v", c)
		}
		return fake, nil
	}}
	code, out, diagnostic := invoke(t, strings.Fields("cell --mcc 460 --mnc 0 --cell-id 0 --tac 0"), cfg)
	result := decode[lookupOutput[corelocation.Cell]](t, out)
	if code != 0 || diagnostic != "" || len(result.Records) != 1 || result.Records[0].Tower.MCC != 460 {
		t.Fatalf("code=%d out=%s stderr=%s", code, out, diagnostic)
	}
}

func TestCellDecimalIdentifiers(t *testing.T) {
	fake := fakeClient{cell: func(_ context.Context, r corelocation.CellRequest) (corelocation.Result[corelocation.Cell], error) {
		if r.Tower != (corelocation.Tower{MCC: 460, MNC: 8, CellID: 123, TAC: 9}) {
			t.Errorf("request=%+v", r)
		}
		if r.Hint == nil || *r.Hint != (corelocation.Point{Latitude: 1, Longitude: 2}) || r.MaxResults != 3 {
			t.Errorf("request=%+v", r)
		}
		return corelocation.Result[corelocation.Cell]{}, nil
	}}
	code, out, diagnostic := invoke(t, strings.Fields("cell --mcc 0460 --mnc 08 --cell-id 0123 --tac 09 --lat 1 --lon 2 --limit 3"), injected(fake))
	if code != 0 || diagnostic != "" {
		t.Fatalf("code=%d out=%s stderr=%s", code, out, diagnostic)
	}
}

func TestTileModesAndOfflineRoundTrip(t *testing.T) {
	key, err := corelocation.TileKeyFromPoint(corelocation.Point{Latitude: 39.9, Longitude: 116.4}, 13)
	if err != nil {
		t.Fatal(err)
	}
	keyString := strconv.FormatUint(uint64(key), 10)
	fake := fakeClient{tile: func(_ context.Context, got corelocation.TileKey) (corelocation.Result[corelocation.AccessPoint], error) {
		if got != key {
			t.Errorf("key=%d want=%d", got, key)
		}
		return corelocation.Result[corelocation.AccessPoint]{}, nil
	}}
	for _, command := range []string{"tile --lat 39.9 --lon 116.4", "tile --lat 39.9 --lon 116.4 --zoom 13", "tile --key " + keyString} {
		code, out, diagnostic := invoke(t, strings.Fields(command), injected(fake))
		if code != 0 || diagnostic != "" || out != "{\"records\":[],\"attempts\":[]}\n" {
			t.Fatalf("code=%d out=%s stderr=%s", code, out, diagnostic)
		}
	}
	code, encoded, diagnostic := invoke(t, strings.Fields("tile-key --lat 39.9 --lon 116.4 --zoom 13"), noClient(t))
	result := decode[tileKeyOutput](t, encoded)
	if code != 0 || diagnostic != "" || result.Key != keyString || result.Zoom != 13 {
		t.Fatalf("code=%d out=%s stderr=%s", code, encoded, diagnostic)
	}
	code, decoded, diagnostic := invoke(t, []string{"tile-key", "--key", result.Key}, noClient(t))
	if code != 0 || diagnostic != "" || decoded != encoded {
		t.Fatalf("roundtrip code=%d encoded=%s decoded=%s stderr=%s", code, encoded, decoded, diagnostic)
	}
	// Largest supported zoom needs more than JavaScript's exact integer range.
	code, out, _ := invoke(t, strings.Fields("tile-key --lat 0 --lon 0 --zoom 30"), noClient(t))
	if code != 0 || decode[tileKeyOutput](t, out).Zoom != 30 {
		t.Fatalf("out=%s code=%d", out, code)
	}
}

func TestLookupErrorPreservesRecordsAndAttempts(t *testing.T) {
	attempts := []corelocation.Attempt{{Region: corelocation.China, Error: "first failure"}, {Region: corelocation.International, Error: "second failure"}}
	fake := fakeClient{wifi: func(context.Context, corelocation.WiFiRequest) (corelocation.Result[corelocation.AccessPoint], error) {
		return corelocation.Result[corelocation.AccessPoint]{Records: []corelocation.AccessPoint{{BSSID: "02:11:22:33:44:55"}}, Attempts: attempts}, errors.New("lookup failed")
	}}
	code, out, diagnostic := invoke(t, strings.Fields("wifi --bssid 02:11:22:33:44:55"), injected(fake))
	result := decode[lookupOutput[corelocation.AccessPoint]](t, out)
	if code != 1 || diagnostic == "" || result.Error != "lookup failed" || len(result.Records) != 1 || !reflect.DeepEqual(result.Attempts, attempts) {
		t.Fatalf("code=%d result=%+v stderr=%s", code, result, diagnostic)
	}
}

func TestLocalHTTPFallbackAndPin(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	cfg := runConfig{ClientConfig: corelocation.Config{HTTPClient: server.Client(), Endpoints: corelocation.Endpoints{WLocInternational: server.URL + "/international", WLocChina: server.URL + "/china", TileInternational: server.URL + "/international", TileChina: server.URL + "/china"}}}
	for _, command := range []string{"wifi --bssid 02:11:22:33:44:55 --region china", "cell --mcc 460 --mnc 0 --cell-id 0 --tac 0", "tile --lat 39.9 --lon 116.4"} {
		for _, pinned := range []bool{false, true} {
			t.Run(command+strconv.FormatBool(pinned), func(t *testing.T) {
				mu.Lock()
				paths = nil
				mu.Unlock()
				args := strings.Fields(command)
				if pinned {
					args = append(args, "--no-fallback")
				}
				code, out, diagnostic := invoke(t, args, cfg)
				result := decode[struct {
					Records  []json.RawMessage      `json:"records"`
					Attempts []corelocation.Attempt `json:"attempts"`
					Error    string                 `json:"error"`
				}](t, out)
				want := []string{"/china", "/international"}
				if pinned {
					want = want[:1]
				}
				mu.Lock()
				gotPaths := append([]string(nil), paths...)
				mu.Unlock()
				if code != 1 || diagnostic == "" || result.Error == "" || result.Records == nil || len(result.Attempts) != len(want) || !reflect.DeepEqual(gotPaths, want) {
					t.Fatalf("code=%d out=%s stderr=%s paths=%v", code, out, diagnostic, gotPaths)
				}
				for _, a := range result.Attempts {
					if !strings.Contains(a.Error, "503") {
						t.Errorf("attempt=%+v", a)
					}
				}
			})
		}
	}
}

func TestLocalHTTPSuccess(t *testing.T) {
	lat, lon := int64(123000000), int64(456000000)
	data, err := proto.Marshal(&pb.AppleWLoc{WifiDevices: []*pb.WifiDevice{{Bssid: "02:11:22:33:44:55", Location: &pb.Location{Latitude: &lat, Longitude: &lon}}}})
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 10+len(data))
	binary.BigEndian.PutUint16(frame, 1)
	binary.BigEndian.PutUint32(frame[2:], 1)
	binary.BigEndian.PutUint32(frame[6:], uint32(len(data)))
	copy(frame[10:], data)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method=%s", r.Method)
		}
		_, _ = w.Write(frame)
	}))
	defer server.Close()
	cfg := runConfig{ClientConfig: corelocation.Config{HTTPClient: server.Client(), Endpoints: corelocation.Endpoints{WLocInternational: server.URL, WLocChina: server.URL}}}
	code, out, diagnostic := invoke(t, strings.Fields("wifi --bssid 02:11:22:33:44:55"), cfg)
	result := decode[lookupOutput[corelocation.AccessPoint]](t, out)
	if code != 0 || diagnostic != "" || result.Error != "" || len(result.Records) != 1 || math.Abs(result.Records[0].Location.Latitude-1.23) > 1e-12 || math.Abs(result.Records[0].Location.Longitude-4.56) > 1e-12 || len(result.Attempts) != 1 {
		t.Fatalf("code=%d out=%s stderr=%s", code, out, diagnostic)
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	fake := fakeClient{wifi: func(ctx context.Context, _ corelocation.WiFiRequest) (corelocation.Result[corelocation.AccessPoint], error) {
		<-ctx.Done()
		return corelocation.Result[corelocation.AccessPoint]{}, ctx.Err()
	}}
	for _, canceled := range []bool{false, true} {
		t.Run(strconv.FormatBool(canceled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				cancel()
			}
			var out, diagnostic bytes.Buffer
			code := run(ctx, strings.Fields("wifi --bssid 02:11:22:33:44:55 --timeout 1ms"), &out, &diagnostic, injected(fake))
			result := decode[lookupOutput[corelocation.AccessPoint]](t, out.String())
			want := context.DeadlineExceeded.Error()
			if canceled {
				want = context.Canceled.Error()
			}
			if code != 1 || result.Error != want || result.Attempts == nil {
				t.Fatalf("code=%d result=%+v", code, result)
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken output") }
func TestClientAndOutputErrors(t *testing.T) {
	cfg := runConfig{NewClient: func(corelocation.Config) (lookupClient, error) { return nil, errors.New("invalid injected config") }}
	code, out, diagnostic := invoke(t, strings.Fields("wifi --bssid 02:11:22:33:44:55"), cfg)
	if code != 1 || diagnostic == "" || decode[struct{ Error string }](t, out).Error != "invalid injected config" {
		t.Fatalf("code=%d out=%s stderr=%s", code, out, diagnostic)
	}
	for _, command := range []string{"tile-key --key 1", "invalid", "wifi --bssid 02:11:22:33:44:55"} {
		var diagnostic bytes.Buffer
		fake := fakeClient{wifi: func(context.Context, corelocation.WiFiRequest) (corelocation.Result[corelocation.AccessPoint], error) {
			return corelocation.Result[corelocation.AccessPoint]{}, nil
		}}
		code := run(context.Background(), strings.Fields(command), failingWriter{}, &diagnostic, injected(fake))
		if code != 1 || !strings.Contains(diagnostic.String(), "broken output") {
			t.Fatalf("code=%d stderr=%s", code, diagnostic.String())
		}
	}
}
