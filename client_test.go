package corelocation

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acheong08/corelocation/internal/pb"
	"google.golang.org/protobuf/proto"
)

func ptr[T any](v T) *T { return &v }
func frame(t *testing.T, msg *pb.AppleWLoc) []byte {
	t.Helper()
	b, err := proto.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	out := binary.BigEndian.AppendUint16(nil, 1)
	out = binary.BigEndian.AppendUint32(out, 1)
	out = binary.BigEndian.AppendUint32(out, uint32(len(b)))
	return append(out, b...)
}
func wifiFrame(t *testing.T) []byte {
	return frame(t, &pb.AppleWLoc{WifiDevices: []*pb.WifiDevice{{Bssid: "02:11:22:33:44:55", Location: &pb.Location{Latitude: ptr(int64(3990000000)), Longitude: ptr(int64(11640000000))}}}})
}
func localClient(t *testing.T, handler http.HandlerFunc, modify func(*Config)) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	cfg := Config{Endpoints: Endpoints{WLocInternational: s.URL + "/international", WLocChina: s.URL + "/china", TileInternational: s.URL + "/international", TileChina: s.URL + "/china"}}
	if modify != nil {
		modify(&cfg)
	}
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var wifiReq = WiFiRequest{BSSIDs: []string{"02:11:22:33:44:55"}}

