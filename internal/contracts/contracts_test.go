// Package contracts checks that the published contracts agree with each other
// and with the domain vocabulary.
//
// A contract file is only worth having if something notices when it stops
// matching the code. Left unchecked, `api/openapi.yaml` and the event schemas
// drift the moment someone adds a value in one place and not the other — and
// the failure surfaces as a Flutter client rendering a severity it has never
// heard of, weeks later, in front of a user.
//
// So: one canonical list here, and every contract file measured against it.
package contracts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	iam "github.com/ayna/ayna-backend/internal/modules/iam/domain"
)

// ---------------------------------------------------------------------------
// The canonical vocabulary.
//
// This is the single source of truth until `internal/modules/skinanalysis/
// domain` exists, at which point these should be replaced by imports of the
// real domain types so the contracts are checked against the code that runs
// rather than a copy of it. Leaving a copy here permanently would recreate
// exactly the drift this file exists to prevent.
// ---------------------------------------------------------------------------

var canonicalSeverity = []string{"unknown", "none", "mild", "moderate", "severe"}

var canonicalIssueTypes = []string{"acne", "redness", "dryness", "dark_spots", "texture", "pores"}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate go.mod")
	return ""
}

func loadYAML(t *testing.T, rel string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return doc
}

func loadJSON(t *testing.T, rel string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return doc
}

// dig walks a nested map by key, returning nil if any step is missing rather
// than panicking — a missing path should fail as a readable test error.
func dig(doc map[string]any, path ...string) any {
	var cur any = doc
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[k]
		if !ok {
			return nil
		}
	}
	return cur
}

func strings_(t *testing.T, v any, where string) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: expected a list, got %T", where, v)
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s: expected string values, got %T", where, e)
		}
		out = append(out, s)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------

func TestSeverityIsIdenticalInEveryContract(t *testing.T) {
	api := loadYAML(t, "api/openapi.yaml")
	evt := loadJSON(t, "events/skinanalysis.report.generated.v1.schema.json")

	fromAPI := strings_(t, dig(api, "components", "schemas", "Severity", "enum"), "openapi Severity")
	fromEvt := strings_(t,
		dig(evt, "properties", "issues", "items", "properties", "severity", "enum"),
		"event severity")

	if !sameSet(fromAPI, canonicalSeverity) {
		t.Errorf("openapi Severity = %v, canonical = %v", fromAPI, canonicalSeverity)
	}
	if !sameSet(fromEvt, canonicalSeverity) {
		t.Errorf("event severity = %v, canonical = %v", fromEvt, canonicalSeverity)
	}
}

func TestUnknownSeverityExistsAndIsNotConfusableWithNone(t *testing.T) {
	// The single most consequential value in the whole contract. A concern the
	// analysis could not assess must be representable as its own thing — if
	// `unknown` were ever dropped, the natural fallback is `none`, and the app
	// would tell someone their skin is clear when nothing ever looked at it.
	for _, set := range [][]string{canonicalSeverity} {
		var hasUnknown, hasNone bool
		for _, v := range set {
			hasUnknown = hasUnknown || v == "unknown"
			hasNone = hasNone || v == "none"
		}
		if !hasUnknown {
			t.Fatal(`severity must include "unknown" — without it, an unassessed ` +
				`concern renders as "none", which is a false statement about someone's face`)
		}
		if !hasNone {
			t.Fatal(`severity must include "none" — "unknown" is not a substitute for it either`)
		}
	}
}

func TestIssueTypesAreIdenticalInEveryContract(t *testing.T) {
	api := loadYAML(t, "api/openapi.yaml")
	evt := loadJSON(t, "events/skinanalysis.report.generated.v1.schema.json")

	fromAPI := strings_(t, dig(api, "components", "schemas", "IssueType", "enum"), "openapi IssueType")
	fromEvt := strings_(t,
		dig(evt, "properties", "issues", "items", "properties", "type", "enum"),
		"event issue type")

	if !sameSet(fromAPI, canonicalIssueTypes) {
		t.Errorf("openapi IssueType = %v, canonical = %v", fromAPI, canonicalIssueTypes)
	}
	if !sameSet(fromEvt, canonicalIssueTypes) {
		t.Errorf("event issue type = %v, canonical = %v", fromEvt, canonicalIssueTypes)
	}
	if len(canonicalIssueTypes) != 6 {
		t.Errorf("FR-4 promises exactly six concerns, canonical has %d", len(canonicalIssueTypes))
	}
}

