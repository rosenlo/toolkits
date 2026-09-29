package eventtime

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This package was lifted out of a private codebase, so its sources are checked
// for what such a codebase leaves behind in comments: dates of the incidents a
// rule came from, issue numbers, addresses. A rule's reason belongs in the
// comment; where it was learned does not.
var leftovers = map[string]*regexp.Regexp{
	"an ISO date":            regexp.MustCompile(`\b20\d\d-\d\d-\d\d\b`),
	"an issue or PR number":  regexp.MustCompile(`#\d{2,}\b`),
	"an IPv4 address":        regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`),
	"a URL":                  regexp.MustCompile(`https?://`),
	"a hostname or a domain": regexp.MustCompile(`\b[a-z0-9-]+\.(com|net|io|org|internal|local|svc)\b`),
}

func sources(t *testing.T) map[string]string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no sources: %v", err)
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

// isImport reports an import path, the one place a domain belongs.
func isImport(line string) bool {
	l := strings.TrimSpace(line)
	return strings.HasPrefix(l, `"`) && strings.HasSuffix(l, `"`) || strings.HasPrefix(l, "import ")
}

func TestTheSourcesCarryNoLeftovers(t *testing.T) {
	for file, src := range sources(t) {
		for i, line := range strings.Split(src, "\n") {
			for what, re := range leftovers {
				if isImport(line) || strings.Contains(line, "regexp.MustCompile") {
					continue
				}
				if m := re.FindString(line); m != "" {
					t.Errorf("%s:%d carries %s: %q", file, i+1, what, m)
				}
			}
		}
	}
}

// TOOLKITS_PRIVATE_WORDS names a file, kept outside this repository, of words
// that must not appear here, one per line, matched case-insensitively as a
// word: a word that ends in a letter or digit must not run on into another
// one. The list is private because publishing it would publish the words.
func TestTheSourcesCarryNoPrivateWords(t *testing.T) {
	path := os.Getenv("TOOLKITS_PRIVATE_WORDS")
	if path == "" {
		t.Skip("TOOLKITS_PRIVATE_WORDS not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var words []*regexp.Regexp
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		w := strings.ToLower(strings.TrimSpace(sc.Text()))
		if w == "" || strings.HasPrefix(w, "//") {
			continue
		}
		pattern := `(^|[^a-z0-9])` + regexp.QuoteMeta(w)
		if last := w[len(w)-1]; last >= 'a' && last <= 'z' || last >= '0' && last <= '9' {
			pattern += `($|[^a-z0-9])`
		}
		words = append(words, regexp.MustCompile(pattern))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(words) == 0 {
		t.Fatalf("%s holds no words", path)
	}
	for file, src := range sources(t) {
		for i, line := range strings.Split(strings.ToLower(src), "\n") {
			for _, w := range words {
				if w.MatchString(line) {
					t.Errorf("%s:%d carries a private word", file, i+1)
				}
			}
		}
	}
}
