package jj

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// SyncState describes how a local bookmark relates to a remote copy.
type SyncState int

const (
	SyncUnknown    SyncState = iota
	SyncInSync               // local and remote point to same commit
	SyncAhead                // local has commits not on remote (remote is behind)
	SyncBehind               // remote has commits not on local (remote is ahead)
	SyncDiverged             // both sides have unique commits
	SyncLocalOnly            // bookmark exists locally only
	SyncRemoteOnly           // bookmark exists on remote only
)

func (s SyncState) String() string {
	switch s {
	case SyncInSync:
		return "in-sync"
	case SyncAhead:
		return "ahead"
	case SyncBehind:
		return "behind"
	case SyncDiverged:
		return "diverged"
	case SyncLocalOnly:
		return "local-only"
	case SyncRemoteOnly:
		return "remote-only"
	default:
		return "unknown"
	}
}

// RemoteBookmarkState holds a bookmark's state on a specific remote.
type RemoteBookmarkState struct {
	Target  string // commit ID on remote
	Tracked bool   // whether this remote ref is tracked by jj
	Synced  bool   // local and remote agree (from jj's synced() template)
}

// BookmarkInfo holds the full state of a named bookmark across local and remotes.
type BookmarkInfo struct {
	Name     string                         // bookmark name
	Target   string                         // local commit ID ("" if remote-only or conflicted)
	ChangeID string                         // local change ID ("" if remote-only or conflicted)
	Present  bool                           // has local target
	Conflict bool                           // bookmark is in conflicted state (multiple targets)
	Remotes  map[string]RemoteBookmarkState // remote name → state
}

// SyncWith returns the sync state of this bookmark relative to the given remote.
//
// Push-safety determination follows jj's model: !conflict && !synced means the
// bookmark can be pushed. When conflict is false and not synced, the local side
// is authoritative — jj auto-fast-forwards on fetch, so a non-conflicting
// difference means the local was deliberately moved (rebase, amend, etc.).
func (b *BookmarkInfo) SyncWith(remote string) SyncState {
	if b.Conflict {
		return SyncDiverged
	}
	rs, ok := b.Remotes[remote]
	if !ok {
		if b.Present {
			return SyncLocalOnly
		}
		return SyncUnknown
	}
	if !b.Present {
		return SyncRemoteOnly
	}
	if rs.Synced {
		return SyncInSync
	}
	// Not synced, no conflict — local is authoritative. Pushable.
	return SyncAhead
}

// rawBookmarkEntry is the JSON structure from jj bookmark list template output.
type rawBookmarkEntry struct {
	Name     string  `json:"name"`
	Remote   *string `json:"remote"` // null for local entries
	Present  bool    `json:"present"`
	Conflict bool    `json:"conflict"`
	Target   string  `json:"target"`
	ChangeID string  `json:"change_id"`
	Tracked  bool    `json:"tracked"`
	Synced   bool    `json:"synced"`
}

// ParseBookmarkList parses JSONL output from jj bookmark list --all-remotes
// into grouped BookmarkInfo entries. The internal "git" remote is filtered out.
func ParseBookmarkList(data []byte) ([]BookmarkInfo, error) {
	var entries []rawBookmarkEntry
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var e rawBookmarkEntry
		if err := dec.Decode(&e); err != nil {
			return nil, fmt.Errorf("parsing bookmark entry: %w", err)
		}
		entries = append(entries, e)
	}

	// Group by bookmark name, preserving order of first occurrence.
	type bookmarkState struct {
		info  BookmarkInfo
		order int
	}
	byName := make(map[string]*bookmarkState)
	orderCounter := 0

	for _, e := range entries {
		// Skip jj's internal git remote.
		if e.Remote != nil && *e.Remote == "git" {
			continue
		}

		bs, exists := byName[e.Name]
		if !exists {
			bs = &bookmarkState{
				info: BookmarkInfo{
					Name:    e.Name,
					Remotes: make(map[string]RemoteBookmarkState),
				},
				order: orderCounter,
			}
			orderCounter++
			byName[e.Name] = bs
		}

		if e.Remote == nil {
			// Local entry.
			bs.info.Present = e.Present
			bs.info.Conflict = e.Conflict
			bs.info.Target = e.Target
			bs.info.ChangeID = e.ChangeID
		} else {
			// Remote entry.
			bs.info.Remotes[*e.Remote] = RemoteBookmarkState{
				Target:  e.Target,
				Tracked: e.Tracked,
				Synced:  e.Synced,
			}
			// If no local entry exists, mark as remote-only with remote's change info.
			if !bs.info.Present && bs.info.Target == "" {
				bs.info.Target = ""
				bs.info.ChangeID = ""
			}
		}
	}

	// Build result sorted by first-occurrence order.
	result := make([]BookmarkInfo, len(byName))
	for _, bs := range byName {
		result[bs.order] = bs.info
	}
	return result, nil
}