func TestAReportAlwaysCarriesAllSixConcerns(t *testing.T) {
	// Omitting an unmeasured concern and reporting it as `unknown` are not
	// equivalent: an absent key is indistinguishable from a field the client
	// forgot to read. Both contracts pin the array to exactly six.
	api := loadYAML(t, "api/openapi.yaml")
	evt := loadJSON(t, "events/skinanalysis.report.generated.v1.schema.json")

	for _, c := range []struct {
		label    string
		min, max any
	}{
		{"openapi SkinReport.issues",
			dig(api, "components", "schemas", "SkinReport", "properties", "issues", "minItems"),
			dig(api, "components", "schemas", "SkinReport", "properties", "issues", "maxItems")},
		{"event issues",
			dig(evt, "properties", "issues", "minItems"),
			dig(evt, "properties", "issues", "maxItems")},
	} {
		if !isSix(c.min) || !isSix(c.max) {
			t.Errorf("%s: minItems=%v maxItems=%v, want 6/6", c.label, c.min, c.max)
		}
	}
}

func isSix(v any) bool {
	switch n := v.(type) {
	case int:
		return n == 6
	case float64:
		return n == 6
	}
	return false
}

func TestSkinAgeIsNullableEverywhere(t *testing.T) {
	// Free-tier reports genuinely have no skin age (PD-3). If the contract
	// made it required, the backend would be forced to invent one — and the
	// obvious invention is the user's chronological age, which would render as
	// "right in step" and be a lie.
	api := loadYAML(t, "api/openapi.yaml")
	if n, _ := dig(api, "components", "schemas", "SkinReport", "properties", "skinAge", "nullable").(bool); !n {
		t.Error("openapi SkinReport.skinAge must be nullable — the free tier has no skin age")
	}

	evt := loadJSON(t, "events/skinanalysis.report.generated.v1.schema.json")
	types := dig(evt, "properties", "skinAge", "type")
	arr, ok := types.([]any)
	if !ok {
		t.Fatalf("event skinAge type = %v, want a [type, null] union", types)
	}
	var allowsNull bool
	for _, x := range arr {
		if s, _ := x.(string); s == "null" {
			allowsNull = true
		}
	}
	if !allowsNull {
		t.Error("event skinAge must allow null")
	}
}

func TestEveryEventSchemaIsSelfConsistent(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "events")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read events dir: %v", err)
	}

	var found int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".schema.json") {
			continue
		}
		found++
		doc := loadJSON(t, filepath.Join("events", e.Name()))

		// The $id must name the file. A schema whose id points somewhere else
		// resolves to the wrong document in any tooling that follows refs.
		id, _ := doc["$id"].(string)
		if !strings.HasSuffix(id, e.Name()) {
			t.Errorf("%s: $id %q does not end with the filename", e.Name(), id)
		}

		// additionalProperties: false on every payload. Without it a typo in a
		// producer ships silently and the consumer reads a zero value.
		if ap, ok := doc["additionalProperties"]; !ok || ap != false {
			t.Errorf("%s: must set additionalProperties:false, got %v", e.Name(), ap)
		}

		if _, ok := doc["required"]; !ok {
			t.Errorf("%s: has no required fields — everything optional is not a contract", e.Name())
		}
	}

	if found < 3 {
		t.Errorf("expected at least the envelope and two payload schemas, found %d", found)
	}
}

