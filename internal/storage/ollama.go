// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage // import "miniflux.app/v2/internal/storage"

import (
	"fmt"

	"miniflux.app/v2/internal/model"

	"github.com/lib/pq"
)

// UpdateEntryOllamaEnrichment writes the score and tags computed by the Ollama
// agent for a single entry. It also stamps ollama_enriched_at with now().
func (s *Storage) UpdateEntryOllamaEnrichment(entryID int64, score float64, tags []string) error {
	query := `
		UPDATE entries
		SET ollama_score = $1,
			ollama_tags = $2,
			ollama_enriched_at = now()
		WHERE id = $3
	`
	if _, err := s.db.Exec(query, score, pq.Array(tags), entryID); err != nil {
		return fmt.Errorf(`store: unable to update ollama enrichment for entry #%d: %v`, entryID, err)
	}
	return nil
}

// MarkEntryAsFiltered flags an entry that the Ollama scorer rated below the
// configured threshold. The entry is kept in the database so users can review
// the filtering decisions, but it is forced to "read" so it does not pollute
// unread lists. The default entry queries hide ollama_filtered_at IS NOT NULL
// rows, so filtered entries only surface on the dedicated review page.
func (s *Storage) MarkEntryAsFiltered(entryID int64) error {
	query := `
		UPDATE entries
		SET ollama_filtered_at = now(),
		    status = 'read',
		    changed_at = now()
		WHERE id = $1
		  AND ollama_filtered_at IS NULL
	`
	if _, err := s.db.Exec(query, entryID); err != nil {
		return fmt.Errorf(`store: unable to mark entry #%d as filtered: %v`, entryID, err)
	}
	return nil
}

// RestoreFilteredEntry clears the Ollama filter flag on an entry, putting it
// back to unread so the user can read or star it. Scoped to the user to
// prevent IDOR.
func (s *Storage) RestoreFilteredEntry(userID, entryID int64) error {
	query := `
		UPDATE entries
		SET ollama_filtered_at = NULL,
		    status = 'unread',
		    changed_at = now()
		WHERE id = $1
		  AND user_id = $2
		  AND ollama_filtered_at IS NOT NULL
	`
	if _, err := s.db.Exec(query, entryID, userID); err != nil {
		return fmt.Errorf(`store: unable to restore filtered entry #%d: %v`, entryID, err)
	}
	return nil
}

// CountOllamaFilteredEntries returns the number of entries currently filtered
// out by the Ollama scorer for the given user.
func (s *Storage) CountOllamaFilteredEntries(userID int64) (int, error) {
	var count int
	query := `
		SELECT count(*)
		FROM entries
		WHERE user_id = $1 AND ollama_filtered_at IS NOT NULL
	`
	if err := s.db.QueryRow(query, userID).Scan(&count); err != nil {
		return 0, fmt.Errorf(`store: unable to count filtered entries: %v`, err)
	}
	return count, nil
}

// CountOllamaFiltered is a layout-friendly wrapper around
// CountOllamaFilteredEntries that swallows the error and returns 0 instead of
// failing the whole page. Used by the menu counter.
func (s *Storage) CountOllamaFiltered(userID int64) int {
	n, err := s.CountOllamaFilteredEntries(userID)
	if err != nil {
		return 0
	}
	return n
}

// CountUserRatedEntries returns the number of entries that carry an
// appreciation signal for the given user (starred or read). It is used as the
// proxy for "is the recommendation base large enough to start filtering?".
func (s *Storage) CountUserRatedEntries(userID int64) (int, error) {
	var count int
	query := `
		SELECT count(*)
		FROM entries
		WHERE user_id = $1
		  AND (starred = true OR status = 'read')
	`
	if err := s.db.QueryRow(query, userID).Scan(&count); err != nil {
		return 0, fmt.Errorf(`store: unable to count rated entries: %v`, err)
	}
	return count, nil
}

// OllamaProfileSample is the compact representation of a past entry used to
// build the user's preference profile sent to the Ollama scorer.
type OllamaProfileSample struct {
	Title   string
	Tags    []string
	Starred bool
	Read    bool
}