// MatchBookmarksToChanges returns a map from change ID to bookmarks that point
// to that change. Matching is done via commit ID (local target).
func MatchBookmarksToChanges(dag *ChangeDAG, bookmarks []BookmarkInfo) map[string][]*BookmarkInfo {
	// Build commit ID → change ID lookup from the DAG.
	commitToChange := make(map[string]string, len(dag.Changes))
	for _, c := range dag.Changes {
		commitToChange[c.CommitID] = c.ChangeID
	}

	result := make(map[string][]*BookmarkInfo)
	for i := range bookmarks {
		b := &bookmarks[i]
		if !b.Present || b.Target == "" {
			continue
		}
		if changeID, ok := commitToChange[b.Target]; ok {
			result[changeID] = append(result[changeID], b)
		}
	}
	return result
}

// ChangeBookmark represents the bookmark assignment for a change.
type ChangeBookmark struct {
	ChangeID  string
	Bookmark  string
	IsNew     bool      // bookmark must be created (not pre-existing)
	SyncState SyncState // sync state relative to the push remote
	Conflict  bool      // bookmark has conflicting targets (true divergence)
	Displaced bool      // bookmark exists but no longer points to this change's commit
	Others    []string  // other usable bookmarks on the change that were not chosen, best first
}

// sortByPriority orders bookmarks best first: higher priority, then
// case-insensitive name, then exact name.
func sortByPriority(bs []*BookmarkInfo, priority func(string) int) {
	prio := func(b *BookmarkInfo) int {
		if priority == nil {
			return 0
		}
		return priority(b.Name)
	}
	slices.SortStableFunc(bs, func(x, y *BookmarkInfo) int {
		if c := cmp.Compare(prio(y), prio(x)); c != 0 {
			return c
		}
		if c := strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name)); c != 0 {
			return c
		}
		return strings.Compare(x.Name, y.Name)
	})
}

