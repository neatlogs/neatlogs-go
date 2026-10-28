package neatlogs

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"time"
	"unicode/utf8"
)

var errAgentProtocol = errors.New("neatlogs: invalid session agent protocol")
var agentAttributeKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)

type agentVersion struct {
	Fingerprint string   `json:"fingerprint"`
	Params      ABParams `json:"params"`
}
type agentLive struct {
	TestID               string `json:"testId"`
	ExperimentID         string `json:"experimentId"`
	CandidateRevision    int    `json:"candidateRevision"`
	AllocationVersion    int    `json:"allocationVersion"`
	Unit                 string `json:"unit"`
	CurrentFingerprint   string `json:"currentFingerprint"`
	CandidateFingerprint string `json:"candidateFingerprint"`
	PolicyDigest         string `json:"policyDigest"`
	Status               string `json:"status"`
	EffectiveTrafficBps  int    `json:"effectiveTrafficBps"`
	TargetTrafficBps     int    `json:"targetTrafficBps"`
	Split                struct {
		A int `json:"a"`
		B int `json:"b"`
	} `json:"split"`
}
type agentConfiguration struct {
	ProtocolVersion int          `json:"protocolVersion"`
	AgentKey        string       `json:"agentKey"`
	Revision        int          `json:"revision"`
	ExpiresAt       string       `json:"expiresAt"`
	Current         agentVersion `json:"current"`
	Live            *agentLive   `json:"live"`
	expires         time.Time
	etag            string
}
type agentAssignment struct {
	ProtocolVersion   int      `json:"protocolVersion"`
	AgentKey          string   `json:"agentKey"`
	Revision          int      `json:"revision"`
	ExpiresAt         string   `json:"expiresAt"`
	Params            ABParams `json:"params"`
	Fingerprint       string   `json:"fingerprint"`
	Reason            string   `json:"reason"`
	Arm               string   `json:"arm"`
	ExperimentID      string   `json:"experimentId,omitempty"`
	CandidateRevision int      `json:"candidateRevision,omitempty"`
	TestID            string   `json:"testId,omitempty"`
	AllocationVersion int      `json:"allocationVersion,omitempty"`
	EnrollmentID      string   `json:"enrollmentId,omitempty"`
	AssignmentID      string   `json:"assignmentId,omitempty"`
}

// AgentDecision carries an immutable receipt across execution and session completion.
type AgentDecision struct {
	agentAssignment
	UnitID  string
	expires time.Time
}

// AgentChoiceOptions supplies a session identity, explicit attributes and deployed fallback settings.
type AgentChoiceOptions struct {
	Unit       string
	Attributes map[string]any
	Fallback   ABParams
}

func agentDecode(data []byte, target any) error {
	if !json.Valid(data) {
		return errAgentProtocol
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
func agentExpiry(value string, now time.Time) (time.Time, error) {
	expiry, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !expiry.After(now) || expiry.After(now.Add(65*time.Second)) {
		return time.Time{}, errAgentProtocol
	}
	if expiry.After(now.Add(time.Minute)) {
		expiry = now.Add(time.Minute)
	}
	return expiry, nil
}
func agentValidParams(value ABParams) bool {
	if value == nil {
		return false
	}
	for key, item := range value {
		if !abValidParams(ABParams{key: item}) {
			return false
		}
	}
	return true
}
func agentValidAttributes(value map[string]any) bool {
	if len(value) > 20 {
		return false
	}
	for key, item := range value {
		if !agentAttributeKey.MatchString(key) {
			return false
		}
		switch v := item.(type) {
		case bool:
		case string:
			if !utf8.ValidString(v) || abLength(v) > 256 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
func (c *agentConfiguration) validate(key string, previous *agentConfiguration, now time.Time) error {
	if c.ProtocolVersion != 2 || c.AgentKey != key || !abKeyPattern.MatchString(key) || c.Revision < 1 || !abSaltPattern.MatchString(c.Current.Fingerprint) || !agentValidParams(c.Current.Params) || previous != nil && c.Revision < previous.Revision {
		return errAgentProtocol
	}

	var err error
	c.expires, err = agentExpiry(c.ExpiresAt, now)
	if err != nil {
		return err
	}
	if live := c.Live; live != nil {
		if !datasetUUID.MatchString(live.TestID) || !datasetUUID.MatchString(live.ExperimentID) || live.CandidateRevision < 1 || live.AllocationVersion < 1 || live.AllocationVersion > 2147483647 || live.Unit != "session" || live.CurrentFingerprint != c.Current.Fingerprint || !abSaltPattern.MatchString(live.CandidateFingerprint) || !abSaltPattern.MatchString(live.PolicyDigest) {
			return errAgentProtocol
		}
		if live.Status != "running" && live.Status != "paused" && live.Status != "auto_paused" {
			return errAgentProtocol
		}
		if live.EffectiveTrafficBps < 0 || live.TargetTrafficBps < live.EffectiveTrafficBps || live.TargetTrafficBps > 10000 || live.Split.A < 0 || live.Split.B < 0 || live.Split.A+live.Split.B != 10000 {
			return errAgentProtocol
		}
	}
	return nil
}
func (a *agentAssignment) validate(c *agentConfiguration, now time.Time) (time.Time, error) {
	if a.ProtocolVersion != 2 || a.AgentKey != c.AgentKey || a.Revision != c.Revision || !agentValidParams(a.Params) || !abSaltPattern.MatchString(a.Fingerprint) {
		return time.Time{}, errAgentProtocol
	}
	expires, err := agentExpiry(a.ExpiresAt, now)
	if err != nil {
		return time.Time{}, err
	}
	if c.expires.Before(expires) {
		expires = c.expires
	}
	if !expires.After(now) {
		return time.Time{}, errAgentProtocol
	}
	if a.Reason == "assigned" {
		live := c.Live
		if live == nil || live.Status != "running" || a.TestID != live.TestID || a.ExperimentID != live.ExperimentID || a.CandidateRevision != live.CandidateRevision || a.AllocationVersion != live.AllocationVersion || !datasetUUID.MatchString(a.EnrollmentID) || !datasetUUID.MatchString(a.AssignmentID) || a.Arm != "a" && a.Arm != "b" {
			return time.Time{}, errAgentProtocol
		}
		fingerprint := c.Current.Fingerprint
		if a.Arm == "b" {
			fingerprint = live.CandidateFingerprint
		}
		if a.Fingerprint != fingerprint {
			return time.Time{}, errAgentProtocol
		}
	} else {
		if a.Reason != "outside_audience" && a.Reason != "outside_share" && a.Reason != "paused" && a.Reason != "current" {
			return time.Time{}, errAgentProtocol
		}
		if a.Arm != "a" || a.Fingerprint != c.Current.Fingerprint || a.AssignmentID != "" || a.TestID != "" || a.AllocationVersion != 0 || a.EnrollmentID != "" || a.ExperimentID != "" || a.CandidateRevision != 0 {
			return time.Time{}, errAgentProtocol
		}
	}
	return expires, nil
}
