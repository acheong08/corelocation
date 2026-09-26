// Command corelocation queries Wi-Fi, LTE cells, and Wi-Fi tiles, or converts tile keys offline.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/acheong08/corelocation"
)

type lookupClient interface {
	LookupWiFi(context.Context, corelocation.WiFiRequest) (corelocation.Result[corelocation.AccessPoint], error)
	LookupCell(context.Context, corelocation.CellRequest) (corelocation.Result[corelocation.Cell], error)
	FetchTile(context.Context, corelocation.TileKey) (corelocation.Result[corelocation.AccessPoint], error)
}

// runConfig supports local endpoints and doubles without environment variables.
type runConfig struct {
	ClientConfig corelocation.Config
	NewClient    func(corelocation.Config) (lookupClient, error)
}

type lookupOutput[T any] struct {
	corelocation.Result[T]
	Error string `json:"error,omitempty"`
}

type tileKeyOutput struct {
	// A decimal string preserves all uint64 bits in JavaScript JSON consumers.
	Key    string             `json:"key"`
	Zoom   int                `json:"zoom"`
	Center corelocation.Point `json:"center"`
}

type stringsFlag []string

func (s *stringsFlag) String() string         { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(value string) error { *s = append(*s, value); return nil }

const usage = `Usage:
  corelocation wifi --bssid MAC [--bssid MAC ...] [--lat LAT --lon LON] [--limit N]
  corelocation cell --mcc N --mnc N --cell-id N --tac N [--lat LAT --lon LON] [--limit N]
  corelocation tile (--key DECIMAL | --lat LAT --lon LON)
  corelocation tile-key (--key DECIMAL | --lat LAT --lon LON --zoom N)

Common flags (after the subcommand):
  --region auto|international|china  Preferred region (default auto)
  --no-fallback                      Query only the preferred region
  --timeout DURATION                 Whole-operation timeout (default 20s)
  --attempt-timeout DURATION         Per-endpoint timeout (default 10s)

Tile queries require zoom 13, including when supplying a key; there is no --zoom
option. tile-key is offline and requires an explicit zoom when encoding.
Lookup JSON includes records, region, attempts,
and an error field on failure. Tile-key JSON contains key (decimal string),
zoom, and center. Diagnostics go to stderr; failures exit nonzero.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, runConfig{})
	stop()
	os.Exit(code)
}

// run returns 0 for success/help, 2 for invalid input, and 1 for operational errors.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, cfg runConfig) int {
	fail := func(err error, code int) int {
		fmt.Fprintln(stderr, "corelocation:", err)
		if writeErr := json.NewEncoder(stdout).Encode(struct {
			Error string `json:"error"`
		}{err.Error()}); writeErr != nil {
			fmt.Fprintln(stderr, "corelocation: write JSON:", writeErr)
			return 1
		}
		return code
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return fail(errors.New("a subcommand is required"), 2)
	}
	command := args[0]
	if command == "--help" || command == "-h" || command == "help" {
		if len(args) > 1 {
			if command == "help" && len(args) == 2 {
				return run(ctx, []string{args[1], "--help"}, stdout, stderr, cfg)
			}
			return fail(errors.New("unexpected arguments after help"), 2)
		}
		fmt.Fprint(stdout, usage)
		return 0
	}
	if command != "wifi" && command != "cell" && command != "tile" && command != "tile-key" {
		return fail(fmt.Errorf("unknown subcommand %q", command), 2)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// flag invokes Usage for parse errors too; keep those stdout streams JSON-only.
	fs.Usage = func() {}
	region := fs.String("region", "auto", "preferred region: auto, international, or china")
	noFallback := fs.Bool("no-fallback", false, "disable cross-region fallback")
	timeout := fs.Duration("timeout", 20*time.Second, "whole-operation timeout")
	attemptTimeout := fs.Duration("attempt-timeout", 10*time.Second, "per-endpoint timeout")
	coordinate := func(name, description string) *float64 {
		value := new(float64)
		supplied := false
		fs.Func(name, description, func(text string) error {
			if supplied {
				return fmt.Errorf("--%s may only be supplied once", name)
			}
			n, err := strconv.ParseFloat(text, 64)
			if err == nil {
				*value, supplied = n, true
			}
			return err
		})
		return value
	}
	lat := coordinate("lat", "latitude in degrees (routing hint for wifi/cell)")
	lon := coordinate("lon", "longitude in degrees (routing hint for wifi/cell)")
	var bssids stringsFlag
	var limit int64
	var mcc, mnc, cellID, tac uint64
	var keyText string
	zoom := corelocation.WiFiTileZoom
	switch command {
	case "wifi":
		fs.Var(&bssids, "bssid", "six-byte MAC address (repeatable)")
		fs.Int64Var(&limit, "limit", 0, "maximum results (0 uses service default)")
	case "cell":
		decimalID := func(name string, target *uint64, description string) {
			fs.Func(name, description, func(value string) error {
				n, err := strconv.ParseUint(value, 10, 64)
				if err == nil {
					*target = n
				}
				return err
			})
		}
		decimalID("mcc", &mcc, "required decimal mobile country code (1..999)")
		decimalID("mnc", &mnc, "required decimal mobile network code (0..999)")
		decimalID("cell-id", &cellID, "required decimal LTE cell ID (0..268435455)")
		decimalID("tac", &tac, "required decimal tracking area code (0..65535)")
		fs.Int64Var(&limit, "limit", 0, "maximum results (0 uses service default)")
	case "tile", "tile-key":
		fs.StringVar(&keyText, "key", "", "decimal uint64 tile key")
		if command == "tile-key" {
			fs.IntVar(&zoom, "zoom", corelocation.WiFiTileZoom, "offline tile zoom (required when encoding)")
		}
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			fmt.Fprintf(stdout, "\n%s flags:\n", command)
			fs.SetOutput(stdout)
			fs.PrintDefaults()
			return 0
		}
		return fail(err, 2)
	}
	if fs.NArg() != 0 {
		return fail(fmt.Errorf("unexpected positional arguments: %v", fs.Args()), 2)
	}
	seen := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	if *region != string(corelocation.Auto) && *region != string(corelocation.International) && *region != string(corelocation.China) {
		return fail(fmt.Errorf("invalid region %q", *region), 2)
	}
	if *timeout <= 0 || *attemptTimeout <= 0 {
		return fail(errors.New("timeout and attempt-timeout must be positive"), 2)
	}
	if limit < 0 || limit > math.MaxInt32 {
		return fail(errors.New("limit must be between 0 and 2147483647"), 2)
	}
	if seen["lat"] != seen["lon"] {
		return fail(errors.New("--lat and --lon must be supplied together"), 2)
	}
	var hint *corelocation.Point
	if seen["lat"] {
		hint = &corelocation.Point{Latitude: *lat, Longitude: *lon}
		if err := hint.Validate(); err != nil {
			return fail(err, 2)
		}
	}
	var tower corelocation.Tower
	var key corelocation.TileKey
	switch command {
	case "wifi":
		if len(bssids) == 0 {
			return fail(errors.New("at least one --bssid is required"), 2)
		}
		for i, value := range bssids {
			normalized, err := corelocation.NormalizeBSSID(value)
			if err != nil {
				return fail(err, 2)
			}
			bssids[i] = normalized
		}
	case "cell":
		for _, name := range []string{"mcc", "mnc", "cell-id", "tac"} {
			if !seen[name] {
				return fail(fmt.Errorf("--%s is required", name), 2)
			}
		}
		if mcc > math.MaxUint32 || mnc > math.MaxUint32 || cellID > math.MaxUint32 || tac > math.MaxUint32 {
			return fail(errors.New("cell identifiers exceed uint32 range"), 2)
		}
		tower = corelocation.Tower{MCC: uint32(mcc), MNC: uint32(mnc), CellID: uint32(cellID), TAC: uint32(tac)}
		if err := tower.Validate(); err != nil {
			return fail(err, 2)
		}
	case "tile", "tile-key":
		if seen["key"] {
			if hint != nil || seen["zoom"] {
				return fail(errors.New("--key cannot be combined with --lat, --lon, or --zoom"), 2)
			}
			n, err := strconv.ParseUint(keyText, 10, 64)
			if err != nil {
				return fail(fmt.Errorf("invalid decimal tile key: %w", err), 2)
			}
			key = corelocation.TileKey(n)
		} else {
			if hint == nil {
				return fail(errors.New("provide either --key or both --lat and --lon"), 2)
			}
			if command == "tile-key" && !seen["zoom"] {
				return fail(errors.New("--zoom is required for tile-key encoding"), 2)
			}
			var err error
			key, err = corelocation.TileKeyFromPoint(*hint, zoom)
			if err != nil {
				return fail(err, 2)
			}
		}
		keyZoom, err := key.Zoom()
		if err != nil {
			return fail(err, 2)
		}
		if command == "tile" && keyZoom != corelocation.WiFiTileZoom {
			return fail(fmt.Errorf("Wi-Fi tile queries require zoom %d; key has zoom %d", corelocation.WiFiTileZoom, keyZoom), 2)
		}
		center, err := key.Center()
		if err != nil {
			return fail(err, 2)
		}
		if command == "tile-key" {
			if err := json.NewEncoder(stdout).Encode(tileKeyOutput{Key: strconv.FormatUint(uint64(key), 10), Zoom: keyZoom, Center: center}); err != nil {
				fmt.Fprintln(stderr, "corelocation: write JSON:", err)
				return 1
			}
			return 0
		}
	}
	clientConfig := cfg.ClientConfig
	clientConfig.Region = corelocation.Region(*region)
	clientConfig.DisableFallback = *noFallback
	clientConfig.AttemptTimeout = *attemptTimeout
	newClient := cfg.NewClient
	if newClient == nil {
		newClient = func(c corelocation.Config) (lookupClient, error) { return corelocation.NewClient(c) }
	}
	client, err := newClient(clientConfig)
	if err != nil {
		return fail(err, 1)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	switch command {
	case "wifi":
		result, err := client.LookupWiFi(ctx, corelocation.WiFiRequest{BSSIDs: bssids, MaxResults: int32(limit), Hint: hint})
		return printLookup(stdout, stderr, result, err)
	case "cell":
		result, err := client.LookupCell(ctx, corelocation.CellRequest{Tower: tower, MaxResults: int32(limit), Hint: hint})
		return printLookup(stdout, stderr, result, err)
	default:
		result, err := client.FetchTile(ctx, key)
		return printLookup(stdout, stderr, result, err)
	}
}

func printLookup[T any](stdout, stderr io.Writer, result corelocation.Result[T], err error) int {
	if result.Records == nil {
		result.Records = make([]T, 0)
	}
	if result.Attempts == nil {
		result.Attempts = make([]corelocation.Attempt, 0)
	}
	output := lookupOutput[T]{Result: result}
	code := 0
	if err != nil {
		output.Error = err.Error()
		fmt.Fprintln(stderr, "corelocation:", err)
		code = 1
	}
	if err := json.NewEncoder(stdout).Encode(output); err != nil {
		fmt.Fprintln(stderr, "corelocation: write JSON:", err)
		return 1
	}
	return code
}