// PlanBookmarks assigns bookmarks without changing the repository. A tie at
// the highest priority is an error. All assignment errors are reported together.
//
// names maps a commit ID to the bookmark name generated for it (see
// Runner.BookmarkNames). It is consulted only for changes without a usable
// existing bookmark; a missing entry for such a change is an error.
func PlanBookmarks(
	dag *ChangeDAG,
	bookmarks []BookmarkInfo,
	names map[string]string,
	pushRemote string,
	shouldUseExisting func(changeID, bookmark string) bool,
	priority func(bookmark string) int,
	createNew bool,
	requireExisting bool,
) ([]ChangeBookmark, error) {
	matched := MatchBookmarksToChanges(dag, bookmarks)

	// Build name lookup for detecting bookmarks that exist but point to a
	// different commit (e.g. after a fetch fast-forwarded the local bookmark).
	bookmarkByName := make(map[string]*BookmarkInfo, len(bookmarks))
	for i := range bookmarks {
		bookmarkByName[bookmarks[i].Name] = &bookmarks[i]
	}

	var result []ChangeBookmark
	var problems []string
	generatedBy := make(map[string]string) // generated name → change ID
	for _, change := range dag.Changes {
		existing := matched[change.ChangeID]

		// Pick a usable existing bookmark.
		var usable []*BookmarkInfo
		for _, b := range existing {
			if shouldUseExisting == nil || shouldUseExisting(change.ChangeID, b.Name) {
				usable = append(usable, b)
			}
		}
		sortByPriority(usable, priority)
		if len(usable) > 1 && (priority == nil || priority(usable[0].Name) == priority(usable[1].Name)) {
			var names []string
			for _, b := range usable {
				names = append(names, b.Name)
			}
			problems = append(problems, fmt.Sprintf("%.12s (%s): ambiguous bookmarks: %s; select one with `jip send <bookmark>`", change.ChangeID, change.Title(), strings.Join(names, ", ")))
			continue
		}
		var chosen *BookmarkInfo
		var others []string
		if len(usable) > 0 {
			chosen = usable[0]
			for _, b := range usable[1:] {
				others = append(others, b.Name)
			}
		}

		if chosen != nil {
			result = append(result, ChangeBookmark{
				ChangeID:  change.ChangeID,
				Bookmark:  chosen.Name,
				IsNew:     false,
				SyncState: chosen.SyncWith(pushRemote),
				Conflict:  chosen.Conflict,
				Others:    others,
			})
			continue
		}

		// No existing bookmark matched by commit ID. Use the generated name and
		// check if a bookmark with that name already exists (can happen when
		// a fetch fast-forwarded the bookmark to a remote commit).
		name, ok := names[change.CommitID]
		if !ok {
			problems = append(problems, fmt.Sprintf("%.12s (%s): no generated bookmark name", change.ChangeID, change.Title()))
			continue
		}
		if other, dup := generatedBy[name]; dup && other != change.ChangeID {
			problems = append(problems, fmt.Sprintf("%.12s (%s): generated bookmark %q is also generated for %.12s; make bookmark-template produce unique names", change.ChangeID, change.Title(), name, other))
			continue
		}
		generatedBy[name] = change.ChangeID

		if bi, exists := bookmarkByName[name]; exists {
			if shouldUseExisting != nil && !shouldUseExisting(change.ChangeID, name) {
				if !createNew {
					continue
				}
				problems = append(problems, fmt.Sprintf("%.12s (%s): generated bookmark %q is excluded; create an eligible bookmark", change.ChangeID, change.Title(), name))
				continue
			}
			// Bookmark exists but points to a different commit than our change.
			// This typically means a fetch fast-forwarded it to a remote commit.
			result = append(result, ChangeBookmark{
				ChangeID:  change.ChangeID,
				Bookmark:  name,
				IsNew:     false,
				SyncState: bi.SyncWith(pushRemote),
				Conflict:  bi.Conflict,
				Displaced: true,
			})
			continue
		}

		if !createNew {
			continue
		}

		if requireExisting {
			problems = append(problems, fmt.Sprintf("%.12s (%s): no eligible bookmark; create one with `jj bookmark set <name> -r %s`", change.ChangeID, change.Title(), change.ChangeID))
			continue
		}
		result = append(result, ChangeBookmark{
			ChangeID:  change.ChangeID,
			Bookmark:  name,
			IsNew:     true,
			SyncState: SyncLocalOnly,
		})
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("cannot assign bookmarks:\n  %s", strings.Join(problems, "\n  "))
	}
	return result, nil
}

// EnsureBookmarks is the plan-and-apply entry point for callers that do not
// need a separate preflight phase.
func EnsureBookmarks(runner Runner, dag *ChangeDAG, bookmarks []BookmarkInfo, names map[string]string, pushRemote string, shouldUseExisting func(string, string) bool, priority func(string) int, createNew bool) ([]ChangeBookmark, error) {
	plan, err := PlanBookmarks(dag, bookmarks, names, pushRemote, shouldUseExisting, priority, createNew, false)
	if err != nil {
		return nil, err
	}
	for _, b := range plan {
		if b.IsNew {
			if err := runner.BookmarkSet(b.Bookmark, b.ChangeID); err != nil {
				return nil, fmt.Errorf("creating bookmark for %s: %w", b.ChangeID, err)
			}
		}
	}
	return plan, nil
}

// DefaultBookmarkTemplate is the jj commit template used to generate names
// for changes without a bookmark: jip/<description-slug>/<short-change-id>.
//
// The slug strips a conventional-commit prefix ("feat(x)!: "), lowercases,
// collapses runs of non-[a-z0-9] characters into "-", trims "-" from both
// ends, and truncates to 30 characters, backing up to the last "-" when it
// lies past the 15th character. An empty slug becomes "change". The "~"
// marks a truncated slug so the word-boundary step only applies then; it
// cannot otherwise occur because only [a-z0-9-] remains at that point.
// U+0130 (İ) is mapped to "i" first: jj lowercases it to "i" plus a combining
// dot, whereas the original Go implementation produced a plain "i".
const DefaultBookmarkTemplate = `"jip/" ++ coalesce(` +
	`description` +
	`.replace(regex:'^[a-zA-Z]+(\([^)]*\))?!?:\s*', "", 1)` +
	`.replace("İ", "i")` +
	`.lower()` +
	`.replace(regex:'[^a-z0-9]+', "-")` +
	`.replace(regex:'^-+|-+$', "")` +
	`.replace(regex:'^(.{30}).+$', "$1~")` +
	`.replace(regex:'^(.{16,})-[^-]*~$', "$1")` +
	`.remove_suffix("~"), ` +
	`"change") ++ "/" ++ change_id.short(8)`
