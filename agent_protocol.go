package neatlogs

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const agentProfile = "serving-trace-v1"
const agentCapabilityHeader = "X-Neatlogs-Agent-Capability"

var errAgentProtocol = errors.New("neatlogs: invalid agent protocol")
var agentAttributeKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)

type agentVersion struct {
	Fingerprint string   `json:"fingerprint"`
	Params      ABParams `json:"params"`
}
type agentLive struct {
	TestID               string           `json:"testId"`
	ExperimentID         string           `json:"experimentId"`
	CandidateRevision    int              `json:"candidateRevision"`
	AllocationVersion    int              `json:"allocationVersion"`
	Unit                 string           `json:"unit"`
	CompletionPolicy     string           `json:"completionPolicy"`
	CurrentFingerprint   string           `json:"currentFingerprint,omitempty"`
	AgentProfile         string           `json:"agentProfile,omitempty"`
	Current              *agentVersion    `json:"current,omitempty"`
	BaselineServing      *agentServingRef `json:"baselineServing"`
	CandidateFingerprint string           `json:"candidateFingerprint"`
	PolicyDigest         string           `json:"policyDigest"`
	Status               string           `json:"status"`
	EffectiveTrafficBps  int              `json:"effectiveTrafficBps"`
	TargetTrafficBps     int              `json:"targetTrafficBps"`
	Split                struct {
		A int `json:"a"`
		B int `json:"b"`
	} `json:"split"`
}
type agentServingRef struct {
	RevisionID string `json:"revisionId"`
	Revision   int    `json:"revision"`
	Digest     string `json:"digest"`
}
type agentServingIdentity struct {
	PublicationID string `json:"publicationId"`
	Fingerprint   string `json:"fingerprint"`
}
type agentServingConfig struct {
	agentServingRef
	Unit                  string                 `json:"unit"`
	RequiredAttributeKeys []string               `json:"requiredAttributeKeys"`
	AllowedIdentities     []agentServingIdentity `json:"allowedIdentities"`
}
type agentPublishedServing struct {
	agentServingRef
	PublicationID string `json:"publicationId"`
}
type AgentDecisionCause string

type agentConfiguration struct {
	Profile         string              `json:"profile,omitempty"`
	Serving         *agentServingConfig `json:"serving"`
	ProtocolVersion int                 `json:"protocolVersion"`
	AgentKey        string              `json:"agentKey"`
	Revision        int                 `json:"revision"`
	ExpiresAt       string              `json:"expiresAt"`
	Current         agentVersion        `json:"current"`
	Live            *agentLive          `json:"live"`
	expires         time.Time
	etag            string
}
type agentAssignment struct {
	Profile           string                 `json:"profile,omitempty"`
	Serving           *agentPublishedServing `json:"serving,omitempty"`
	AttributesDigest  string                 `json:"attributesDigest,omitempty"`
	WireUnitID        string                 `json:"unitId,omitempty"`
	ProtocolVersion   int                    `json:"protocolVersion"`
	AgentKey          string                 `json:"agentKey"`
	Revision          int                    `json:"revision"`
	ExpiresAt         string                 `json:"expiresAt"`
	Params            ABParams               `json:"params"`
	Fingerprint       string                 `json:"fingerprint"`
	Reason            string                 `json:"reason"`
	Arm               string                 `json:"arm,omitempty"`
	ExperimentID      string                 `json:"experimentId,omitempty"`
	CandidateRevision int                    `json:"candidateRevision,omitempty"`
	TestID            string                 `json:"testId,omitempty"`
	AllocationVersion int                    `json:"allocationVersion,omitempty"`
	EnrollmentID      string                 `json:"enrollmentId,omitempty"`
	AssignmentID      string                 `json:"assignmentId,omitempty"`
	Unit              string                 `json:"unit,omitempty"`
	CompletionPolicy  string                 `json:"completionPolicy,omitempty"`
}

// AgentDecision carries an immutable receipt across execution and conversation completion.
type AgentDecision struct {
	agentAssignment
	UnitID        string
	Cause         AgentDecisionCause
	knownRevision int
	knownUnit     string
	expires       time.Time
}

// AgentChoiceOptions supplies a customer or session identity, explicit attributes and deployed fallback settings.
type AgentChoiceOptions struct {
	Unit       string
	Attributes map[string]any
	Fallback   ABParams
}

