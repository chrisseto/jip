//go:build integration

package jj

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// legacyBookmarkName is the Go implementation that DefaultBookmarkTemplate
// replaced. It is kept as a test oracle so the template stays
// backward compatible: existing users must keep getting the same names.
func legacyBookmarkName(description, shortChangeID string) string {
	s := regexp.MustCompile(`^[a-zA-Z]+(\([^)]*\))?!?:\s*`).ReplaceAllString(description, "")
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	const maxSlugLen = 30
	if len(s) > maxSlugLen {
		s = s[:maxSlugLen]
		if i := strings.LastIndex(s, "-"); i > maxSlugLen/2 {
			s = s[:i]
		}
	}
	if s == "" {
		s = "change"
	}
	return fmt.Sprintf("jip/%s/%s", s, shortChangeID)
}

func TestIntegration_DefaultBookmarkTemplateMatchesLegacy(t *testing.T) {
	dir := initJJRepo(t)
	runner := NewRunner(dir)

	descriptions := []string{
		// Former slugify/GenerateBookmarkName unit-test cases.
		"feat: add auth module",
		"fix: handle nil pointer",
		"refactor(auth): extract token",
		"docs: update README",
		"feat!: breaking change",
		"hello world",
		"Hello  World!",
		"one/two/three",
		"CamelCase",
		"dots.and.dashes-here",
		"  leading trailing  ",
		"this is a very long commit description that should be truncated at a reasonable length",
		"",
		// Edge cases around prefixes, truncation and non-ASCII input.
		"feat(api)!: Add the Über cool thing, with extra-long words here\n\nBody text.",
		"fix: !!!",
		"Refactor:   Thing: other",
		"abcdefghijklmnopqrstuvwxyz0123456789",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbb",
		"aaaaa-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"exactly thirty characters ok!!",
		"exactly-thirty-characters-okay",
		"exactly-thirty-characters-okay-",
		"日本語 title here",
		"\u212Aelvin \u0130stanbul",
		"tilde~in~title",
		"$1 dollar $2 signs",
	}

	for _, desc := range descriptions {
		t.Run(desc, func(t *testing.T) {
			jjRun(t, dir, "new", "-m", desc)
			changeID := getChangeID(t, dir, "@")
			commitID := getCommitID(t, dir, "@")

			// jj stores descriptions with a trailing newline; the Go code
			// received the same value from jj log.
			stored := desc
			if stored != "" && !strings.HasSuffix(stored, "\n") {
				stored += "\n"
			}
			want := legacyBookmarkName(stored, changeID[:8])

			names, err := runner.BookmarkNames([]string{commitID}, DefaultBookmarkTemplate)
			if err != nil {
				t.Fatalf("BookmarkNames: %v", err)
			}
			if got := names[commitID]; got != want {
				t.Errorf("template = %q, legacy = %q", got, want)
			}
		})
	}
}

func TestIntegration_BookmarkNamesBatchAndErrors(t *testing.T) {
	dir := initJJRepo(t)
	runner := NewRunner(dir)

	writeAndCommit(t, dir, "a.txt", "a", "first")
	writeAndCommit(t, dir, "b.txt", "b", "second")
	a, b := getCommitID(t, dir, "@--"), getCommitID(t, dir, "@-")

	names, err := runner.BookmarkNames([]string{a, b}, `"u/" ++ description.first_line()`)
	if err != nil {
		t.Fatalf("BookmarkNames: %v", err)
	}
	if names[a] != "u/first" || names[b] != "u/second" {
		t.Fatalf("got %v", names)
	}

	if _, err := runner.BookmarkNames([]string{a}, `not_a_keyword`); err == nil || !strings.Contains(err.Error(), "evaluating bookmark-template") {
		t.Fatalf("expected template parse error, got %v", err)
	}
	if _, err := runner.BookmarkNames([]string{a}, `description.first_line() ++ " x"`); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("expected whitespace rejection, got %v", err)
	}
	if _, err := runner.BookmarkNames([]string{a}, `""`); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-name rejection, got %v", err)
	}
}