// GetOllamaUserProfile returns up to limit recent entries the user has reacted
// to (starred or read), most recent first, projected to title + ollama_tags.
// Tags fall back to the feed-provided tags when the entry has not been
// enriched yet.
func (s *Storage) GetOllamaUserProfile(userID int64, limit int) ([]OllamaProfileSample, error) {
	if limit <= 0 {
		return nil, nil
	}
	query := `
		SELECT
			title,
			COALESCE(NULLIF(ollama_tags, '{}'), tags) AS effective_tags,
			starred,
			status = 'read' AS is_read
		FROM entries
		WHERE user_id = $1
		  AND (starred = true OR status = 'read')
		ORDER BY changed_at DESC
		LIMIT $2
	`
	rows, err := s.db.Query(query, userID, limit)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to fetch ollama profile samples: %v`, err)
	}
	defer rows.Close()

	samples := make([]OllamaProfileSample, 0, limit)
	for rows.Next() {
		var sample OllamaProfileSample
		if err := rows.Scan(&sample.Title, pq.Array(&sample.Tags), &sample.Starred, &sample.Read); err != nil {
			return nil, fmt.Errorf(`store: unable to scan ollama profile sample: %v`, err)
		}
		samples = append(samples, sample)
	}
	return samples, nil
}

// SetOllamaFeedback applies an explicit user feedback (+1 = boost,
// -1 = penalize, 0 = clear) on a single entry. As a side-effect:
//   - +1 forces the score up to at least 0.95 and clears any active filter,
//     putting the entry back to "unread" so the user can find it again.
//   - -1 forces the score down to at most 0.05 and applies the filter so
//     the entry leaves the regular reading lists.
//   - 0 leaves the score and filter state untouched.
//
// The ollama_enriched_at timestamp is also stamped so the regular worker
// treats the entry as already processed and the manual feedback survives
// future refreshes (the worker also short-circuits on non-zero feedback).
// Returns the resulting feedback value (which may equal value or 0 if the
// caller passed value to toggle it off).
func (s *Storage) SetOllamaFeedback(userID, entryID int64, value int) (int, error) {
	if value < -1 || value > 1 {
		return 0, fmt.Errorf(`store: invalid ollama feedback %d`, value)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf(`store: unable to begin feedback tx: %v`, err)
	}
	defer tx.Rollback()

	var current int
	if err := tx.QueryRow(
		`SELECT ollama_feedback FROM entries WHERE id = $1 AND user_id = $2`,
		entryID, userID,
	).Scan(&current); err != nil {
		return 0, fmt.Errorf(`store: unable to read feedback for entry #%d: %v`, entryID, err)
	}

	// If the user clicked the same button twice, treat it as an undo.
	target := value
	if current == value && value != 0 {
		target = 0
	}

	switch target {
	case 1:
		if _, err := tx.Exec(`
			UPDATE entries
			SET ollama_feedback = 1,
			    ollama_score = GREATEST(COALESCE(ollama_score, 0), 0.95),
			    ollama_enriched_at = COALESCE(ollama_enriched_at, now()),
			    ollama_filtered_at = NULL,
			    status = CASE WHEN ollama_filtered_at IS NOT NULL THEN 'unread' ELSE status END,
			    changed_at = now()
			WHERE id = $1 AND user_id = $2
		`, entryID, userID); err != nil {
			return 0, fmt.Errorf(`store: unable to apply +1 feedback on entry #%d: %v`, entryID, err)
		}
	case -1:
		if _, err := tx.Exec(`
			UPDATE entries
			SET ollama_feedback = -1,
			    ollama_score = LEAST(COALESCE(ollama_score, 1), 0.05),
			    ollama_enriched_at = COALESCE(ollama_enriched_at, now()),
			    ollama_filtered_at = COALESCE(ollama_filtered_at, now()),
			    status = 'read',
			    changed_at = now()
			WHERE id = $1 AND user_id = $2
		`, entryID, userID); err != nil {
			return 0, fmt.Errorf(`store: unable to apply -1 feedback on entry #%d: %v`, entryID, err)
		}
	case 0:
		// Clearing feedback leaves the score and filter as they are: the user
		// only walks back the explicit thumb, not the broader filtering logic.
		if _, err := tx.Exec(`
			UPDATE entries
			SET ollama_feedback = 0,
			    changed_at = now()
			WHERE id = $1 AND user_id = $2
		`, entryID, userID); err != nil {
			return 0, fmt.Errorf(`store: unable to clear feedback on entry #%d: %v`, entryID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf(`store: unable to commit feedback tx: %v`, err)
	}
	return target, nil
}

// CountEntriesPendingOllamaEnrichment counts the user's entries that still
// have no Ollama enrichment recorded. Used to label the manual backfill
// button on the filtered-entries page.
func (s *Storage) CountEntriesPendingOllamaEnrichment(userID int64) (int, error) {
	var count int
	query := `
		SELECT count(*)
		FROM entries
		WHERE user_id = $1
		  AND ollama_enriched_at IS NULL
	`
	if err := s.db.QueryRow(query, userID).Scan(&count); err != nil {
		return 0, fmt.Errorf(`store: unable to count pending ollama entries: %v`, err)
	}
	return count, nil
}

// GetEntriesForOllamaBackfill returns the next batch of entries waiting for
// Ollama enrichment, newest first so backfilled scores are most relevant for
// the recent reading flow. Filtered-out entries are deliberately *not*
// excluded — although they already carry a score, the worker is idempotent
// and skipping them at the SQL level would complicate the query for no gain.
func (s *Storage) GetEntriesForOllamaBackfill(userID int64, limit int) (model.Entries, error) {
	if limit <= 0 {
		return nil, nil
	}
	query := `
		SELECT id, user_id, feed_id, title, url, content
		FROM entries
		WHERE user_id = $1
		  AND ollama_enriched_at IS NULL
		ORDER BY changed_at DESC
		LIMIT $2
	`
	rows, err := s.db.Query(query, userID, limit)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to fetch entries pending ollama enrichment: %v`, err)
	}
	defer rows.Close()

	entries := make(model.Entries, 0, limit)
	for rows.Next() {
		entry := model.NewEntry()
		if err := rows.Scan(&entry.ID, &entry.UserID, &entry.FeedID, &entry.Title, &entry.URL, &entry.Content); err != nil {
			return nil, fmt.Errorf(`store: unable to scan ollama backfill row: %v`, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// GetEntryForOllama returns the minimal data the enrichment worker needs to
// compute tags and score for a freshly inserted entry.
func (s *Storage) GetEntryForOllama(entryID int64) (*model.Entry, error) {
	entry := model.NewEntry()
	query := `
		SELECT id, user_id, feed_id, title, url, content
		FROM entries
		WHERE id = $1
	`
	err := s.db.QueryRow(query, entryID).Scan(
		&entry.ID,
		&entry.UserID,
		&entry.FeedID,
		&entry.Title,
		&entry.URL,
		&entry.Content,
	)
	if err != nil {
		return nil, fmt.Errorf(`store: unable to fetch entry #%d for ollama: %v`, entryID, err)
	}
	return entry, nil
}
