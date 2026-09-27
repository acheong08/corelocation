// Package corelocation queries Apple's undocumented Wi-Fi, LTE cell, and Wi-Fi
// tile services. It is not a binding to the macOS CoreLocation framework.
package corelocation

import (
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
)

// Region chooses endpoint preference. Auto uses a location hint, cell MCC, or
// tile center; without a hint it starts with International.
type Region string

const (
	Auto          Region = "auto"
	International Region = "international"
	China         Region = "china"
)

// Point contains geographic coordinates in degrees. Coordinates are returned
// as supplied by the service; no datum transformation is performed.
type Point struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

func (p Point) Validate() error {
	if math.IsNaN(p.Latitude) || math.IsInf(p.Latitude, 0) || p.Latitude < -90 || p.Latitude > 90 || math.IsNaN(p.Longitude) || math.IsInf(p.Longitude, 0) || p.Longitude < -180 || p.Longitude > 180 {
		return fmt.Errorf("invalid coordinates: latitude=%v longitude=%v", p.Latitude, p.Longitude)
	}
	return nil
}

// AccessPoint is a located Wi-Fi access point, not an estimate of the caller's
// location. Unknown optional wire fields are deliberately not given units.
type AccessPoint struct {
	BSSID    string `json:"bssid"`
	Location Point  `json:"location"`
}

// Tower identifies an LTE cell. GSM, UMTS and NR are not supported.
type Tower struct {
	MCC    uint32 `json:"mcc"`
	MNC    uint32 `json:"mnc"`
	CellID uint32 `json:"cell_id"`
	TAC    uint32 `json:"tac"`
}

func (t Tower) Validate() error {
	if t.MCC < 1 || t.MCC > 999 || t.MNC > 999 || t.CellID > 0xfffffff || t.TAC > 0xffff {
		return fmt.Errorf("invalid LTE tower identifiers")
	}
	return nil
}

type Cell struct {
	Tower    Tower `json:"tower"`
	Location Point `json:"location"`
}
type WiFiRequest struct {
	BSSIDs []string
	// MaxResults is passed to Apple. Zero selects the service default; positive
	// values are enforced client-side as a hard cap on returned records.
	MaxResults int32
	// Hint affects routing only and is never transmitted to Apple.
	Hint *Point
}
type CellRequest struct {
	Tower      Tower
	MaxResults int32
	Hint       *Point
}

// Attempt records endpoint provenance without logging submitted identifiers.
type Attempt struct {
	Region Region `json:"region"`
	Error  string `json:"error,omitempty"`
}

// Result retains attempts even on failure. Records come from the first endpoint
// with usable data, not a merge across regions. Partial batches do not fallback.
type Result[T any] struct {
	Records  []T       `json:"records"`
	Region   Region    `json:"region,omitempty"`
	Attempts []Attempt `json:"attempts"`
}

// NormalizeBSSID accepts six-byte colon, hyphen, or dotted MAC notation,
// including single-digit colon-separated octets returned by some Apple records.
func NormalizeBSSID(s string) (string, error) {
	text := strings.TrimSpace(s)
	if parts := strings.Split(text, ":"); len(parts) == 6 {
		m := make(net.HardwareAddr, 6)
		valid := true
		for i, part := range parts {
			if len(part) < 1 || len(part) > 2 {
				valid = false
				break
			}
			// ParseUint accepts a leading +; a MAC octet must contain only hex digits.
			for _, ch := range part {
				if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f' || ch >= 'A' && ch <= 'F') {
					valid = false
				}
			}
			n, err := strconv.ParseUint(part, 16, 8)
			if err != nil {
				valid = false
				break
			}
			m[i] = byte(n)
		}
		if valid {
			return m.String(), nil
		}
	}
	m, err := net.ParseMAC(text)
	if err != nil || len(m) != 6 {
		return "", fmt.Errorf("invalid BSSID %q: expected a six-byte MAC address", s)
	}
	return m.String(), nil
}

// PreferChina is an intentionally coarse routing hint, NOT a political boundary
// or geofence. Border false positives are handled by fallback. Small exclusion
// boxes prefer the international dataset for Hong Kong, Macau, and Taiwan.
func PreferChina(p Point) bool {
	if p.Validate() != nil {
		return false
	}
	in := func(s, n, w, e float64) bool {
		return p.Latitude >= s && p.Latitude <= n && p.Longitude >= w && p.Longitude <= e
	}
	if in(21.8, 22.6, 113.8, 114.5) || in(22.0, 22.25, 113.5, 113.65) || in(21.8, 25.4, 119.3, 122.1) {
		return false
	}
	return in(18, 54, 73, 135)
}

// PreferredRegion selects the initial endpoint. An explicit coordinate hint
// wins over the MCC (460); no inference is possible from a BSSID alone.
func PreferredRegion(hint *Point, mcc uint32) Region {
	if hint != nil {
		if PreferChina(*hint) {
			return China
		}
		return International
	}
	if mcc == 460 {
		return China
	}
	return International
}