func TestRoutingAndFallback(t *testing.T) {
	tests := []struct {
		name     string
		region   Region
		hint     *Point
		disabled bool
		want     []string
	}{
		{"no hint", Auto, nil, false, []string{"/international", "/china"}},
		{"Beijing", Auto, &Point{39.9, 116.4}, false, []string{"/china", "/international"}},
		{"London", Auto, &Point{51.5, -.1}, false, []string{"/international", "/china"}},
		{"force China", China, &Point{51.5, -.1}, false, []string{"/china", "/international"}},
		{"pin international", International, &Point{39.9, 116.4}, true, []string{"/international"}},
		{"pin China", China, nil, true, []string{"/china"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			data := wifiFrame(t)
			c := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if len(paths) == 1 {
					w.Write(frame(t, &pb.AppleWLoc{}))
					return
				}
				w.Write(data)
			}, func(cfg *Config) { cfg.Region = tt.region; cfg.DisableFallback = tt.disabled })
			req := wifiReq
			req.Hint = tt.hint
			result, err := c.LookupWiFi(context.Background(), req)
			if !reflect.DeepEqual(paths, tt.want) {
				t.Fatalf("paths %v want %v", paths, tt.want)
			}
			if len(result.Attempts) != len(tt.want) {
				t.Fatalf("attempts %+v", result.Attempts)
			}
			if tt.disabled {
				if !errors.Is(err, ErrNoResults) {
					t.Fatalf("want no results, got %v", err)
				}
			} else if err != nil || len(result.Records) != 1 || result.Region != Region(strings.TrimPrefix(tt.want[1], "/")) {
				t.Fatalf("result %+v err %v", result, err)
			}
		})
	}
}
func TestFallbackStatuses(t *testing.T) {
	for _, status := range []int{204, 301, 400, 401, 403, 404, 408, 429, 500, 502, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			c := localClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(status)
			}, nil)
			result, err := c.LookupWiFi(context.Background(), wifiReq)
			want := 1
			if status == 404 || status == 408 || status == 429 || status >= 500 {
				want = 2
			}
			if calls != want || len(result.Attempts) != want {
				t.Fatalf("calls %d attempts %d want %d", calls, len(result.Attempts), want)
			}
			var he *HTTPError
			if !errors.As(err, &he) || he.StatusCode != status || he.RetryAfter != "60" {
				t.Fatalf("lost HTTP status: %v", err)
			}
		})
	}
}
func TestNoFallbackOnUsablePartialResult(t *testing.T) {
	calls := 0
	c := localClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.Write(wifiFrame(t)) }, nil)
	req := wifiReq
	req.BSSIDs = append(append([]string{}, req.BSSIDs...), "02:aa:bb:cc:dd:ee")
	result, err := c.LookupWiFi(context.Background(), req)
	if err != nil || calls != 1 || len(result.Records) != 1 {
		t.Fatalf("result %+v calls %d err %v", result, calls, err)
	}
}
func TestMalformedAndOversizedResponses(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		max  int64
		want error
	}{
		{"short", []byte{1, 2}, 0, ErrInvalidResponse},
		{"bad version", make([]byte, 10), 0, ErrInvalidResponse},
		{"bad length", []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 1}, 0, ErrInvalidResponse},
		{"bad protobuf", []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0xff}, 0, ErrInvalidResponse},
		{"oversized", wifiFrame(t), 10, ErrResponseTooLarge},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			c := localClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.Write(tt.data) }, func(cfg *Config) { cfg.MaxResponseBytes = tt.max })
			_, err := c.LookupWiFi(context.Background(), wifiReq)
			if !errors.Is(err, tt.want) || calls != 1 {
				t.Fatalf("calls %d err %v", calls, err)
			}
		})
	}
}
func TestCancellationAndAttemptTimeout(t *testing.T) {
	t.Run("already cancelled", func(t *testing.T) {
		var calls atomic.Int32
		c := localClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := c.LookupWiFi(ctx, wifiReq)
		if !errors.Is(err, context.Canceled) || calls.Load() != 0 || len(result.Attempts) != 0 {
			t.Fatalf("result %+v err %v", result, err)
		}
	})
	t.Run("parent deadline prevents fallback", func(t *testing.T) {
		var calls atomic.Int32
		c := localClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		}, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		result, err := c.LookupWiFi(ctx, wifiReq)
		if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 || len(result.Attempts) != 1 {
			t.Fatalf("calls %d result %+v err %v", calls.Load(), result, err)
		}
	})
	t.Run("attempt deadline allows fallback", func(t *testing.T) {
		data := wifiFrame(t)
		c := localClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/international" {
				io.Copy(io.Discard, r.Body)
				<-r.Context().Done()
				return
			}
			w.Write(data)
		}, func(cfg *Config) { cfg.AttemptTimeout = 30 * time.Millisecond })
		result, err := c.LookupWiFi(context.Background(), wifiReq)
		if err != nil || result.Region != China || len(result.Attempts) != 2 {
			t.Fatalf("result %+v err %v", result, err)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTransportFailureFallback(t *testing.T) {
	failure := errors.New("synthetic connection failure")
	calls := 0
	c, err := NewClient(Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, failure })}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.LookupWiFi(context.Background(), wifiReq)
	if calls != 2 || len(result.Attempts) != 2 || !errors.Is(err, failure) {
		t.Fatalf("result %+v err %v", result, err)
	}
}
func TestRedirectIsNotFollowed(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	c := localClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}, nil)
	_, err := c.LookupWiFi(context.Background(), wifiReq)
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != 307 || targetCalls.Load() != 0 {
		t.Fatalf("redirect followed or status lost: %v", err)
	}
}
func TestValidationBeforeIO(t *testing.T) {
	var calls atomic.Int32
	c := localClient(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }, nil)
	for _, req := range []WiFiRequest{{}, {BSSIDs: []string{"bad"}}, {BSSIDs: wifiReq.BSSIDs, MaxResults: -1}, {BSSIDs: wifiReq.BSSIDs, Hint: &Point{91, 0}}} {
		if _, err := c.LookupWiFi(context.Background(), req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
	for _, req := range []CellRequest{{}, {Tower: Tower{MCC: 460, MNC: 1000}}, {Tower: Tower{MCC: 460}, MaxResults: -1}} {
		if _, err := c.LookupCell(context.Background(), req); err == nil {
			t.Fatalf("accepted %+v", req)
		}
	}
	if _, err := c.FetchTile(context.Background(), 0); err == nil {
		t.Fatal("accepted invalid tile")
	}
	for zoom := 0; zoom <= 30; zoom++ {
		if zoom == WiFiTileZoom {
			continue
		}
		key, err := TileKeyFromPoint(Point{51.48, -3.18}, zoom)
		if err != nil {
			t.Fatal(err)
		}
		result, err := c.FetchTile(context.Background(), key)
		if err == nil || !strings.Contains(err.Error(), "require zoom 13") || len(result.Attempts) != 0 {
			t.Fatalf("zoom %d: expected validation error without attempts, got %+v, %v", zoom, result, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("validation performed IO")
	}
}
func TestConfigValidation(t *testing.T) {
	for _, cfg := range []Config{{Region: "wrong"}, {AttemptTimeout: -1}, {MaxResponseBytes: -1}, {MaxResponseBytes: 1<<63 - 1}, {Endpoints: Endpoints{WLocChina: "file:///tmp/no"}}, {Endpoints: Endpoints{WLocChina: "https://user:secret@example.com"}}, {Endpoints: Endpoints{TileChina: "http://example.com/#fragment"}}} {
		if _, err := NewClient(cfg); err == nil {
			t.Fatalf("accepted %+v", cfg)
		}
	}
}
func TestConcurrentClient(t *testing.T) {
	data := wifiFrame(t)
	c := localClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write(data) }, nil)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := c.LookupWiFi(context.Background(), wifiReq)
			if err != nil || len(result.Records) != 1 {
				t.Errorf("result %+v err %v", result, err)
			}
		}()
	}
	wg.Wait()
}
func TestBodyReadFailureFallback(t *testing.T) {
	failure := errors.New("broken response stream")
	c, err := NewClient(Config{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(failedReader{failure}), Header: make(http.Header)}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.LookupWiFi(context.Background(), wifiReq)
	if len(result.Attempts) != 2 || !errors.Is(err, failure) {
		t.Fatalf("result %+v err %v", result, err)
	}
}

func TestCancellationDuringDecode(t *testing.T) {
	c, err := NewClient(Config{AttemptTimeout: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result, err := query(ctx, c, nil, 0, func(context.Context, Region) ([]int, error) { cancel(); return []int{1}, nil })
	if !errors.Is(err, context.Canceled) || len(result.Records) != 0 || len(result.Attempts) != 1 {
		t.Fatalf("late result accepted: %+v %v", result, err)
	}
	result, err = query(context.Background(), c, nil, 0, func(ctx context.Context, _ Region) ([]int, error) { <-ctx.Done(); return []int{1}, nil })
	if !errors.Is(err, context.DeadlineExceeded) || len(result.Records) != 0 || len(result.Attempts) != 2 {
		t.Fatalf("late attempt result accepted: %+v %v", result, err)
	}
}

type failedReader struct{ err error }

func (r failedReader) Read([]byte) (int, error) { return 0, r.err }