func agentDecode(data []byte, target any) error {
	if !json.Valid(data) {
		return errAgentProtocol
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return errAgentProtocol
	}
	var profile string
	_ = json.Unmarshal(fields["profile"], &profile)
	if profile == agentProfile {
		if !agentWireUnicode(data) {
			return errAgentCanonical
		}
		required := ""
		switch target.(type) {
		case *agentConfiguration:
			required = "protocolVersion profile agentKey revision expiresAt current live serving"
			if string(fields["live"]) != "null" {
				var live map[string]json.RawMessage
				if json.Unmarshal(fields["live"], &live) != nil || !agentExactFields(live, "testId experimentId candidateRevision allocationVersion unit completionPolicy candidateFingerprint policyDigest status effectiveTrafficBps targetTrafficBps split agentProfile current baselineServing") {
					return errAgentProtocol
				}
			}
		case *agentAssignment:
			var reason string
			_ = json.Unmarshal(fields["reason"], &reason)
			required = "protocolVersion profile agentKey revision expiresAt params fingerprint reason"
			if reason == "assigned" {
				required += " arm experimentId candidateRevision testId allocationVersion enrollmentId assignmentId unit completionPolicy"
			} else if reason == "published" {
				required += " unit unitId attributesDigest serving"
			} else {
				required += " arm"
			}
		}
		if required != "" && !agentExactFields(fields, required) {
			return errAgentProtocol
		}
	} else {
		for _, key := range []string{"profile", "serving", "attributesDigest", "unitId"} {
			if _, exists := fields[key]; exists {
				return errAgentProtocol
			}
		}
		if _, ok := target.(*agentConfiguration); ok && string(fields["live"]) != "null" {
			var live map[string]json.RawMessage
			_ = json.Unmarshal(fields["live"], &live)
			for _, key := range []string{"current", "agentProfile", "baselineServing"} {
				if _, exists := live[key]; exists {
					return errAgentProtocol
				}
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
func agentExactFields(fields map[string]json.RawMessage, names string) bool {
	keys := strings.Fields(names)
	if len(fields) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, exists := fields[key]; !exists {
			return false
		}
	}
	return true
}
func (r *agentServingRef) valid() bool {
	return r != nil && datasetUUID.MatchString(r.RevisionID) && r.Revision > 0 && abSaltPattern.MatchString(r.Digest)
}
func agentSameServing(a, b *agentServingRef) bool { return a != nil && b != nil && *a == *b }
func (s *agentServingConfig) valid() bool {
	if s == nil || !s.agentServingRef.valid() || s.Unit != "session" && s.Unit != "end_user" || s.RequiredAttributeKeys == nil || len(s.RequiredAttributeKeys) > 20 || !sort.StringsAreSorted(s.RequiredAttributeKeys) || s.AllowedIdentities == nil || len(s.AllowedIdentities) > 32 {
		return false
	}
	seenKeys, seenIDs := map[string]bool{}, map[string]bool{}
	for _, key := range s.RequiredAttributeKeys {
		if !agentAttributeKey.MatchString(key) || seenKeys[key] {
			return false
		}
		seenKeys[key] = true
	}
	for _, identity := range s.AllowedIdentities {
		if !datasetUUID.MatchString(identity.PublicationID) || !abSaltPattern.MatchString(identity.Fingerprint) || seenIDs[identity.PublicationID] {
			return false
		}
		seenIDs[identity.PublicationID] = true
	}
	return true
}
func (s *agentServingConfig) allows(id, fingerprint string) bool {
	for _, identity := range s.AllowedIdentities {
		if identity.PublicationID == id && identity.Fingerprint == fingerprint {
			return true
		}
	}
	return false
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
func agentValidCompletion(unit, policy string) bool {
	return unit == "session" && policy == "explicit" || unit == "end_user" && policy == "fixed_horizon"
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
	if c.Profile != "" && c.Profile != agentProfile || c.Profile == "" && c.Serving != nil || c.Serving != nil && !c.Serving.valid() {
		return errAgentProtocol
	}
	if live := c.Live; live != nil {
		currentOK := live.CurrentFingerprint == c.Current.Fingerprint
		if c.Profile == agentProfile {
			currentOK = live.CurrentFingerprint == "" && live.Current != nil && agentValidParams(live.Current.Params) && abSaltPattern.MatchString(live.Current.Fingerprint) && (live.AgentProfile == "legacy-v2" || live.AgentProfile == agentProfile) && (live.BaselineServing == nil || live.BaselineServing.valid()) && (c.Serving == nil || c.Serving.Unit == live.Unit)
		}
		if !datasetUUID.MatchString(live.TestID) || !datasetUUID.MatchString(live.ExperimentID) || live.CandidateRevision < 1 || live.AllocationVersion < 1 || live.AllocationVersion > 2147483647 || !agentValidCompletion(live.Unit, live.CompletionPolicy) || !currentOK || !abSaltPattern.MatchString(live.CandidateFingerprint) || !abSaltPattern.MatchString(live.PolicyDigest) {
			return errAgentProtocol
		}
		if live.Status != "running" && live.Status != "paused" && live.Status != "auto_paused" {
			return errAgentProtocol
		}
		if live.EffectiveTrafficBps < 0 || live.TargetTrafficBps < live.EffectiveTrafficBps || live.TargetTrafficBps > 10000 || live.Split.A < 0 || live.Split.B < 0 || live.Split.A+live.Split.B != 10000 {
			return errAgentProtocol
		}
	}
	if c.Profile == agentProfile && !(c.Live != nil && c.Live.AgentProfile == "legacy-v2" && c.Serving == nil) {
		fingerprint, err := agentVersionFingerprint(c.AgentKey, c.Current.Params)
		if err != nil {
			return err
		}
		if fingerprint != c.Current.Fingerprint {
			return errAgentProtocol
		}
		if c.Live != nil {
			fingerprint, err = agentVersionFingerprint(c.AgentKey, c.Live.Current.Params)
			if err != nil {
				return err
			}
			if fingerprint != c.Live.Current.Fingerprint {
				return errAgentProtocol
			}
			if c.Live.AgentProfile == agentProfile && (c.Serving == nil && c.Live.BaselineServing != nil || c.Serving != nil && !agentSameServing(c.Live.BaselineServing, &c.Serving.agentServingRef)) {
				return errAgentProtocol
			}
		}
	}
	return nil
}
func (a *agentAssignment) validate(c *agentConfiguration, now time.Time) (time.Time, error) {
	if a.Profile != c.Profile || a.ProtocolVersion != 2 || a.AgentKey != c.AgentKey || a.Revision != c.Revision || !agentValidParams(a.Params) || !abSaltPattern.MatchString(a.Fingerprint) {
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
		if live == nil || live.Status != "running" || a.TestID != live.TestID || a.ExperimentID != live.ExperimentID || a.CandidateRevision != live.CandidateRevision || a.AllocationVersion != live.AllocationVersion || a.Unit != live.Unit || a.CompletionPolicy != live.CompletionPolicy || !datasetUUID.MatchString(a.EnrollmentID) || !datasetUUID.MatchString(a.AssignmentID) || a.Arm != "a" && a.Arm != "b" {
			return time.Time{}, errAgentProtocol
		}
		fingerprint := c.Current.Fingerprint
		if c.Profile == agentProfile {
			fingerprint = live.Current.Fingerprint
		}
		if a.Arm == "b" {
			fingerprint = live.CandidateFingerprint
		}
		if a.Fingerprint != fingerprint {
			return time.Time{}, errAgentProtocol
		}
		if live.AgentProfile == agentProfile {
			actual, err := agentVersionFingerprint(a.AgentKey, a.Params)
			if err != nil {
				return time.Time{}, err
			}
			if actual != a.Fingerprint {
				return time.Time{}, errAgentProtocol
			}
		}
	} else if a.Reason == "published" {
		serving := c.Serving
		if c.Profile != agentProfile || serving == nil || a.Serving == nil || a.Unit != serving.Unit || !agentSameServing(&a.Serving.agentServingRef, &serving.agentServingRef) || !serving.allows(a.Serving.PublicationID, a.Fingerprint) || a.Arm != "" || a.AssignmentID != "" || a.TestID != "" || a.ExperimentID != "" || a.EnrollmentID != "" || a.CandidateRevision != 0 || a.AllocationVersion != 0 || a.CompletionPolicy != "" || !abSaltPattern.MatchString(a.AttributesDigest) || !utf8.ValidString(a.WireUnitID) || abLength(a.WireUnitID) < 1 || abLength(a.WireUnitID) > 512 {
			return time.Time{}, errAgentProtocol
		}
		actual, err := agentVersionFingerprint(a.AgentKey, a.Params)
		if err != nil {
			return time.Time{}, err
		}
		if actual != a.Fingerprint {
			return time.Time{}, errAgentProtocol
		}
	} else {
		if a.Reason != "outside_audience" && a.Reason != "outside_share" && a.Reason != "paused" && a.Reason != "current" {
			return time.Time{}, errAgentProtocol
		}
		if a.Arm != "a" || a.Fingerprint != c.Current.Fingerprint || a.AssignmentID != "" || a.TestID != "" || a.AllocationVersion != 0 || a.EnrollmentID != "" || a.ExperimentID != "" || a.CandidateRevision != 0 || a.Unit != "" || a.CompletionPolicy != "" {
			return time.Time{}, errAgentProtocol
		}
		if c.Profile == agentProfile && (c.Serving != nil || c.Live == nil || c.Live.AgentProfile != "legacy-v2") {
			actual, err := agentVersionFingerprint(a.AgentKey, a.Params)
			if err != nil {
				return time.Time{}, err
			}
			if actual != a.Fingerprint {
				return time.Time{}, errAgentProtocol
			}
		}

	}
	return expires, nil
}