func TestEventTypeNamingConventionAcceptsRealNamesAndRejectsBadOnes(t *testing.T) {
	env := loadJSON(t, "events/envelope.schema.json")
	pattern, _ := dig(env, "properties", "eventType", "pattern").(string)
	if pattern == "" {
		t.Fatal("envelope has no eventType pattern")
	}
	re := regexp.MustCompile(pattern)

	for _, good := range []string{
		"skinanalysis.scan.submitted.v1",
		"skinanalysis.report.generated.v1",
		"iam.user.registered.v1",
		"billing.subscription.renewed.v2",
	} {
		if !re.MatchString(good) {
			t.Errorf("pattern rejects a valid event type: %q", good)
		}
	}

	for _, bad := range []string{
		"ScanSubmitted",            // not dotted, not lowercase
		"skinanalysis.scan.v1",     // missing a segment
		"skinanalysis.scan.done",   // unversioned — the whole point of the suffix
		"skinanalysis.scan.done.v", // version with no number
	} {
		if re.MatchString(bad) {
			t.Errorf("pattern accepts an invalid event type: %q", bad)
		}
	}
}

func TestOpenAPICoversTheTwoRiskiestFlows(t *testing.T) {
	// The Contracts note commits to specifying scan submission and report
	// generation in full. This fails if either is ever removed or renamed.
	api := loadYAML(t, "api/openapi.yaml")
	paths, ok := api["paths"].(map[string]any)
	if !ok {
		t.Fatal("openapi has no paths")
	}

	for _, required := range []string{
		"/scans",
		"/scans/{scanId}",
		"/scans/{scanId}/report",
		"/users",
		"/users/me",
		"/consent",
	} {
		if _, ok := paths[required]; !ok {
			t.Errorf("openapi is missing %s", required)
		}
	}
}

func TestErrorsUseProblemDetailsNotAdHocShapes(t *testing.T) {
	// One error shape across the whole surface. A client that has to branch on
	// three different error bodies will get one of them wrong.
	api := loadYAML(t, "api/openapi.yaml")
	if dig(api, "components", "schemas", "Problem") == nil {
		t.Fatal("no Problem schema — errors should follow RFC 9457")
	}

	b, err := os.ReadFile(filepath.Join(repoRoot(t), "api/openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "application/problem+json") {
		t.Error("Problem is defined but never used as an error content type")
	}
}

// The retention options are checked against the DOMAIN, not against a copy.
//
// This is the drift that would be most expensive to discover late: the API
// promising a 90-day option the domain refuses, or the domain accepting a value
// the settings screen never offers. Both fail as a 400 in front of a user who
// tapped a button the app drew for them.
//
// Note this imports the real domain package rather than restating the list, as
// the header above says the severity lists eventually should.
func TestRetentionOptionsMatchTheContract(t *testing.T) {
	spec := loadYAML(t, filepath.Join("api", "openapi.yaml"))

	node := dig(spec, "components", "schemas", "User", "properties", "photoRetentionDays")
	schema, ok := node.(map[string]any)
	if !ok {
		t.Fatal("User.photoRetentionDays is missing from the contract")
	}
	raw, ok := schema["enum"].([]any)
	if !ok {
		t.Fatal("User.photoRetentionDays has no enum in the contract")
	}

	var (
		fromSpec  []int
		nullFound bool
	)
	for _, v := range raw {
		if v == nil {
			nullFound = true
			continue
		}
		n, ok := v.(int)
		if !ok {
			t.Fatalf("non-integer retention option in the contract: %v", v)
		}
		fromSpec = append(fromSpec, n)
	}

	// null is the "keep indefinitely" choice. Losing it from the contract would
	// make the option undocumented while the server still honours it.
	if !nullFound {
		t.Error("the contract's retention enum has no null, so 'keep indefinitely' is undocumented")
	}

	want := append([]int(nil), iam.PhotoRetentionOptions...)
	sort.Ints(want)
	sort.Ints(fromSpec)

	if len(want) != len(fromSpec) {
		t.Fatalf("retention options differ: domain %v, contract %v", want, fromSpec)
	}
	for i := range want {
		if want[i] != fromSpec[i] {
			t.Fatalf("retention options differ: domain %v, contract %v", want, fromSpec)
		}
	}
}
