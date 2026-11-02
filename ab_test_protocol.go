package neatlogs

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"time"
	"unicode/utf16"
)

// ABParams contains scalar settings selected by an A/B test.
type ABParams map[string]any

type abArm struct {
	Key    string   `json:"key"`
	Label  *string  `json:"label"`
	Params ABParams `json:"params"`
	Weight *int     `json:"weight"`
}
type abConfiguration struct {
	ProtocolVersion     int     `json:"protocolVersion"`
	ID                  string  `json:"id"`
	Key                 string  `json:"key"`
	Revision            int     `json:"revision"`
	AllocationVersion   int     `json:"allocationVersion"`
	Phase               string  `json:"phase"`
	RolloutPercentage   int     `json:"rolloutPercentage"`
	Salt                string  `json:"salt"`
	Unit                string  `json:"unit"`
	Status              string  `json:"status"`
	Enabled             *bool   `json:"enabled"`
	DefaultArm          string  `json:"defaultArm"`
	ExpiresAt           string  `json:"expiresAt"`
	EndsAt              string  `json:"endsAt"`
	RefreshAfterSeconds int     `json:"refreshAfterSeconds"`
	Assignment          string  `json:"assignment"`
	Arms                []abArm `json:"arms"`
	ETag                string  `json:"etag"`
	expires             time.Time
	ends                time.Time
}

var abKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,79}$`)
var abSaltPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errABConfiguration = errors.New("neatlogs: A/B settings are unavailable or invalid")

const abAssignmentAlgorithm = "sha256_utf8_salt_colon_unit_first_uint32_be_mod_10000"

func abBucket(salt, unit string) uint32 {
	digest := sha256.Sum256([]byte(salt + ":" + unit))
	return binary.BigEndian.Uint32(digest[:4]) % 10000
}
func abLength(value string) int { return len(utf16.Encode([]rune(value))) }
func abCopyParams(params ABParams) ABParams {
	result := make(ABParams, len(params))
	for key, value := range params {
		result[key] = value
	}
	return result
}
func abValidParams(params ABParams) bool {
	if params == nil || len(params) > 20 {
		return false
	}
	for key, value := range params {
		if abLength(key) < 1 || abLength(key) > 100 {
			return false
		}
		switch v := value.(type) {
		case nil, bool:
		case string:
			if abLength(v) > 2000 {
				return false
			}
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return false
			}
		case json.Number:
			n, err := v.Float64()
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
func (c *abConfiguration) validate(key string, previous *abConfiguration, now time.Time) error {
	if c.ProtocolVersion != 1 || !datasetUUID.MatchString(c.ID) || c.Key != key || !abKeyPattern.MatchString(c.Key) || c.Revision < 1 || (c.AllocationVersion < 1 || c.AllocationVersion > 2147483647) || !abSaltPattern.MatchString(c.Salt) ||
		(c.Unit != "session" && c.Unit != "end_user") || c.Enabled == nil || (c.DefaultArm != "a" && c.DefaultArm != "b") || c.RefreshAfterSeconds != 60 || c.Assignment != abAssignmentAlgorithm || abLength(c.ETag) < 1 || abLength(c.ETag) > 200 || len(c.Arms) != 2 {
		return errABConfiguration
	}
	switch c.Status {
	case "draft", "running", "paused", "auto_paused", "concluded", "ramping", "shipped", "rolled_back":
	default:
		return errABConfiguration
	}
	for index, arm := range c.Arms {
		if arm.Key != []string{"a", "b"}[index] || arm.Label == nil || abLength(*arm.Label) > 255 || !abValidParams(arm.Params) || arm.Weight == nil || *arm.Weight < 0 || *arm.Weight > 10000 {
			return errABConfiguration
		}
	}
	if *c.Arms[0].Weight+*c.Arms[1].Weight != 10000 {
		return errABConfiguration
	}
	if c.Phase == "rollout" {
		if c.AllocationVersion < 2 || (c.RolloutPercentage != 50 && c.RolloutPercentage != 100) || c.DefaultArm != "a" {
			return errABConfiguration
		}
		if *c.Enabled {
			if (c.RolloutPercentage == 50 && c.Status != "ramping") || (c.RolloutPercentage == 100 && c.Status != "shipped") {
				return errABConfiguration
			}
			if *c.Arms[1].Weight != c.RolloutPercentage*100 {
				return errABConfiguration
			}
		} else if *c.Arms[0].Weight != 10000 {
			return errABConfiguration
		}
	} else {
		if (c.Phase != "" && c.Phase != "analysis") || c.AllocationVersion != 1 || c.RolloutPercentage != 0 {
			return errABConfiguration
		}
		defaultIndex := 0
		if c.Status == "shipped" {
			defaultIndex = 1
		}
		if *c.Enabled && c.Status != "running" || c.DefaultArm != c.Arms[defaultIndex].Key || !*c.Enabled && *c.Arms[defaultIndex].Weight != 10000 {
			return errABConfiguration
		}
	}
	var err error
	c.expires, err = time.Parse(time.RFC3339Nano, c.ExpiresAt)
	if err != nil {
		return errABConfiguration
	}
	c.ends, err = time.Parse(time.RFC3339Nano, c.EndsAt)
	if err != nil {
		return errABConfiguration
	}
	if c.Phase == "rollout" && *c.Enabled && !c.ends.After(now) {
		return errABConfiguration
	}
	if !c.expires.After(now) || c.expires.After(now.Add(65*time.Second)) || previous != nil && (c.ID != previous.ID || c.Salt != previous.Salt || c.Revision < previous.Revision || c.AllocationVersion < previous.AllocationVersion) {
		return errABConfiguration
	}
	if c.expires.After(now.Add(time.Minute)) {
		c.expires = now.Add(time.Minute)
	}
	return nil
}
