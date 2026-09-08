package id

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestGeneratedIDsMatchTheShapeTheAPIContractPromises(t *testing.T) {
	// The OpenAPI spec pins scan ids to ^scn_[0-9A-HJKMNP-TV-Z]{26}$. If the
	// generator and the contract ever disagree, clients reject ids the server
	// considers perfectly valid — so the pattern is read from the spec itself
	// rather than copied here, where it could drift.
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "api", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}

	m := regexp.MustCompile(`pattern:\s*"(\^scn_[^"]+)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("could not find the scanId pattern in api/openapi.yaml")
	}
	re := regexp.MustCompile(string(m[1]))

	for i := 0; i < 200; i++ {
		got := New(PrefixScan)
		if !re.MatchString(got) {
			t.Fatalf("generated id %q does not match the contract pattern %s", got, m[1])
		}
	}
}

func TestIDsAreTwentySixCharactersAfterThePrefix(t *testing.T) {
	got := New(PrefixUser)
	if !strings.HasPrefix(got, "usr_") {
		t.Errorf("missing prefix: %q", got)
	}
	if body := strings.TrimPrefix(got, "usr_"); len(body) != 26 {
		t.Errorf("body is %d chars, want 26: %q", len(body), body)
	}
}

func TestAlphabetExcludesTheConfusableLetters(t *testing.T) {
	// I, L, O and U are absent from Crockford base32 on purpose: I/1, O/0 and
	// L/1 are misread constantly, and excluding U keeps accidental profanity
	// out of generated ids.
	if len(alphabet) != 32 {
		t.Fatalf("alphabet has %d symbols, want 32", len(alphabet))
	}
	for _, c := range "ILOU" {
		if strings.ContainsRune(alphabet, c) {
			t.Errorf("alphabet contains %q, which is confusable", c)
		}
	}
	seen := map[rune]bool{}
	for _, c := range alphabet {
		if seen[c] {
			t.Errorf("duplicate symbol %q in alphabet", c)
		}
		seen[c] = true
	}
}

func TestIDsSortChronologicallyAsPlainStrings(t *testing.T) {
	// The property the whole format exists for: `ORDER BY id` is `ORDER BY
	// created_at` for free, and index writes append rather than scattering.
	first := newULID(time.UnixMilli(1_700_000_000_000))
	later := newULID(time.UnixMilli(1_700_000_001_000))
	muchLater := newULID(time.UnixMilli(1_900_000_000_000))

	if !(first < later) {
		t.Errorf("ids do not sort by time: %q should precede %q", first, later)
	}
	if !(later < muchLater) {
		t.Errorf("ids do not sort by time: %q should precede %q", later, muchLater)
	}
}

func TestIDsAreUnique(t *testing.T) {
	const n = 10_000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		got := New(PrefixUser)
		if seen[got] {
			t.Fatalf("collision after %d ids: %q", i, got)
		}
		seen[got] = true
	}
}

func TestTimeOfRecoversTheGenerationTime(t *testing.T) {
	want := time.UnixMilli(1_700_000_000_000).UTC()
	s := "usr_" + newULID(want)

	got, ok := TimeOf(s)
	if !ok {
		t.Fatalf("could not parse %q", s)
	}
	if !got.Equal(want) {
		t.Errorf("TimeOf = %v, want %v", got, want)
	}
}

func TestValidRejectsWhatItShould(t *testing.T) {
	good := New(PrefixUser)
	if !Valid(PrefixUser, good) {
		t.Errorf("rejected a valid id: %q", good)
	}

	for _, c := range []struct {
		name, in string
	}{
		{"wrong prefix", "scn_" + strings.TrimPrefix(good, "usr_")},
		{"no prefix", strings.TrimPrefix(good, "usr_")},
		{"too short", "usr_01J8XK"},
		{"too long", good + "X"},
		{"confusable letter I", "usr_" + strings.Repeat("I", 26)},
		{"lowercase body", strings.ToLower(good)},
		{"empty", ""},
		{"prefix only", "usr_"},
		{"sql-ish", "usr_'; DROP TABLE users;--"},
	} {
		if Valid(PrefixUser, c.in) {
			t.Errorf("%s: accepted %q", c.name, c.in)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
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
